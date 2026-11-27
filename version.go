// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// Persistent audited PTY sessions.

package main

import "fmt"

const (
	progName    = "ptscry"
	progVersion = "0.7.1"
	progCredits = "Guido Cauli <guido.cauli@gmail.com>"
	progTagline = "persistent audited PTY sessions"
)

// licenseLine goes with the version: the license and the absence of warranty.
const licenseLine = "Copyright (C) 2026 Guido Cauli. License AGPL-3.0-or-later: free software, WITHOUT ANY WARRANTY (see LICENSE and NOTICE.md)."

func versionLine() string {
	return fmt.Sprintf("%s - %s v%s (%s)", progName, progCredits, progVersion, progTagline)
}
