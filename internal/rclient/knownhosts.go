// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// knownhosts.go: server key verification for the remote client (trust on
// first use, then the key must not change), in client.conf's directory.

package rclient

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

func KnownHostsPath() string {
	d, err := os.UserConfigDir()
	if err != nil || d == "" {
		return ""
	}
	return filepath.Join(d, "ptscry", "known_hosts")
}

// HostKeyChecker returns the ssh.HostKeyCallback. pinned, if not empty, is a
// SHA256 fingerprint that must match (no file, no question). prompt asks the
// trust-on-first-use question (a terminal for the CLI, a dialog for the GUI).
func HostKeyChecker(pinned string, prompt Prompter) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if pinned != "" {
			if fp != pinned {
				return fmt.Errorf("server key %s does not match the fingerprint given with -F (%s)", fp, pinned)
			}
			return nil
		}
		path := KnownHostsPath()
		if path == "" {
			return errors.New("no configuration directory for known_hosts: use -F SHA256:...")
		}
		if known, ok := lookupKnownHost(path, hostname); ok {
			if known == fp {
				return nil
			}
			return fmt.Errorf("@@@ THE KEY OF %s HAS CHANGED @@@\n  known: %s\n  now:   %s\n"+
				"Somebody may be intercepting the connection, or the server was reinstalled.\n"+
				"If you are sure it is the server, remove its line from %s", hostname, known, fp, path)
		}
		q := fmt.Sprintf("The authenticity of %s cannot be established.\nKey type %s, fingerprint %s\nTrust this server and remember its key? (yes/no) ", hostname, key.Type(), fp)
		ans, err := prompt(q, false)
		if err != nil || strings.ToLower(strings.TrimSpace(ans)) != "yes" {
			return errors.New("server key not accepted (check the fingerprint with the administrator, or use -F)")
		}
		return rememberKnownHost(path, hostname, fp)
	}
}

func lookupKnownHost(path, host string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) == 2 && p[0] == host {
			return p[1], true
		}
	}
	return "", false
}

func rememberKnownHost(path, host, fp string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s\n", host, fp)
	return err
}
