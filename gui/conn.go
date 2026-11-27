// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// conn.go: connections and tabs. One SSH connection per profile is shared by
// all its tabs; each tab is a ptscry session ("new -A -n NAME": attach if it
// exists, else create), so closing a tab or the window only detaches.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"ptscry/internal/rclient"
)

const guiVersion = "0.8.1-gui"

// hub holds the SSH connection of one profile.
type hub struct {
	p      profile
	mu     sync.Mutex
	cl     *ssh.Client
	prompt rclient.Prompter
}

func newHub(p profile, prompt rclient.Prompter) *hub { return &hub{p: p, prompt: prompt} }

func (h *hub) target() (rclient.Target, error) {
	if h.p.Mode == "local" {
		u, err := rclient.CurrentUser()
		if err != nil {
			return rclient.Target{}, err
		}
		port := rclient.DefaultPort
		if i := strings.LastIndexByte(localTarget(h.p), ':'); i >= 0 {
			port = localTarget(h.p)[i+1:]
		}
		return rclient.Target{User: u, Host: "127.0.0.1", Port: port}, nil
	}
	if h.p.Mode == "ssh" {
		return parseSSHTarget(h.p.Target)
	}
	return rclient.ParseTarget(h.p.Target)
}

// sshAuth is how a plain SSH server is logged into: the agent or a key file,
// then the password (as ssh does), or only the password.
func (h *hub) sshAuth(t rclient.Target) []ssh.AuthMethod {
	var am []ssh.AuthMethod
	if h.p.Auth != "password" {
		id := ""
		if h.p.Auth == "key" {
			id = h.p.Identity
		}
		if a, err := rclient.Auth(id, h.p.Cert, h.prompt); err == nil {
			am = append(am, a...)
		}
	}
	return append(am, rclient.PasswordAuth(t, h.prompt), rclient.InteractiveAuth(h.prompt))
}

// permanent errors are not worth retrying: the person must change something.
type permanentError struct{ error }

// client returns the live connection, dialling (and authenticating) if needed.
// Calls are serialised, so that three tabs opening together ask for the TOTP
// code once.
func (h *hub) client() (*ssh.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cl != nil {
		return h.cl, nil
	}
	t, err := h.target()
	if err != nil {
		return nil, permanentError{err}
	}
	if h.p.Mode == "ssh" {
		return h.sshClient(t)
	}
	auth, err := rclient.Auth(h.p.Identity, h.p.Cert, h.prompt)
	if err != nil {
		if strings.Contains(err.Error(), "no key:") {
			err = fmt.Errorf("nessuna chiave per %s: importa la tua chiave privata con File > Chiavi private (o Sfoglia in \"Un altro server\"), "+
				"oppure mettila in ~/.ssh/id_ed25519, oppure avvia l'agente OpenSSH (ssh-add). Dettaglio: %v", t.Addr(), err)
		}
		return nil, permanentError{err}
	}
	cl, err := rclient.Connect(t, auth, h.p.Pinned, guiVersion, h.prompt)
	if err != nil {
		return nil, classify(err, h.p, t)
	}
	h.cl = cl
	go func() {
		_ = cl.Wait()
		h.mu.Lock()
		if h.cl == cl {
			h.cl = nil
		}
		h.mu.Unlock()
	}()
	return cl, nil
}

func (h *hub) drop() {
	h.mu.Lock()
	cl := h.cl
	h.cl = nil
	h.mu.Unlock()
	if cl != nil {
		cl.Close()
	}
}

