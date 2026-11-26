//go:build !linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// main_other.go: outside Linux this program is the remote client only.

package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"help"}
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Println(versionLine())
		fmt.Println(licenseLine)
	case "help", "-h", "--help":
		fmt.Println(versionLine())
		fmt.Println(licenseLine)
		fmt.Print("\n", remoteUsage)
	default:
		os.Exit(remoteMain(args))
	}
}
