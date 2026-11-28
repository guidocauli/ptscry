//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ctl.go: control channel between the monitor (root) and a user host.
//
// The channel is an anonymous SOCK_SEQPACKET socketpair created by the
// monitor and inherited by the host as fd 3. Nobody else holds it, so no
// other process (not even one running as the same user) can speak on it.
// Each datagram is: 1 byte type, then the payload. A datagram may carry one
// file descriptor (SCM_RIGHTS): the monitor uses it to hand an authenticated
// client connection over to the host.

package main

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	// monitor -> host
	ctlInit     byte = 'I' // JSON initMsg (first message)
	ctlClient   byte = 'C' // JSON clientMsg + fd of the client connection
	ctlKill     byte = 'K' // JSON {session, reason}
	ctlShutdown byte = 'S' // JSON {reason}
	ctlRecAck   byte = 'A' // JSON {session, ok, error, file}
	ctlSpawned  byte = 'P' // JSON spawnReply (+ fd of the terminal master when ok)
	ctlExited   byte = 'Z' // JSON {session, code}: the session's process ended
	ctlByeAck   byte = 'b' // no payload: after this, no more clients for the host

	// host -> monitor
	ctlEvent     byte = 'E' // JSON {event, fields}
	ctlNewResult byte = 'N' // JSON {req, ok, error}
	ctlRecOpen   byte = 'O' // JSON {session, mode, term, rows, cols}
	ctlRecData   byte = 'D' // kind(1) namelen(1) name data
	ctlRecClose  byte = 'X' // session name
	ctlSpawn     byte = 'W' // JSON launchRequest: start a session process
	ctlKillSess  byte = 'T' // JSON {session}: end a session's process (monitor signals it)
	ctlBye       byte = 'B' // no payload: the host is ending, send no more clients
)

const (
	ctlMaxMsg = 128 * 1024
	ctlChunk  = 32 * 1024
)

type ctlConn struct {
	c   *net.UnixConn
	wmu sync.Mutex
	// wait: how long a send may block (0 = no limit). The monitor sets it for
	// the hosts: a host is a process of the user, who can stop it (SIGSTOP);
	// no root goroutine must wait on it for ever.
	wait time.Duration
}

func newCtlConn(c *net.UnixConn) *ctlConn { return &ctlConn{c: c} }

// newHostCtl is the monitor's end of a host's control socket.
func newHostCtl(c *net.UnixConn) *ctlConn { return &ctlConn{c: c, wait: hostCtlWait} }

// hostCtlWait limits the monitor's sends to a host.
const hostCtlWait = 10 * time.Second

func (c *ctlConn) deadline() {
	if c.wait > 0 {
		_ = c.c.SetWriteDeadline(time.Now().Add(c.wait))
	}
}

func ctlBuf(t byte, payload []byte) ([]byte, error) {
	if len(payload)+1 > ctlMaxMsg {
		return nil, errors.New("control message too large")
	}
	buf := make([]byte, 1+len(payload))
	buf[0] = t
	copy(buf[1:], payload)
	return buf, nil
}

func (c *ctlConn) send(t byte, payload []byte) error {
	buf, err := ctlBuf(t, payload)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.deadline()
	_, err = c.c.Write(buf)
	return err
}

// sendFD sends a message carrying f. The caller still owns (and must close) f.
func (c *ctlConn) sendFD(t byte, payload []byte, f *os.File) error {
	buf, err := ctlBuf(t, payload)
	if err != nil {
		return err
	}
	var rerr error
	var oob []byte
	if err := fdControl(f, func(fd uintptr) error {
		oob = syscall.UnixRights(int(fd))
		c.wmu.Lock()
		defer c.wmu.Unlock()
		c.deadline()
		_, _, rerr = c.c.WriteMsgUnix(buf, oob, nil)
		return rerr
	}); err != nil {
		return err
	}
	return rerr
}

// recv reads one datagram. A passed descriptor is returned only when allowFD
// is set; otherwise (and for any extra descriptors) it is closed immediately.
func (c *ctlConn) recv(allowFD bool) (t byte, payload []byte, f *os.File, err error) {
	buf := make([]byte, ctlMaxMsg)
	oob := make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, flags, _, err := c.c.ReadMsgUnix(buf, oob)
	if oobn > 0 {
		if msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for i := range msgs {
				fds, ferr := syscall.ParseUnixRights(&msgs[i])
				if ferr != nil {
					continue
				}
				for _, fd := range fds {
					if allowFD && f == nil {
						_ = syscall.SetNonblock(fd, true) // so that os.File can poll it
						f = os.NewFile(uintptr(fd), "passed")
					} else {
						_ = syscall.Close(fd)
					}
				}
			}
		}
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		return 0, nil, nil, err
	}
	if n == 0 || flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 {
		if f != nil {
			f.Close()
		}
		return 0, nil, nil, errors.New("malformed control message")
	}
	return buf[0], buf[1:n], f, nil
}