// classify tells wrong credentials from an unreachable server, and gives the
// local mode a hint that says what to configure.
func classify(err error, p profile, t rclient.Target) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		// the server refused every key (and, for ptscry, there is nothing else)
		if p.Mode == "ssh" {
			return permanentError{fmt.Errorf("%s ha rifiutato l'accesso di %q: chiave non autorizzata o password sbagliata "+
				"(il server accetta solo: %s). Controlla l'utente, oppure aggiungi la tua chiave pubblica in ~/.ssh/authorized_keys "+
				"sul server (File > Chiavi private > Copia la chiave pubblica)", t.Addr(), t.User, acceptedMethods(msg))}
		}
		if t.User == "root" {
			return permanentError{fmt.Errorf("%s: root non puo' entrare dal front-end di ptscry (come PermitRootLogin no): "+
				"usa il tuo utente personale e, se serve, sudo nella sessione", t.Addr())}
		}
		return permanentError{fmt.Errorf("%s non accetta nessuna delle tue chiavi per l'utente %q. Sul server, da root: "+
			"\"ptscry setup keys add %s -\" e incolla la chiave pubblica (File > Chiavi private > Copia la chiave pubblica); "+
			"controlla anche che l'utente sia quello giusto (campo Utente)", t.Addr(), t.User, t.User)}
	case strings.Contains(msg, "HAS CHANGED"),
		strings.Contains(msg, "not accepted"), strings.Contains(msg, "does not match the fingerprint"):
		return permanentError{err}
	case strings.Contains(msg, "refused"): // "connection refused"; Windows: "actively refused it"
		// nothing listens on that port (a firewall would rather time out)
		switch p.Mode {
		case "local":
			alt := "oppure usa il tipo \"Terminale locale\" con \"Sessione persistente\", che non richiede server"
			if runtime.GOOS == "windows" {
				alt = "su Windows il server ptscry non c'e': usa il tipo \"ptscry\" verso un server Linux, o il server in WSL"
			}
			return permanentError{fmt.Errorf("nessun server ptscry risponde su %s di questo computer: va attivato il front-end SSH "+
				"(sul server: sudo ptscry setup frontend); %s", t.Addr(), alt)}
		case "ssh":
			return permanentError{fmt.Errorf("%s ha rifiutato la connessione: su quella porta non c'e' un server SSH "+
				"(sshd attivo? la porta e' giusta? di solito 22)", t.Addr())}
		default:
			return permanentError{fmt.Errorf("%s ha rifiutato la connessione: su quella porta non risponde il front-end di ptscry. "+
				"Sul server va attivato con \"sudo ptscry setup frontend\" (porta frontend_listen, di solito 2222); "+
				"per un server SSH normale usa il tipo \"SSH\" (porta 22)", t.Addr())}
		}
	}
	return err
}

// tab is one terminal bound to one session.
type tab struct {
	id         int
	h          *hub
	profile    string
	session    string
	shell      string // "terminale locale": the shell command of this tab (empty = the profile's)
	shellLabel string // its name, for the tab
	model      *termModel
	view       *termView
	app        *gui

	mu       sync.Mutex
	status   string // "", "connecting", "live", "lost", "ended", "error"
	detail   string
	term     *gterm
	closing  bool
	logged   string        // the last state written in the activity panel
	detached bool          // the person asked to disconnect: no automatic reattach
	frozen   bool          // the processes of the session are stopped
	cwd      string        // last known directory of the session
	label    string        // name the person gave to the tab
	env      []envVar      // variables of this session, typed in when it is created
	ro       bool          // attached read-only
	redo     chan struct{} // reattach after the session ended
	in       chan []byte
	rows     int
	cols     int
	sizeCh   chan struct{}
	done     chan struct{}

	paneFields
	lastCtl time.Time // the last request of the session to the GUI (OSC 7799)
	liveAt  time.Time // when the terminal last (re)attached

	pfx      atomic.Int32 // the prefix key of the ptscry client in the terminal (0: none)
	prefixAt atomic.Int64 // when the person last typed it (unix ns)
}

func (a *gui) newTab(h *hub, sp tabSpec, scrollback int) *tab {
	session, cwd := sp.session, sp.cwd
	a.nextID++
	t := &tab{id: a.nextID, h: h, profile: h.p.Name, session: session, cwd: cwd, label: sp.label, shell: sp.shell, ro: sp.ro, env: cleanEnv(sp.env), app: a,
		redo: make(chan struct{}, 1), in: make(chan []byte, 1024), sizeCh: make(chan struct{}, 1), done: make(chan struct{})}
	t.model = newTermModel(80, 24, scrollback, func(b []byte) { t.queue(b) })
	t.model.bell = func() { a.bell() }
	t.model.onTitle = func(string) { a.invalidate() }
	t.view = newTermView(t.model, a.terminalShaper(), a.set.FontSize)
	t.view.face = a.set.FontFace
	t.view.input = t.queue
	t.view.mirrors = func() []*termView { return a.mirrors(t) }
	t.model.onCtl = func(args []string) {
		select {
		case a.ctl <- ctlReq{t, args}:
			a.invalidate()
		default:
		}
	}
	t.view.onSize = func(c, r int) {
		t.mu.Lock()
		t.cols, t.rows = c, r
		t.mu.Unlock()
		select {
		case t.sizeCh <- struct{}{}:
		default:
		}
	}
	t.view.cmd = termCommands{
		newTab: a.openNewTabDialog, closeTab: func() { a.closePane(t) }, nextTab: func() { a.cycle(1) }, prevTab: func() { a.cycle(-1) },
		zoom: a.zoom, sessions: a.openSessionsDialog, workspaces: a.openWorkspacesDialog, rename: func() { a.openRenameDialog(t) }, env: func() { a.openEnvDialog(t) },
		detach: func() { a.detachTab(t) }, freeze: func() { a.toggleFreeze(t) },
		pane: func(act string) bool { return a.paneKey(t, act) },
		app:  a.appKey,
	}
	go t.resizer()
	go t.writer()
	go t.run()
	return t
}

