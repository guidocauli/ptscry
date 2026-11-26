// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeAgent answers the two requests the client makes.
func fakeAgent(t *testing.T, c net.Conn, signer ssh.Signer) {
	defer c.Close()
	for {
		var h [4]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint32(h[:]))
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		var reply []byte
		switch msg[0] {
		case agentRequestIdentities:
			reply = ssh.Marshal(struct {
				Type    byte
				Count   uint32
				Blob    []byte
				Comment string
			}{agentIdentitiesAnswer, 1, signer.PublicKey().Marshal(), "test"})
		case agentSignRequest:
			var req struct {
				Type  byte
				Key   []byte
				Data  []byte
				Flags uint32
			}
			if ssh.Unmarshal(msg, &req) != nil {
				reply = []byte{agentFailure}
				break
			}
			sig, err := signer.Sign(rand.Reader, req.Data)
			if err != nil {
				reply = []byte{agentFailure}
				break
			}
			reply = ssh.Marshal(struct {
				Type byte
				Sig  []byte
			}{agentSignResponse, ssh.Marshal(sig)})
		default:
			reply = []byte{agentFailure}
		}
		out := make([]byte, 4+len(reply))
		binary.BigEndian.PutUint32(out, uint32(len(reply)))
		copy(out[4:], reply)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func TestAgentClient(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	a, b := net.Pipe()
	go fakeAgent(t, b, signer)
	ag := &AgentClient{c: a}
	defer ag.Close()
	ss, err := ag.Signers()
	if err != nil || len(ss) != 1 {
		t.Fatalf("signers: %v %v", ss, err)
	}
	data := []byte("challenge")
	sig, err := ss[0].Sign(rand.Reader, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss[0].PublicKey().Verify(data, sig); err != nil {
		t.Fatalf("signature from the agent does not verify: %v", err)
	}
	if err := ss[0].PublicKey().Verify([]byte("other"), sig); err == nil {
		t.Fatal("verification must fail on other data")
	}
}

func TestParseTargetAndCmdline(t *testing.T) {
	for in, want := range map[string]Target{
		"ivan@h.example":      {"ivan", "h.example", "2222"},
		"ivan@h.example:2200": {"ivan", "h.example", "2200"},
		"ivan@[::1]:22":       {"ivan", "::1", "22"},
		"ivan@::1":            {"ivan", "::1", "2222"},
	} {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"ivan@h:0", "ivan@h:99999", "ivan@h:x", "ivan@"} {
		if _, err := ParseTarget(in); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
	if l, tty, n, err := Cmdline(nil); err != nil || l != "" || !tty || n != "main" {
		t.Errorf("default: %q %v %q %v", l, tty, n, err)
	}
	if l, tty, n, _ := Cmdline([]string{"new", "-A", "-n", "lav"}); l != "ptscry new -A -n lav" || !tty || n != "lav" {
		t.Errorf("new: %q %v %q", l, tty, n)
	}
	if _, tty, _, _ := Cmdline([]string{"new", "-d", "-n", "x"}); tty {
		t.Error("new -d needs no terminal")
	}
	if _, tty, n, _ := Cmdline([]string{"attach"}); !tty || n != "main" {
		t.Error("attach default")
	}
	for _, bad := range [][]string{{"list;id"}, {"new", "$(x)"}, {"admin", "list"}, {"bash"}, {"new", "a b"}} {
		if _, _, _, err := Cmdline(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	for in, want := range map[string]byte{"ctrl-b": 2, "Ctrl-B": 2, "C-a": 1, "^A": 1, "ctrl+]": 0x1d, "ctrl-\\": 0x1c, "ctrl-_": 0x1f, " ctrl-x ": 0x18} {
		if got, err := ParsePrefix(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "b", "ctrl-", "ctrl-bb", "ctrl-c", "ctrl-d", "ctrl-z", "ctrl-m", "ctrl-i", "ctrl-j", "ctrl-h", "ctrl-[", "ctrl-@", "ctrl-1", "alt-b"} {
		if _, err := ParsePrefix(in); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
	if PrefixName(2) != "Ctrl-B" || PrefixName(0x1d) != "Ctrl-]" {
		t.Errorf("names: %s %s", PrefixName(2), PrefixName(0x1d))
	}
}

func TestClientConfPrefix(t *testing.T) {
	d := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := w("user.conf", "# comment\nprefix = ctrl-a\n")
	sys := w("sys.conf", "prefix = ctrl-]\n")
	if cc, warn := ReadConf([]string{user, sys}, true); cc.Prefix != 1 || len(warn) != 0 {
		t.Fatalf("user file must win: %v %v", cc, warn)
	}
	if cc, _ := ReadConf([]string{filepath.Join(d, "none"), sys}, true); cc.Prefix != 0x1d {
		t.Fatalf("falls through to the next file: %v", cc)
	}
	srv := w("srv.conf", "server = ivan@host.example:2222\nidentity = /k/id\ncertificate = /k/id-cert.pub\nprefix = ctrl-a\n")
	if cc, warn := ReadConf([]string{srv, sys}, true); cc.Server != "ivan@host.example:2222" || cc.Identity != "/k/id" || cc.Cert != "/k/id-cert.pub" || cc.Prefix != 1 || len(warn) != 0 {
		t.Fatalf("server/identity/certificate: %+v %v", cc, warn)
	}
	bad := w("bad.conf", "prefix = ctrl-c\nfoo = bar\nnonsense\n")
	cc, warn := ReadConf([]string{bad}, true)
	if cc.Prefix != 0 || len(warn) != 3 {
		t.Fatalf("a bad file must not set anything and must warn: %v %v", cc, warn)
	}
	loose := w("loose.conf", "prefix = ctrl-a\n")
	if err := os.Chmod(loose, 0o666); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if cc, warn := ReadConf([]string{loose}, true); cc.Prefix != 0 || len(warn) != 1 {
			t.Fatalf("a world-writable file must be ignored: %v %v", cc, warn)
		}
	}
}

func TestDefaultKeySigners(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := defaultKeySigners(nil); len(got) != 0 {
		t.Fatalf("keys from nowhere: %d", len(got))
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultKeySigners(nil); len(got) != 1 {
		t.Fatalf("default key not found: %d", len(got))
	}
}
