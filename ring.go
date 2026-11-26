//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ring.go: bounded replay buffer of recent terminal output.

package main

import "bytes"

type ring struct {
	buf  []byte
	pos  int
	full bool
}

func newRing(size int) *ring {
	return &ring{buf: make([]byte, size)}
}

func (r *ring) Write(p []byte) {
	size := len(r.buf)
	if size == 0 || len(p) == 0 {
		return
	}
	if len(p) >= size {
		copy(r.buf, p[len(p)-size:])
		r.pos = 0
		r.full = true
		return
	}
	if r.pos+len(p) >= size {
		r.full = true
	}
	n := copy(r.buf[r.pos:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.pos = (r.pos + len(p)) % size
}

// Snapshot returns the buffered bytes in order and whether older output has
// already been overwritten.
func (r *ring) Snapshot() (data []byte, truncated bool) {
	if !r.full {
		return append([]byte(nil), r.buf[:r.pos]...), false
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.pos:]...)
	out = append(out, r.buf[:r.pos]...)
	return out, true
}

// cleanReplay makes a snapshot safe to replay on a fresh terminal. When older
// output was overwritten the snapshot may start in the middle of an escape
// sequence or a UTF-8 character, so everything up to the first newline is
// dropped, and attributes are reset first.
func cleanReplay(data []byte, truncated bool) []byte {
	if len(data) == 0 {
		return nil
	}
	if truncated {
		if i := bytes.IndexByte(data, '\n'); i >= 0 && i < 8192 {
			data = data[i+1:]
		} else {
			for len(data) > 0 && data[0]&0xC0 == 0x80 { // UTF-8 continuation
				data = data[1:]
			}
		}
	}
	return append([]byte("\x1b[0m"), stripControlOSC(data)...)
}

// A request of a program to the client (ptscry split, ptscry layout apply,
// the session announcement) is an OSC 7799, with the 7-bit (ESC ]) or the
// 8-bit (0x9D) introducer. Such a request is meant once: a replay must not
// make a client that attaches later act on it again.
var ctlOSCPrefixes = [][]byte{[]byte("\x1b]7799;"), []byte("\x9d7799;")}

// stripControlOSC removes the client requests, until none is left: taking
// one out must not join the pieces around it into a new one.
func stripControlOSC(b []byte) []byte {
	for {
		i, plen := -1, 0
		for _, p := range ctlOSCPrefixes {
			if k := bytes.Index(b, p); k >= 0 && (i < 0 || k < i) {
				i, plen = k, len(p)
			}
		}
		if i < 0 {
			return b
		}
		rest := b[i+plen:]
		end := len(rest)
		for k := 0; k < len(rest); k++ {
			if rest[k] == 0x07 || rest[k] == 0x9c {
				end = k + 1
				break
			}
			if rest[k] == 0x1b && k+1 < len(rest) && rest[k+1] == '\\' {
				end = k + 2
				break
			}
		}
		out := make([]byte, 0, len(b))
		out = append(out, b[:i]...)
		b = append(out, rest[end:]...)
	}
}

var (
	altOn  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altOff = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

func lastIndexOfAny(b []byte, seqs [][]byte) int {
	best := -1
	for _, s := range seqs {
		if i := bytes.LastIndex(b, s); i > best {
			best = i
		}
	}
	return best
}

// altScreenState scans a chunk of output for alternate-screen switches and
// returns the new state (unchanged when the chunk has none).
func altScreenState(chunk []byte, cur bool) bool {
	on, off := lastIndexOfAny(chunk, altOn), lastIndexOfAny(chunk, altOff)
	switch {
	case on > off:
		return true
	case off > on:
		return false
	}
	return cur
}
