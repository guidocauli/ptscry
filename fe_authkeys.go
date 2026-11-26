//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_authkeys.go: authorized_keys lines with options, for the SSH front-end.
//
// Supported options: from="...", expiry-time="...", pty / no-pty, restrict,
// no-port-forwarding, no-X11-forwarding, no-agent-forwarding, no-user-rc,
// verify-required. Anything else (command=, environment=, principals=,
// cert-authority, no-touch-required, positive forwarding options...) makes the
// whole line unusable: a restriction that cannot be honoured must never be
// silently dropped, so such a line grants nothing and `ptscry check` reports it.

package main

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type authKey struct {
	key    ssh.PublicKey
	from   string    // from="" pattern list (addresses only), empty = anywhere
	noPTY  bool      // no terminal (no-pty, or restrict without pty)
	expiry time.Time // zero = none
}

var fromPatternRe = regexp.MustCompile(`^!?[0-9A-Fa-f:.*?/]+$`)

// parseAuthLine parses one authorized_keys line.
func parseAuthLine(line string) (authKey, error) {
	var ak authKey
	k, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ak, err
	}
	ak.key = k
	restrict, pty := false, false
	for _, o := range opts {
		name, val, hasVal := strings.Cut(o, "=")
		val = strings.TrimSuffix(strings.TrimPrefix(val, `"`), `"`)
		switch strings.ToLower(name) {
		case "from":
			if !hasVal || val == "" {
				return ak, errors.New("empty from=")
			}
			for _, p := range strings.Split(val, ",") {
				if !fromPatternRe.MatchString(p) { // host names need reverse DNS: not supported
					return ak, fmt.Errorf("from= pattern %q is not an address pattern", p)
				}
			}
			ak.from = val
		case "expiry-time":
			t, err := parseExpiry(val)
			if err != nil {
				return ak, err
			}
			ak.expiry = t
		case "pty":
			pty = true
		case "no-pty":
			ak.noPTY = true
		case "restrict":
			restrict = true
		case "no-port-forwarding", "no-x11-forwarding", "no-agent-forwarding", "no-user-rc", "verify-required":
			// always true here (or enforced elsewhere)
		default:
			return ak, fmt.Errorf("option %q is not supported", name)
		}
	}
	if restrict && !pty {
		ak.noPTY = true
	}
	return ak, nil
}

func parseExpiry(v string) (time.Time, error) {
	loc := time.Local
	if strings.HasSuffix(v, "Z") {
		v, loc = strings.TrimSuffix(v, "Z"), time.UTC
	}
	for _, layout := range []string{"20060102", "200601021504", "20060102150405"} {
		if len(v) == len(layout) {
			if t, err := time.ParseInLocation(layout, v, loc); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid expiry-time %q", v)
}

// fromAllowed implements from="" for address patterns: a negated match denies;
// otherwise at least one positive pattern must match.
func fromAllowed(list, ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	allowed := false
	for _, p := range strings.Split(list, ",") {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		var m bool
		if strings.Contains(p, "/") {
			if _, n, err := net.ParseCIDR(p); err == nil {
				m = n.Contains(addr)
			}
		} else if ok, err := path.Match(p, addr.String()); err == nil && ok {
			m = true
		}
		if m {
			if neg {
				return false
			}
			allowed = true
		}
	}
	return allowed
}