// queue sends keys and answers to the program without ever blocking the UI.
func (t *tab) queue(b []byte) {
	if p := byte(t.pfx.Load()); p != 0 && bytes.IndexByte(b, p) >= 0 {
		t.prefixAt.Store(time.Now().UnixNano()) // the person used a command of the ptscry client
	}
	cp := append([]byte(nil), b...)
	select {
	case t.in <- cp:
	default: // a dead connection: dropping is better than freezing the window
	}
}

func (t *tab) writer() {
	for {
		select {
		case b := <-t.in:
			t.mu.Lock()
			term := t.term
			t.mu.Unlock()
			if term != nil {
				_, _ = term.In.Write(b)
			} else if len(b) == 1 && (b[0] == '\r' || b[0] == '\n') {
				// Enter on a finished tab reattaches
				select {
				case t.redo <- struct{}{}:
				default:
				}
			}
		case <-t.done:
			return
		}
	}
}

func (t *tab) resizer() {
	for {
		select {
		case <-t.sizeCh:
			t.mu.Lock()
			term, c, r := t.term, t.cols, t.rows
			t.mu.Unlock()
			if term != nil {
				_ = term.Resize(r, c)
			}
		case <-t.done:
			return
		}
	}
}

func (t *tab) setStatus(s, detail string) {
	if s == "live" {
		t.mu.Lock()
		t.liveAt = time.Now()
		t.mu.Unlock()
	}
	t.mu.Lock()
	t.status, t.detail = s, detail
	closing := t.closing
	// the activity panel gets the changes, not every attempt of a reconnection
	news := s != "connecting" && s != t.logged
	if news {
		t.logged = s
	}
	prof := t.profile
	t.mu.Unlock()
	if news && !closing {
		t.logStatus(prof, s, detail)
	}
	t.app.invalidate()
}

// logStatus puts the changes of the connection in the activity panel.
func (t *tab) logStatus(prof, s, detail string) {
	who := prof + " · " + t.sessionName()
	if t.h != nil && (t.h.p.Mode == "shell") {
		who = "terminale locale"
	}
	switch s {
	case "live":
		t.app.logAct(false, "%s: collegato", who)
	case "lost":
		t.app.logAct(true, "%s: connessione persa (%s)", who, detail)
	case "error":
		t.app.logAct(true, "%s: errore: %s", who, detail)
	case "ended":
		t.app.logAct(false, "%s: chiuso (%s)", who, detail)
	}
}

func (t *tab) State() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status, t.detail
}

func (t *tab) say(format string, a ...any) {
	t.model.Feed([]byte("\r\n\x1b[2m[ptscry-gui: " + fmt.Sprintf(format, a...) + "]\x1b[0m\r\n"))
	t.app.invalidate()
}

func (t *tab) isClosing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closing
}

