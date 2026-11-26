//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// totp_cmd.go: enrol a user's TOTP secret (root only).

package main

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// cmdTotpInit: ptscry totp-init [-f] USER
// Creates /etc/ptscry/totp/USER (root:root 0600) and prints the secret and the
// otpauth:// URI for an authenticator app. It never overwrites without -f.
func cmdTotpInit(args []string) error {
	fs := flag.NewFlagSet("totp-init", flag.ContinueOnError)
	force := fs.Bool("f", false, "replace an existing secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !feUserRe.MatchString(fs.Arg(0)) {
		return errors.New("usage: ptscry totp-init [-f] USER")
	}
	if os.Getuid() != 0 {
		return errors.New("only root can enrol a TOTP secret")
	}
	cf, err := parseConfigFile(configPath())
	if err != nil {
		return err
	}
	g := cf.resolve("", nil)
	if g.FrontendTOTPDir == "none" {
		return errors.New("frontend_totp_dir is none")
	}
	if err := ensureRootDir(g.FrontendTOTPDir, 0o700); err != nil {
		return err
	}
	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	path := filepath.Join(g.FrontendTOTPDir, fs.Arg(0))
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s exists (use -f to replace it)", path)
		}
		return err
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	host, _ := os.Hostname()
	uri := "otpauth://totp/" + url.PathEscape("ptscry:"+fs.Arg(0)+"@"+host) +
		"?secret=" + secret + "&issuer=ptscry&algorithm=SHA1&digits=6&period=30"
	fmt.Printf("secret written to %s\nsecret: %s\n%s\n", path, secret, uri)
	return nil
}
