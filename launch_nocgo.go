// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// launch_nocgo.go: without cgo there is no session launcher (and no PAM).

//go:build linux && !cgo

package main

import "errors"

const launcherAvailable = false

func pamAvailable() error { return errors.New("this build has no PAM support (built without cgo)") }

type pamHandle struct{}

type pamError struct{ Stage, Msg string }

func (e *pamError) Error() string { return e.Stage + ": " + e.Msg }

func pamOpen(service, user, tty string) (*pamHandle, []string, error) {
	return nil, nil, &pamError{"load", "built without cgo"}
}

func (p *pamHandle) close() {}

func spawnUser(slaveFD int, uid, gid uint32, groups []uint32, dir string, argv, env []string, loginuid uint32) (int, error) {
	return 0, errors.New("built without cgo")
}
