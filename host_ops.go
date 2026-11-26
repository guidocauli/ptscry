//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// host_ops.go: the host side of rename, notify, send-keys and the layouts.
// Everything runs as the user (the host is the user's own process); the
// layouts are files in the user's state directory, never touched by root.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// opsDispatch serves the small session commands; false = not one of them.
func (h *host) opsDispatch(fc *framed, hl *hello, peer peerInfo) bool {
	switch hl.Op {
	case "rename":
		s, err := h.rename(hl.Name, hl.NewName)
		if err != nil {
			replyErr(fc, err.Error())
			return true
		}
		if hl.Name != hl.NewName {
			h.audit.log("rename", peer.fields(fields{"session": hl.NewName, "from": hl.Name}))
			s.tellName(s.Name()) // outside h.mu: a slow client may hold it for a while
		}
		_ = replyOK(fc, okReply{Message: "renamed " + hl.Name + " to " + s.Name()})

	case "notify":
		s := h.get(hl.Name)
		if s == nil {
			replyErr(fc, fmt.Sprintf("no such session: %q", hl.Name))
			return true
		}
		text := cleanNotice(hl.Text)
		if text == "" {
			replyErr(fc, "empty notice")
			return true
		}
		n, err := s.notice(text)
		if err != nil {
			replyErr(fc, err.Error())
			return true
		}
		h.audit.log("notify", peer.fields(fields{"session": s.Name(), "bytes": len(text), "clients": n}))
		_ = replyOK(fc, okReply{Message: fmt.Sprintf("shown to %d client(s)", n)})

	case "send-keys":
		s := h.get(hl.Name)
		if s == nil {
			replyErr(fc, fmt.Sprintf("no such session: %q", hl.Name))
			return true
		}
		if len(hl.Text) > maxSendKeys {
			replyErr(fc, "too much to send at once (max 4096 bytes)")
			return true
		}
		if err := s.sendKeys([]byte(hl.Text)); err != nil {
			replyErr(fc, err.Error())
			return true
		}
		// the length only: what is typed may be a password
		h.audit.log("send_keys", peer.fields(fields{"session": s.Name(), "bytes": len(hl.Text)}))
		_ = replyOK(fc, okReply{Message: "sent"})

	case "layout-set":
		if err := h.saveLayout(hl.Name, hl.Text); err != nil {
			replyErr(fc, err.Error())
			return true
		}
		h.audit.log("layout_set", peer.fields(fields{"layout": hl.Name, "bytes": len(hl.Text)}))
		_ = replyOK(fc, okReply{Message: "layout " + hl.Name + " saved"})

	case "layout-get":
		text, err := h.loadLayout(hl.Name)
		if err != nil {
			replyErr(fc, err.Error())
			return true
		}
		_ = replyOK(fc, okReply{Text: text})

	case "layout-list":
		names, err := h.listLayouts()
		if err != nil {
			replyErr(fc, err.Error())
			return true
		}
		_ = replyOK(fc, okReply{Names: names})

	case "layout-del":
		if err := h.deleteLayout(hl.Name); err != nil {
			replyErr(fc, err.Error())
			return true
		}
		h.audit.log("layout_del", peer.fields(fields{"layout": hl.Name}))
		_ = replyOK(fc, okReply{Message: "layout " + hl.Name + " removed"})

	default:
		return false
	}
	return true
}

// ------------------------------------------------------------ notify

// noticeBurst notices per noticeWindow per session are shown; more are refused
// (a loop in a script must not bury the terminal).
const (
	noticeBurst  = 10
	noticeWindow = 10 * time.Second
)

// notice shows a text to every client attached to the session.
func (s *session) notice(text string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	kept := s.notices[:0]
	for _, t := range s.notices {
		if now.Sub(t) < noticeWindow {
			kept = append(kept, t)
		}
	}
	s.notices = kept
	if len(s.notices) >= noticeBurst {
		return 0, errors.New("too many notices, retry in a few seconds")
	}
	s.notices = append(s.notices, now)
	var flags byte
	if s.alt {
		flags |= noticeAlt
	}
	msg := append([]byte{flags}, text...)
	n := 0
	for c := range s.clients {
		if c.push(outMsg{msgNotice, msg}) {
			n++
		} else {
			delete(s.clients, c)
		}
	}
	return n, nil
}