// run keeps the tab attached to its session: it connects, copies the output to
// the screen, and when the connection drops reconnects and reattaches.
func (t *tab) run() {
	defer close(t.done)
	if t.h.p.Mode == "standalone" || t.h.p.Mode == "shell" {
		t.runLocal()
		return
	}
	attempt := 0
	prefix := byte(0)
	if cc, _ := rclient.ReadConf(rclient.ConfigPaths(), true); cc.Prefix != 0 {
		prefix = cc.Prefix
	}
	if prefix != 0 {
		t.pfx.Store(int32(prefix))
	} else {
		t.pfx.Store(int32(rclient.DefaultPrefix)) // the server's, most likely
	}
	for !t.isClosing() {
		t.setStatus("connecting", "")
		cl, err := t.h.client()
		if err != nil {
			var pe permanentError
			if errors.As(err, &pe) {
				t.setStatus("error", err.Error())
				t.say("%v", err)
				if !t.waitRedo() {
					return
				}
				continue
			}
			attempt++
			if attempt > 20 {
				t.setStatus("error", err.Error())
				t.say("%v", err)
				if !t.waitRedo() {
					return
				}
				attempt = 0
				continue
			}
			t.setStatus("lost", fmt.Sprintf("riconnessione %d/20: %v", attempt, err))
			if !t.sleep(time.Duration(min(3*attempt, 15)) * time.Second) {
				return
			}
			continue
		}
		t.mu.Lock()
		cols, rows := t.cols, t.rows
		t.mu.Unlock()
		if cols == 0 {
			cols, rows = t.model.Size()
		}
		plain := t.h.p.Mode == "ssh"
		fresh, line, pfx := false, t.h.p.Command, byte(0)
		if !plain {
			fresh, line, pfx = t.checkFresh(), t.attachLine(), prefix
		}
		sterm, err := rclient.OpenTerm(cl, line, rows, cols, "xterm-256color", pfx)
		var term *gterm
		if err == nil {
			term = fromSSH(sterm)
		}
		if err != nil {
			var oce *ssh.OpenChannelError
			if !errors.As(err, &oce) {
				t.h.drop() // the connection is broken; a refused channel is not (the others keep working)
			}
			attempt++
			t.setStatus("lost", err.Error())
			if !t.sleep(time.Duration(min(3*attempt, 15)) * time.Second) {
				return
			}
			continue
		}
		attempt = 0
		t.mu.Lock()
		t.term = term
		t.mu.Unlock()
		// the window may have changed size while we were connecting
		select {
		case t.sizeCh <- struct{}{}:
		default:
		}
		t.setStatus("live", "")
		t.model.Feed([]byte("\x1b[H\x1b[2J")) // the session redraws its screen: do not stack it on the old one
		if !plain {
			go t.seedLater(term, fresh)
		}
		copied := make(chan struct{})
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := term.Out.Read(buf)
				if n > 0 {
					t.model.Feed(buf[:n])
					t.app.invalidate()
				}
				if err != nil {
					break
				}
			}
			close(copied)
		}()
		res := term.Wait()
		select {
		case <-copied:
		case <-time.After(2 * time.Second):
		}
		t.mu.Lock()
		t.term = nil
		closing := t.closing
		t.mu.Unlock()
		term.Close()
		if closing {
			return
		}
		if t.wasDetached() {
			t.setStatus("ended", "disconnesso")
			t.say("disconnesso dalla sessione %q: continua a vivere; premi Invio per riattaccare, Ctrl+Maiusc+W per chiudere il tab", t.sessionName())
			if !t.waitRedo() {
				return
			}
			continue
		}
		if plain {
			if res.Lost {
				t.h.drop()
			}
			t.setStatus("ended", fmt.Sprintf("uscita %d", res.Code))
			t.say("connessione SSH terminata (codice %d): premi Invio per riconnetterti, Ctrl+Maiusc+W per chiudere", res.Code)
			if !t.waitRedo() {
				return
			}
			continue
		}
		if res.Lost {
			t.h.drop()
			t.setStatus("lost", "connessione persa")
			t.say("connessione persa: riconnessione e riattacco alla sessione %q", t.sessionName())
			if !t.sleep(2 * time.Second) {
				return
			}
			continue
		}
		t.setStatus("ended", fmt.Sprintf("uscita %d", res.Code))
		t.say("sessione %q staccata o terminata (codice %d): premi Invio per riattaccare, Ctrl+Maiusc+W per chiudere", t.sessionName(), res.Code)
		if !t.waitRedo() {
			return
		}
	}
}

func (t *tab) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return !t.isClosing()
	case <-t.redo:
		return !t.isClosing()
	}
}

