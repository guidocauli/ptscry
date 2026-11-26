//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// recorder.go: optional session recording in asciicast v2 format
// (playable with `asciinema play`).
//
// record = output  -> terminal output only
// record = all     -> output and keystrokes (may capture passwords!)

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// recSink receives the data of a session to record.
type recSink interface {
	output(b []byte)
	inputData(b []byte)
	close()
	path() string
}

type recorder struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
	input bool
	carry map[string][]byte // incomplete trailing UTF-8 bytes per stream
	Path  string
	werr  error // first write error (the monitor kills the session on it)

	max     int64 // size cap (0 = none)
	written int64
	checked int64 // value of written at the last free-space check
	dir     string
}

// Recording stops (and the session with it) when the filesystem is nearly
// full: a user must not be able to fill the disk the audit log lives on.
const (
	minFreeBytes   = 64 << 20
	freeCheckEvery = 1 << 20
)

func newRecorder(dir, session, mode, term string, rows, cols uint16, maxBytes int64) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%06d.cast", session, now.Unix(), now.Nanosecond()/1000))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	hdr, _ := json.Marshal(map[string]any{
		"version":   2,
		"width":     cols,
		"height":    rows,
		"timestamp": now.Unix(),
		"title":     session,
		"env":       map[string]string{"TERM": term},
	})
	if _, err := f.Write(append(hdr, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	return &recorder{f: f, start: now, input: mode == "all", carry: map[string][]byte{}, Path: path,
		max: maxBytes, dir: dir, written: int64(len(hdr)) + 1}, nil
}

func (r *recorder) event(kind string, b []byte) {
	buf := append(r.carry[kind], b...)
	cut := len(buf)
	for i := 1; i <= 3 && i <= len(buf); i++ {
		if !utf8.RuneStart(buf[len(buf)-i]) {
			continue
		}
		if !utf8.FullRune(buf[len(buf)-i:]) {
			cut = len(buf) - i
		}
		break
	}
	r.carry[kind] = append([]byte(nil), buf[cut:]...)
	if cut == 0 {
		return
	}
	line, err := json.Marshal([]any{time.Since(r.start).Seconds(), kind, string(buf[:cut])})
	if err != nil {
		return
	}
	if r.werr != nil {
		return
	}
	n, err := r.f.Write(append(line, '\n'))
	if err != nil {
		r.werr = err
		return
	}
	r.written += int64(n)
	if r.max > 0 && r.written > r.max {
		r.werr = fmt.Errorf("recording size limit reached (%d bytes)", r.max)
		return
	}
	if r.written-r.checked >= freeCheckEvery {
		r.checked = r.written
		var st syscall.Statfs_t
		if syscall.Statfs(r.dir, &st) == nil && int64(st.Bavail)*int64(st.Bsize) < minFreeBytes {
			r.werr = errors.New("filesystem almost full: recording stopped")
		}
	}
}

func (r *recorder) output(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event("o", b)
}

func (r *recorder) inputData(b []byte) {
	if !r.input {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event("i", b)
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.f.Close()
}

func (r *recorder) path() string { return r.Path }

// err returns the first write error, if any.
func (r *recorder) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.werr
}

// recState is a recorder's state across a monitor re-exec (the file descriptor
// itself is carried over; fd is its number in the new process).
type recState struct {
	Path      string            `json:"path"`
	FD        int               `json:"fd"`
	StartNano int64             `json:"start_ns"`
	Input     bool              `json:"input"`
	Carry     map[string][]byte `json:"carry,omitempty"`
	Written   int64             `json:"written"`
	Checked   int64             `json:"checked"`
	Max       int64             `json:"max"`
	Dir       string            `json:"dir"`
}

func (r *recorder) export(fd int) recState {
	r.mu.Lock()
	defer r.mu.Unlock()
	carry := map[string][]byte{}
	for k, v := range r.carry {
		carry[k] = append([]byte(nil), v...)
	}
	return recState{Path: r.Path, FD: fd, StartNano: r.start.UnixNano(), Input: r.input, Carry: carry,
		Written: r.written, Checked: r.checked, Max: r.max, Dir: r.dir}
}

func recorderFromState(st recState) *recorder {
	carry := st.Carry
	if carry == nil {
		carry = map[string][]byte{}
	}
	return &recorder{f: os.NewFile(uintptr(st.FD), st.Path), start: time.Unix(0, st.StartNano), input: st.Input,
		carry: carry, Path: st.Path, max: st.Max, written: st.Written, checked: st.Checked, dir: st.Dir}
}
