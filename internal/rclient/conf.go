// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// conf.go: the key that introduces a command while attached (like tmux's
// prefix) and the small client configuration file that can change it.

package rclient

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultPrefix is Ctrl-B, as in tmux.
const DefaultPrefix byte = 0x02

// ParsePrefix reads "ctrl-b", "C-b", "^B" or "ctrl-]" (case-insensitive).
// Only control characters are allowed, and the ones that would make a session
// unusable (interrupt, EOF, suspend, Enter, Tab, Backspace, Escape, NUL) are
// refused.
func ParsePrefix(s string) (byte, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(v, "ctrl-"):
		v = v[5:]
	case strings.HasPrefix(v, "ctrl+"):
		v = v[5:]
	case strings.HasPrefix(v, "c-"):
		v = v[2:]
	case strings.HasPrefix(v, "^"):
		v = v[1:]
	default:
		return 0, fmt.Errorf("%q: write it as ctrl-b, C-b or ^B", s)
	}
	if len(v) != 1 {
		return 0, fmt.Errorf("%q: one character after ctrl-", s)
	}
	c := v[0]
	var b byte
	switch {
	case c >= 'a' && c <= 'z':
		b = c - 'a' + 1
	case c == '[':
		b = 0x1b
	case c == '\\':
		b = 0x1c
	case c == ']':
		b = 0x1d
	case c == '^':
		b = 0x1e
	case c == '_':
		b = 0x1f
	case c == '@' || c == ' ':
		b = 0
	default:
		return 0, fmt.Errorf("%q: not a control key", s)
	}
	if !ValidPrefixByte(b) {
		return 0, fmt.Errorf("%q is reserved (interrupt, EOF, suspend, Enter, Tab, Backspace, Escape): choose another", s)
	}
	return b, nil
}

// ValidPrefixByte says whether b may be a prefix (also checked by the server
// on the byte a client announces).
func ValidPrefixByte(b byte) bool {
	if b < 1 || b > 0x1f {
		return false
	}
	switch b {
	case 0x03, 0x04, 0x08, 0x09, 0x0a, 0x0d, 0x1a, 0x1b:
		return false
	}
	return true
}

// PrefixName is the printable form, for messages.
func PrefixName(b byte) string {
	switch {
	case b >= 1 && b <= 26:
		return "Ctrl-" + strings.ToUpper(string(rune('a'+b-1)))
	case b >= 0x1b && b <= 0x1f:
		return "Ctrl-" + string(rune('['+(b-0x1b)))
	}
	return fmt.Sprintf("0x%02x", b)
}

// ConfigPaths lists the client.conf files, most specific first: the
// user's own, then (Linux only) the system-wide one.
func ConfigPaths() []string {
	var p []string
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		p = append(p, filepath.Join(d, "ptscry", "client.conf"))
	}
	if runtime.GOOS == "linux" {
		p = append(p, "/etc/ptscry/client.conf")
	}
	return p
}

// Conf is the content of client.conf.
type Conf struct {
	Prefix   byte   // 0 = not set
	Server   string // [user@]host[:port] of the ptscry front-end
	Identity string // private key file (otherwise ssh-agent)
	Cert     string // SSH certificate file
	Status   string // ptscry-tui: "on" / "off" (the status bar), "" = not set
	Mouse    string // ptscry-tui: "on" / "off", "" = not set
}

// ReadConf reads the client.conf files, most specific first; for each
// key the first file that sets it wins. A missing file is normal. A broken or
// untrusted one is skipped with a warning, so that the defaults always work.
func ReadConf(paths []string, checkOwner bool) (cc Conf, warn []string) {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if !os.IsNotExist(err) {
				warn = append(warn, fmt.Sprintf("%s: %v", p, err))
			}
			continue
		}
		fi, err := f.Stat()
		if err == nil && checkOwner {
			err = checkConfFile(fi)
		}
		if err != nil {
			f.Close()
			warn = append(warn, fmt.Sprintf("%s: ignored: %v", p, err))
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), 16<<10)
		n := 0
		for sc.Scan() {
			n++
			if n > 200 {
				break
			}
			ln := strings.TrimSpace(sc.Text())
			if ln == "" || ln[0] == '#' || ln[0] == ';' {
				continue
			}
			k, v, ok := strings.Cut(ln, "=")
			if !ok {
				warn = append(warn, fmt.Sprintf("%s:%d: expected key = value", p, n))
				continue
			}
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			switch k {
			case "prefix":
				pb, err := ParsePrefix(v)
				if err != nil {
					warn = append(warn, fmt.Sprintf("%s:%d: %v", p, n, err))
				} else if cc.Prefix == 0 {
					cc.Prefix = pb
				}
			case "server", "identity", "certificate":
				if v == "" || len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") {
					warn = append(warn, fmt.Sprintf("%s:%d: invalid value for %s", p, n, k))
					continue
				}
				dst := &cc.Server
				if k == "identity" {
					dst = &cc.Identity
				} else if k == "certificate" {
					dst = &cc.Cert
				}
				if *dst == "" {
					*dst = v
				}
			case "status", "mouse":
				v = strings.ToLower(v)
				if v != "on" && v != "off" {
					warn = append(warn, fmt.Sprintf("%s:%d: %s wants on or off", p, n, k))
					continue
				}
				dst := &cc.Status
				if k == "mouse" {
					dst = &cc.Mouse
				}
				if *dst == "" {
					*dst = v
				}
			default:
				warn = append(warn, fmt.Sprintf("%s:%d: unknown key %q", p, n, k))
			}
		}
		f.Close()
	}
	return cc, warn
}
