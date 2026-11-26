//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// proto.go: wire protocol between client and host.
//
// Every message is a frame: 1 byte type, 4 bytes big-endian payload length,
// payload. The payload size is capped (maxFrame) so a peer can never make the
// other side allocate arbitrary memory.

package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	msgHello  byte = 1  // C->H  JSON hello
	msgData   byte = 2  // both  raw terminal bytes
	msgResize byte = 3  // C->H  rows(2) cols(2), big endian
	msgDetach byte = 4  // C->H  request detach
	msgOK     byte = 5  // H->C  JSON okReply
	msgErr    byte = 6  // H->C  error text
	msgExit   byte = 7  // H->C  int32 exit status of the session
	msgBye    byte = 8  // H->C  reason text (detached, killed, ...)
	msgNotice byte = 9  // H->C  flags(1) + notice text (ptscry notify); older clients ignore it
	msgRedraw byte = 10 // C->H  ask the program in the session to repaint (after a notice)
	msgWatch  byte = 11 // C->H  flags(1) [+ silence seconds(2)]: tell me about activity / silence elsewhere
	msgName   byte = 12 // H->C  the session was renamed: its new name
	msgTake   byte = 13 // C->H  1: this client takes the keyboard (the others only watch), 0: gives it back, 2: toggle
)

const maxFrame = 1 << 20

var errFrameTooBig = errors.New("frame too large")

// framed wraps a connection with frame read/write helpers. Writes are
// serialised so several goroutines can send on the same connection.
type framed struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func newFramed(c net.Conn) *framed {
	return &framed{c: c, r: bufio.NewReaderSize(c, 64*1024)}
}

func (f *framed) write(t byte, p []byte) error {
	if len(p) > maxFrame {
		return errFrameTooBig
	}
	buf := make([]byte, 5+len(p))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(p)))
	copy(buf[5:], p)
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = f.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := f.c.Write(buf)
	return err
}

func (f *framed) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errFrameTooBig
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(f.r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// hello is the first message a client sends.
type hello struct {
	Op       string            `json:"op"`             // new | attach | list | kill | stop | admin-list | admin-kill | admin-stop
	User     string            `json:"user,omitempty"` // admin operations: target user
	Name     string            `json:"name,omitempty"`
	Cmd      []string          `json:"cmd,omitempty"`
	Rows     uint16            `json:"rows,omitempty"`
	Cols     uint16            `json:"cols,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	ReadOnly bool              `json:"readonly,omitempty"`
	Create   bool              `json:"create,omitempty"`   // attach-or-create (new -A / attach -c)
	Detached bool              `json:"detached,omitempty"` // new: do not attach
	Job      bool              `json:"job,omitempty"`      // new: keep the result after the command ends (run -d)
	Remove   bool              `json:"rm,omitempty"`       // attach to a finished job: discard it after delivery (wait -x)
	NewName  string            `json:"new_name,omitempty"` // rename
	Text     string            `json:"text,omitempty"`     // notify, send-keys, layout-set
	// Proxy is set only by the SSH front-end, which acts for a remote user.
	Proxy *proxyInfo `json:"proxy,omitempty"`
}

// proxyInfo is what the front-end tells the monitor about the remote user it
// is acting for. The monitor accepts it only from the front-end's uid, and
// checks the ticket (issued by the monitor itself) before acting.
type proxyInfo struct {
	User        string   `json:"user"`
	Remote      string   `json:"remote,omitempty"`
	Fingerprint string   `json:"fp,omitempty"`
	KeyType     string   `json:"key_type,omitempty"`
	CertID      string   `json:"cert_id,omitempty"`
	Serial      uint64   `json:"cert_serial,omitempty"`
	Method      string   `json:"method,omitempty"`
	Ticket      string   `json:"ticket,omitempty"`
	Code        string   `json:"code,omitempty"`    // fe-totp only; never logged
	Count       int      `json:"count,omitempty"`   // fe-report
	Sources     []string `json:"sources,omitempty"` // fe-report
}

type sessionInfo struct {
	User     string    `json:"user,omitempty"`
	Name     string    `json:"name"`
	PID      int       `json:"pid"`
	Created  time.Time `json:"created"`
	Last     time.Time `json:"last_activity"`
	Attached int       `json:"attached"`
	Rows     uint16    `json:"rows"`
	Cols     uint16    `json:"cols"`
	Cmd      string    `json:"cmd"`
	Cwd      string    `json:"cwd,omitempty"`    // working directory of the session process (Linux)
	Frozen   bool      `json:"frozen,omitempty"` // its processes are stopped (ptscry freeze)
}

// jobInfo describes a finished job whose result the host still holds.
type jobInfo struct {
	Name     string    `json:"name"`
	Cmd      string    `json:"cmd"`
	Created  time.Time `json:"created"`
	Ended    time.Time `json:"ended"`
	ExitCode int       `json:"exit_code"`
	Reason   string    `json:"reason"`
	Expires  time.Time `json:"expires"`
}

type okReply struct {
	Session  *sessionInfo  `json:"session,omitempty"`
	Sessions []sessionInfo `json:"sessions,omitempty"`
	Jobs     []jobInfo     `json:"jobs,omitempty"`
	Created  bool          `json:"created,omitempty"`
	Prefix   byte          `json:"prefix,omitempty"` // detach prefix from the user's policy
	Message  string        `json:"message,omitempty"`
	Keys     []string      `json:"keys,omitempty"`      // fe-keys
	NeedTOTP bool          `json:"need_totp,omitempty"` // fe-gate
	Ticket   string        `json:"ticket,omitempty"`    // fe-gate, fe-totp
	Text     string        `json:"text,omitempty"`      // layout-get
	Names    []string      `json:"names,omitempty"`     // layout-list
}

func encodeResize(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b
}

func decodeResize(p []byte) (rows, cols uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	rows = binary.BigEndian.Uint16(p[0:2])
	cols = binary.BigEndian.Uint16(p[2:4])
	return rows, cols, rows > 0 && cols > 0
}