// tellName lets the attached clients know the new name of the session.
func (s *session) tellName(n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		if !c.push(outMsg{msgName, []byte(n)}) {
			delete(s.clients, c)
		}
	}
}

// ------------------------------------------------------------ send-keys

// sendKeys types into the session. The write happens aside: a program that
// does not read its input would otherwise hold the request (and the
// connection) forever; while one write is pending the next is refused.
func (s *session) sendKeys(p []byte) error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("session %q has ended", s.Name())
	}
	if s.sending {
		s.mu.Unlock()
		return errors.New("the session is not reading its input (a previous send-keys is still waiting)")
	}
	s.sending = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.write(p)
		s.mu.Lock()
		s.sending = false
		s.mu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// still queued: it is delivered when the program reads
	}
	return nil
}

// ------------------------------------------------------------ layouts

var layoutMu sync.Mutex

func (h *host) layoutDir() string {
	return filepath.Join(h.home, ".local", "state", "ptscry", "layouts")
}

func (h *host) layoutPath(name string) (string, error) {
	if !validLayoutName(name) {
		return "", errors.New("invalid layout name (letters, digits, . _ -; not starting with a dot)")
	}
	return filepath.Join(h.layoutDir(), name+".json"), nil
}

// ensureLayoutDir creates the directory (0700) and refuses one that is a
// symlink, is not a directory or is not the user's.
func (h *host) ensureLayoutDir() (string, error) {
	dir := h.layoutDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !ok || int(st.Uid) != h.uid {
		return "", fmt.Errorf("%s is not a directory of yours", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, 0o700)
	}
	return dir, nil
}

func (h *host) saveLayout(name, text string) error {
	layoutMu.Lock() // the count below, then the write: one save at a time
	defer layoutMu.Unlock()
	if err := checkLayout(text); err != nil {
		return err
	}
	p, err := h.layoutPath(name)
	if err != nil {
		return err
	}
	dir, err := h.ensureLayoutDir()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		names, _ := h.listLayouts()
		if len(names) >= maxLayouts {
			return fmt.Errorf("too many layouts (max %d): remove some first", maxLayouts)
		}
	}
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(text)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, p) // replaces even a symlink, never follows it
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cannot save the layout: %w", werr)
	}
	return nil
}

func (h *host) loadLayout(name string) (string, error) {
	p, err := h.layoutPath(name)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no such layout: %q", name)
		}
		return "", fmt.Errorf("cannot read the layout: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxLayout {
		return "", fmt.Errorf("layout %q is not a regular file of at most %d bytes", name, maxLayout)
	}
	buf := make([]byte, maxLayout+1)
	n, _ := f.Read(buf)
	text := string(buf[:n])
	if err := checkLayout(text); err != nil {
		return "", fmt.Errorf("layout %q is damaged: %v", name, err)
	}
	return text, nil
}

func (h *host) listLayouts() ([]string, error) {
	ents, err := os.ReadDir(h.layoutDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && e.Type().IsRegular() && validLayoutName(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *host) deleteLayout(name string) error {
	p, err := h.layoutPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no such layout: %q", name)
		}
		return err
	}
	return nil
}

// ------------------------------------------------------------ activity / silence watch

const (
	watchActivity byte = 1
	watchSilence  byte = 2

	watchCooldown  = 30 * time.Second // between two activity alerts for one session
	defaultSilence = 30 * time.Second
)

// setWatch applies a msgWatch from the client.
func (c *hclient) setWatch(p []byte) {
	if len(p) != 1 && len(p) != 3 {
		return
	}
	sil := defaultSilence
	if len(p) == 3 {
		if n := int(p[1])<<8 | int(p[2]); n >= 5 && n <= 3600 {
			sil = time.Duration(n) * time.Second
		}
	}
	c.cmu.Lock()
	c.watch = watchState{flags: p[0] & (watchActivity | watchSilence), silence: sil, since: time.Now(),
		actAlert: map[*session]time.Time{}, silAlert: map[*session]time.Time{}}
	c.cmu.Unlock()
}

