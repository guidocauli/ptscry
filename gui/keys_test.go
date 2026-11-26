// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeKey(t *testing.T, dir, name, pass string) string {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_ = pub
	var blk *pem.Block
	var err error
	if pass == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(pass))
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(blk), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportKey(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	src := t.TempDir()
	k := writeKey(t, src, "id ed25519!", "")
	got, err := importKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "ssh-ed25519" || !strings.HasPrefix(got.Fingerprint, "SHA256:") || got.Encrypted {
		t.Fatalf("%+v", got)
	}
	st, err := os.Stat(got.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("copy: %v %v", st, err)
	}
	if strings.ContainsAny(got.Name, " !") {
		t.Fatalf("name %q", got.Name)
	}
	// the same file again gets another name
	got2, err := importKey(k)
	if err != nil || got2.Name == got.Name {
		t.Fatalf("second import: %+v %v", got2, err)
	}
	// with a passphrase: accepted, marked
	e, err := importKey(writeKey(t, src, "enc", "segreto"))
	if err != nil || !e.Encrypted || e.Fingerprint == "" {
		t.Fatalf("encrypted: %+v %v", e, err)
	}
	if n := len(listKeys()); n != 3 {
		t.Fatalf("listKeys = %d", n)
	}
	// a public key and a random file are refused
	pubf := filepath.Join(src, "id.pub")
	os.WriteFile(pubf, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl x\n"), 0o644)
	if _, err := importKey(pubf); err == nil || !strings.Contains(err.Error(), "PUBBLICA") {
		t.Fatalf("public key: %v", err)
	}
	junk := filepath.Join(src, "junk")
	os.WriteFile(junk, []byte("hello"), 0o644)
	if _, err := importKey(junk); err == nil {
		t.Fatal("junk accepted")
	}
	// removal stays inside the key directory
	if removeKey("../x") == nil || removeKey("") == nil {
		t.Fatal("bad names accepted")
	}
	if err := removeKey(got.Name); err != nil {
		t.Fatal(err)
	}
	if n := len(listKeys()); n != 2 {
		t.Fatalf("after remove: %d", n)
	}
}

func TestGenerateRenameAndPublic(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	k, err := generateKey("prova", "segreta")
	if err != nil || !k.Encrypted || k.Kind != "ssh-ed25519" {
		t.Fatalf("generate: %+v %v", k, err)
	}
	if _, err := generateKey("prova", ""); err == nil {
		t.Fatal("a second key with the same name was made")
	}
	line, err := publicKeyLine(k)
	if err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Fatalf("public: %q %v", line, err)
	}
	os.Remove(k.Path + ".pub") // derived from the private key, passphrase or not
	if l2, err := publicKeyLine(k); err != nil || strings.Fields(l2)[1] != strings.Fields(line)[1] {
		t.Fatalf("public from the private key: %q %v", l2, err)
	}
	n, err := renameKey("prova", "nuovo nome")
	if err != nil || n != "nuovo_nome" {
		t.Fatalf("rename: %q %v", n, err)
	}
	l := listKeys()
	if len(l) != 1 || l[0].Name != "nuovo_nome" || l[0].Fingerprint != k.Fingerprint {
		t.Fatalf("list after rename: %+v", l)
	}
}
