// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_seccomp_other.go: no seccomp filter on this platform.

//go:build linux && !amd64

package main

func installSeccomp() (bool, error) { return false, nil }