// checkWatches tells the clients that asked about what happens in the other
// sessions of the user (one janitor tick).
func (h *host) checkWatches() {
	type sinfo struct {
		s       *session
		name    string
		lastOut time.Time
	}
	type cinfo struct {
		c   *hclient
		own *session
	}
	h.mu.Lock()
	ss := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		ss = append(ss, s)
	}
	h.mu.Unlock()
	var infos []sinfo
	var watchers []cinfo
	for _, s := range ss {
		s.mu.Lock()
		infos = append(infos, sinfo{s, "", s.lastOut})
		for c := range s.clients {
			c.cmu.Lock()
			if c.watch.flags != 0 {
				watchers = append(watchers, cinfo{c, s})
			}
			c.cmu.Unlock()
		}
		s.mu.Unlock()
	}
	if len(watchers) == 0 {
		return
	}
	alive := map[*session]bool{}
	for i := range infos {
		infos[i].name = infos[i].s.Name()
		alive[infos[i].s] = true
	}
	now := time.Now()
	for _, w := range watchers {
		var texts []string
		w.c.cmu.Lock()
		ws := &w.c.watch
		for _, o := range infos {
			if o.s == w.own {
				continue
			}
			if ws.flags&watchActivity != 0 && o.lastOut.After(ws.since) &&
				o.lastOut.After(ws.actAlert[o.s]) && now.Sub(ws.actAlert[o.s]) >= watchCooldown {
				ws.actAlert[o.s] = now
				texts = append(texts, "activity in session "+o.name)
			}
			if ws.flags&watchSilence != 0 {
				quiet := o.lastOut
				if ws.since.After(quiet) {
					quiet = ws.since
				}
				if now.Sub(quiet) >= ws.silence && !ws.silAlert[o.s].Equal(quiet) {
					ws.silAlert[o.s] = quiet
					texts = append(texts, fmt.Sprintf("session %s silent for %s", o.name, ws.silence))
				}
			}
		}
		// sessions that are gone are forgotten
		for s := range ws.actAlert {
			if !alive[s] {
				delete(ws.actAlert, s)
			}
		}
		for s := range ws.silAlert {
			if !alive[s] {
				delete(ws.silAlert, s)
			}
		}
		w.c.cmu.Unlock()
		if len(texts) == 0 {
			continue
		}
		w.own.mu.Lock()
		var flags byte
		if w.own.alt {
			flags = noticeAlt
		}
		if _, ok := w.own.clients[w.c]; ok {
			msg := append([]byte{flags}, cleanNotice(strings.Join(texts, "; "))...)
			if !w.c.push(outMsg{msgNotice, msg}) {
				delete(w.own.clients, w.c)
			}
		}
		w.own.mu.Unlock()
	}
}

// ------------------------------------------------------------ the keyboard of a shared session

// mayType tells whether c may type: when a client holds the keyboard, the
// others only watch (and are told so, once in a while).
func (s *session) mayType(c *hclient) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil || s.writer == c {
		return true
	}
	c.cmu.Lock()
	tell := time.Since(c.toldWriter) > 10*time.Second
	if tell {
		c.toldWriter = time.Now()
	}
	c.cmu.Unlock()
	if tell {
		var flags byte
		if s.alt {
			flags = noticeAlt
		}
		msg := append([]byte{flags}, "another client holds the keyboard of this session (prefix w to take it)"...)
		if !c.push(outMsg{msgNotice, msg}) {
			delete(s.clients, c)
		}
	}
	return false
}

// take gives the keyboard to c (on) or takes it back from c (off), and tells
// every client of the session.
func (s *session) take(c *hclient, on bool, peer peerInfo) {
	s.mu.Lock()
	changed := false
	text := ""
	switch {
	case on && s.writer != c:
		s.writer, changed = c, true
		text = "a client took the keyboard: the others only watch (prefix w to take it back)"
	case !on && s.writer == c:
		s.writer, changed = nil, true
		text = "the keyboard is free again: every client may type"
	}
	if changed {
		var flags byte
		if s.alt {
			flags = noticeAlt
		}
		for x := range s.clients {
			t := text
			if x == c && on {
				t = "you hold the keyboard: the other clients only watch (prefix w to give it back)"
			}
			if !x.push(outMsg{msgNotice, append([]byte{flags}, t...)}) {
				delete(s.clients, x)
			}
		}
	}
	s.mu.Unlock()
	if changed {
		ev := "keyboard_take"
		if !on {
			ev = "keyboard_release"
		}
		s.h.audit.log(ev, peer.fields(fields{"session": s.Name()}))
	}
}
