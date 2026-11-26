//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// setup_frontend.go: "ptscry setup frontend": turns the built-in SSH front-end
// on in one step: its account, the host key, the key directory, "frontend =
// yes" in the configuration (validated before it is written, the old file
// kept as .bak) and a reload of the running monitor.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var keyLineRe = regexp.MustCompile(`^\s*#?\s*([a-z_]+)\s*=`)

// setGlobalKey sets key = value in the [global] section of a configuration
// text: it replaces the line (also a commented one) or adds it right after the
// section header. Comments and everything else are kept.
func setGlobalKey(text, key, value string) string {
	lines := strings.Split(text, "\n")
	section, globalAt, done := "", -1, false
	var commented = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.ToLower(strings.Trim(t, "[] "))
			if section == "global" {
				globalAt = i
			}
			continue
		}
		if section != "global" && !(section == "" && globalAt < 0) {
			continue
		}
		m := keyLineRe.FindStringSubmatch(l)
		if m == nil || m[1] != key {
			continue
		}
		if strings.HasPrefix(t, "#") {
			if commented < 0 {
				commented = i
			}
			continue
		}
		lines[i] = key + " = " + value
		done = true
	}
	if done {
		return strings.Join(lines, "\n")
	}
	if commented >= 0 {
		lines[commented] = key + " = " + value
		return strings.Join(lines, "\n")
	}
	if globalAt < 0 {
		return "[global]\n" + key + " = " + value + "\n" + text
	}
	out := append([]string{}, lines[:globalAt+1]...)
	out = append(out, key+" = "+value)
	return strings.Join(append(out, lines[globalAt+1:]...), "\n")
}

func setupFrontend(w io.Writer) error {
	if err := needRoot("set up the front-end"); err != nil {
		return err
	}
	path := configPath()
	g, err := setupConfig()
	if err != nil {
		return err
	}
	// 1. the account the front-end runs as
	if _, err := lookupUserName(g.FrontendUser); err == nil {
		fmt.Fprintf(w, "account %s: already there\n", g.FrontendUser)
	} else {
		nologin := "/usr/sbin/nologin"
		if _, err := os.Stat(nologin); err != nil {
			nologin = "/sbin/nologin"
		}
		if out, err := exec.Command("useradd", "-r", "-M", "-s", nologin, "-c", "ptscry ssh front-end", g.FrontendUser).CombinedOutput(); err != nil {
			return fmt.Errorf("useradd %s: %v %s", g.FrontendUser, err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "account %s: created (no login shell)\n", g.FrontendUser)
	}
	// 2. the host key
	if _, err := os.Stat(g.FrontendHostKey); err == nil {
		fmt.Fprintf(w, "host key: already there (%s)\n", g.FrontendHostKey)
	} else {
		if err := ensureRootDir("/etc/ptscry", 0o755); err != nil {
			return err
		}
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ptscry front-end", "-f", g.FrontendHostKey).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v %s (is openssh-client installed?)", err, strings.TrimSpace(string(out)))
		}
		_ = os.Chmod(g.FrontendHostKey, 0o600)
		fp, _ := exec.Command("ssh-keygen", "-lf", g.FrontendHostKey+".pub").Output()
		fmt.Fprintf(w, "host key: created, fingerprint %s\n", strings.Fields(string(fp) + " ? ?")[1])
	}
	// 3. the directory of the users' public keys
	if g.FrontendKeysDir != "none" {
		if err := ensureRootDir(g.FrontendKeysDir, 0o755); err != nil {
			return err
		}
	}
	// 4. frontend = yes
	if g.Frontend {
		fmt.Fprintf(w, "%s: frontend = yes already\n", path)
	} else {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		next := setGlobalKey(string(raw), "frontend", "yes")
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(next), 0o644); err != nil {
			return err
		}
		cf, err := parseConfigFile(tmp)
		if err == nil && !cf.resolve("", nil).Frontend {
			err = errors.New("frontend still off after the change")
		}
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("could not turn the front-end on in %s: %v (set \"frontend = yes\" in [global] by hand)", path, err)
		}
		_ = os.WriteFile(path+".bak", raw, 0o644)
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: frontend = yes (previous file: %s.bak)\n", path, path)
	}
	// 5. the running monitor rereads its configuration
	if out, err := exec.Command("systemctl", "is-active", "ptscry-monitor").Output(); err == nil && strings.TrimSpace(string(out)) == "active" {
		if out, err := exec.Command("systemctl", "reload", "ptscry-monitor").CombinedOutput(); err != nil {
			fmt.Fprintf(w, "systemctl reload ptscry-monitor: %v %s\n", err, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintln(w, "monitor: configuration reloaded, the front-end starts now")
		}
	} else {
		fmt.Fprintln(w, "monitor: not running; start it with: systemctl enable --now ptscry-monitor")
	}
	cf, _ := parseConfigFile(path)
	listen := g.FrontendListen
	if cf != nil {
		listen = cf.resolve("", nil).FrontendListen
	}
	fmt.Fprintf(w, "\nThe front-end listens on %s. Open that port in the firewall, for example:\n", listen)
	port := listen[strings.LastIndexByte(listen, ':')+1:]
	fmt.Fprintf(w, "  firewall-cmd --add-port=%s/tcp --permanent && firewall-cmd --reload    (RHEL/Alma)\n", port)
	fmt.Fprintf(w, "  ufw allow %s/tcp                                                    (Ubuntu)\n", port)
	fmt.Fprintln(w, "Then give each user a public key: ptscry setup keys add USER FILE   (and check: ptscry check -u USER)")
	return nil
}