// waitRedo blocks until the person asks to reattach (true) or closes the tab.
func (t *tab) waitRedo() bool {
	for {
		if t.isClosing() {
			return false
		}
		select {
		case <-t.redo:
			return !t.isClosing()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Detach disconnects from the session but keeps the tab: the session goes on
// running; Enter in the tab attaches again.
func (t *tab) Detach() bool {
	t.mu.Lock()
	term := t.term
	if term != nil {
		t.detached = true
	}
	t.mu.Unlock()
	if term == nil {
		return false
	}
	term.Close()
	return true
}

// wasDetached tells (once) whether the end of the connection was asked for.
func (t *tab) wasDetached() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.detached
	t.detached = false
	return d
}

func (t *tab) isFrozen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frozen
}

// Close detaches (the session keeps running on the server) and ends the tab.
func (t *tab) Close() {
	t.mu.Lock()
	t.closing = true
	term := t.term
	t.mu.Unlock()
	if term != nil {
		term.Close()
	}
	select {
	case t.redo <- struct{}{}:
	default:
	}
	t.model.Close()
}

// runLocal is run for the standalone profile: the client of this computer on a
// local pseudoterminal, no connection to keep up.
func (t *tab) runLocal() {
	for !t.isClosing() {
		t.setStatus("connecting", "")
		fresh := t.checkFresh()
		for i := 0; i < 50; i++ { // the first frame tells the real size of the window
			t.mu.Lock()
			known := t.cols != 0
			t.mu.Unlock()
			if known {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.mu.Lock()
		cols, rows := t.cols, t.rows
		t.mu.Unlock()
		if cols == 0 {
			cols, rows = t.model.Size()
		}
		p := t.h.p
		if p.Mode == "shell" && t.shell != "" {
			p.Target = t.shell
		}
		term, err := openLocal(p, t.sessionName(), t.ro, rows, cols)
		if err != nil {
			t.setStatus("error", err.Error())
			t.say("%v", err)
			if !t.waitRedo() {
				return
			}
			continue
		}
		t.mu.Lock()
		t.term = term
		t.mu.Unlock()
		select {
		case t.sizeCh <- struct{}{}:
		default:
		}
		t.setStatus("live", "")
		t.model.Feed([]byte("\x1b[H\x1b[2J")) // the session redraws its screen: do not stack it on the old one
		go t.seedLater(term, fresh)
		go func() {
			// the client of this computer reads the size once at start-up and a
			// signal sent before it is ready is lost: say it again
			for _, d := range []time.Duration{300 * time.Millisecond, 900 * time.Millisecond} {
				time.Sleep(d)
				select {
				case t.sizeCh <- struct{}{}:
				default:
				}
			}
		}()
		copied := make(chan struct{})
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := term.Out.Read(buf)
				if n > 0 {
					t.model.Feed(buf[:n])
					t.app.invalidate()
				}
				if err != nil {
					break
				}
			}
			close(copied)
		}()
		res := term.Wait()
		select {
		case <-copied:
		case <-time.After(2 * time.Second):
		}
		t.mu.Lock()
		t.term = nil
		closing := t.closing
		t.mu.Unlock()
		term.Close()
		if closing {
			return
		}
		if t.h.p.Mode == "shell" {
			t.setStatus("ended", fmt.Sprintf("uscita %d", res.Code))
			t.say("la shell e' terminata (codice %d): premi Invio per avviarne una nuova, Ctrl+Maiusc+W per chiudere", res.Code)
			if !t.waitRedo() {
				return
			}
			continue
		}
		if t.wasDetached() {
			t.setStatus("ended", "disconnesso")
			t.say("disconnesso dalla sessione %q: continua a vivere; premi Invio per riattaccare, Ctrl+Maiusc+W per chiudere il tab", t.sessionName())
			if !t.waitRedo() {
				return
			}
			continue
		}
		t.setStatus("ended", fmt.Sprintf("uscita %d", res.Code))
		t.say("sessione %q staccata o terminata (codice %d): premi Invio per riattaccare, Ctrl+Maiusc+W per chiudere", t.sessionName(), res.Code)
		if !t.waitRedo() {
			return
		}
	}
}

// attachLine is the command that opens the session of the tab: create or
// attach, or attach read-only.
func (t *tab) attachLine() string {
	if t.ro {
		return rclient.ProgName + " attach -r " + t.sessionName()
	}
	return rclient.ProgName + " new -A -n " + t.sessionName()
}

// title is what the tab bar shows.
func (t *tab) title() string {
	s := t.baseTitle()
	switch t.kind() {
	case "ssh":
		s = "[SSH] " + s
	case "ptscry":
		s = "[PTS] " + s
	}
	if t.isFrozen() {
		return "[II] " + s
	}
	return s
}

// kind is what the tab is attached to: "local" (a shell of this computer,
// also a ptscry session without server), "ptscry" (a ptscry server, this
// computer's or a remote one) or "ssh" (a plain SSH server).
func (t *tab) kind() string {
	switch t.h.p.Mode {
	case "shell", "standalone":
		return "local"
	case "ssh":
		return "ssh"
	}
	return "ptscry"
}

// shellName is the name of the local shell of the tab, without its path
// (worked out once: on macOS finding the login shell runs a program).
func (t *tab) shellName() string {
	t.mu.Lock()
	n := t.shellLabel
	t.mu.Unlock()
	if n == "" {
		n = t.findShellName()
		t.mu.Lock()
		t.shellLabel = n
		t.mu.Unlock()
	}
	return n
}

func (t *tab) findShellName() string {
	cmd := ""
	switch t.h.p.Mode {
	case "shell":
		cmd = t.shell
		if cmd == "" {
			cmd = t.h.p.Target
		}
		if cmd == "" {
			cmd = autoShellName()
		}
	case "standalone":
		cmd = loginShell()
	}
	w, err := splitCommand(cmd)
	if runtime.GOOS == "windows" && strings.HasPrefix(cmd, `"`) {
		// a Windows command line: the program is between the quotes
		if i := strings.Index(cmd[1:], `"`); i >= 0 {
			w, err = []string{cmd[1 : i+1]}, nil
		}
	}
	if err != nil || len(w) == 0 {
		return "shell"
	}
	n := w[0]
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.TrimSuffix(strings.TrimSuffix(n, ".exe"), ".EXE")
	if strings.EqualFold(n, "wsl") {
		return "WSL"
	}
	if n == "" {
		return "shell"
	}
	return n
}

func (t *tab) baseTitle() string {
	t.mu.Lock()
	l := t.label
	t.mu.Unlock()
	if l != "" {
		return l
	}
	switch t.h.p.Mode {
	case "shell":
		return t.shellName()
	case "standalone":
		return t.shellName() + " (" + t.sessionName() + ")"
	}
	if m := t.model.Title(); m != "" {
		return m
	}
	return t.sessionName()
}

// sshClient connects to a plain SSH server, through the jump host if any. The
// caller holds h.mu.
func (h *hub) sshClient(t rclient.Target) (*ssh.Client, error) {
	var jump *ssh.Client
	if h.p.Jump != "" {
		jt, err := parseSSHTarget(h.p.Jump)
		if err != nil {
			return nil, permanentError{fmt.Errorf("jump host: %v", err)}
		}
		if jt.User == "" {
			jt.User = t.User
		}
		jump, err = rclient.Connect(jt, h.sshAuth(jt), "", guiVersion, h.prompt)
		if err != nil {
			return nil, classify(fmt.Errorf("jump host %s: %w", jt.Addr(), err), h.p, jt)
		}
	}
	cl, err := rclient.ConnectVia(jump, t, h.sshAuth(t), h.p.Pinned, guiVersion, h.prompt)
	if err != nil {
		if jump != nil {
			jump.Close()
		}
		return nil, classify(err, h.p, t)
	}
	h.cl = cl
	go func() {
		_ = cl.Wait()
		if jump != nil {
			jump.Close()
		}
		h.mu.Lock()
		if h.cl == cl {
			h.cl = nil
		}
		h.mu.Unlock()
	}()
	return cl, nil
}

// acceptedMethods reads "attempted methods [none publickey]" out of the error.
func acceptedMethods(msg string) string {
	i := strings.Index(msg, "attempted methods [")
	if i < 0 {
		return "?"
	}
	m := msg[i+len("attempted methods ["):]
	if j := strings.IndexByte(m, ']'); j >= 0 {
		m = m[:j]
	}
	m = strings.TrimSpace(strings.ReplaceAll(m, "none", ""))
	if m == "" {
		return "?"
	}
	return m
}
