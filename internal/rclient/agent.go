// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// agent_client.go: a minimal ssh-agent client (identities and signatures
// only), so that FIDO2 keys (sk-ssh-ed25519) and certificates held by the
// system agent can be used without cgo and without libfido2.

package rclient

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	agentRequestIdentities = 11
	agentIdentitiesAnswer  = 12
	agentSignRequest       = 13
	agentSignResponse      = 14
	agentFailure           = 5
	agentMaxMessage        = 256 << 10
)

type AgentClient struct {
	mu sync.Mutex
	c  io.ReadWriteCloser
}

func (a *AgentClient) Close() error { return a.c.Close() }

// call sends one request and returns the reply payload.
func (a *AgentClient) call(req []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dc, ok := a.c.(interface{ SetDeadline(time.Time) error }); ok {
		_ = dc.SetDeadline(time.Now().Add(60 * time.Second)) // a touch on the key may be needed
	}
	buf := make([]byte, 4+len(req))
	binary.BigEndian.PutUint32(buf, uint32(len(req)))
	copy(buf[4:], req)
	if _, err := a.c.Write(buf); err != nil {
		return nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(a.c, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > agentMaxMessage {
		return nil, errors.New("agent: bad reply size")
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(a.c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// signers lists the agent's identities as ssh.Signers.
func (a *AgentClient) Signers() ([]ssh.Signer, error) {
	resp, err := a.call([]byte{agentRequestIdentities})
	if err != nil {
		return nil, err
	}
	if resp[0] != agentIdentitiesAnswer {
		return nil, errors.New("agent refused to list the keys")
	}
	var msg struct {
		Count uint32
		Rest  []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(resp[1:], &msg); err != nil || msg.Count > 1024 {
		return nil, errors.New("agent: bad identities reply")
	}
	var out []ssh.Signer
	rest := msg.Rest
	for i := uint32(0); i < msg.Count; i++ {
		var k struct {
			Blob, Comment []byte
			Rest          []byte `ssh:"rest"`
		}
		if err := ssh.Unmarshal(rest, &k); err != nil {
			return nil, errors.New("agent: bad identities reply")
		}
		rest = k.Rest
		pub, err := ssh.ParsePublicKey(k.Blob)
		if err != nil { // a key type we do not know: skip it
			continue
		}
		out = append(out, &agentSigner{a: a, pub: pub})
	}
	return out, nil
}

type agentSigner struct {
	a   *AgentClient
	pub ssh.PublicKey
}

func (s *agentSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s *agentSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	req := ssh.Marshal(struct {
		Type  byte
		Key   []byte
		Data  []byte
		Flags uint32
	}{agentSignRequest, s.pub.Marshal(), data, 0})
	resp, err := s.a.call(req)
	if err != nil {
		return nil, err
	}
	if resp[0] == agentFailure {
		return nil, errors.New("agent refused to sign")
	}
	if resp[0] != agentSignResponse {
		return nil, fmt.Errorf("agent: unexpected reply %d", resp[0])
	}
	var w struct{ Sig []byte }
	if err := ssh.Unmarshal(resp[1:], &w); err != nil {
		return nil, err
	}
	sig := new(ssh.Signature)
	if err := ssh.Unmarshal(w.Sig, sig); err != nil {
		return nil, err
	}
	return sig, nil
}

// DialAgent connects to the platform's agent (agent_unix.go, agent_windows.go).
func DialAgent() (*AgentClient, error) {
	c, err := agentDial()
	if err != nil {
		return nil, err
	}
	return &AgentClient{c: c}, nil
}
