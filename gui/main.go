// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// main.go: entry point of the graphical session multiplexer.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	gioapp "gioui.org/app"
)

const guiUsage = `ptscry-gui - Guido Cauli <guido.cauli@gmail.com>

Uso: ptscry-gui [-H [utente@]host[:porta]] [-s sessione] [-version]

    -H   apri subito un tab su questo server (altrimenti: il profilo predefinito,
         "locale" = 127.0.0.1:2222)
    -s   nome della sessione del primo tab (se esiste ci si riattacca)
`

func main() {
	fs := flag.NewFlagSet("ptscry-gui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := fs.String("H", "", "")
	sess := fs.String("s", "", "")
	ver := fs.Bool("version", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprint(os.Stderr, guiUsage)
		os.Exit(2)
	}
	if *ver {
		fmt.Printf("ptscry-gui %s - Guido Cauli <guido.cauli@gmail.com>\n", guiVersion)
		fmt.Println("Copyright (C) 2026 Guido Cauli. License AGPL-3.0-or-later: free software, WITHOUT ANY WARRANTY (see LICENSE and NOTICE.md).")
		return
	}
	startLog()
	setWindowIcon()
	gioapp.ID = "ptscry-gui" // Wayland app_id / X11 class: matches ptscry-gui.desktop and its icon
	set, err := loadSettings()
	g := newGUI(set)
	if err != nil {
		g.flash("%v", err)
	}
	go guard("window", func() {
		w := new(gioapp.Window)
		w.Option(g.windowOptions()...)
		g.win = w
		// first tabs: -H wins over the saved workspace
		switch {
		case *host != "":
			g.addRemoteProfile(*host)
			g.openTab(*host, *sess)
		case set.Restore && len(set.Tabs) > 0 && *sess == "":
			for _, t := range set.Tabs {
				g.openSpec(specOf(t))
			}
			g.active = min(max(set.Active, 0), len(g.tabs)-1)
		case *sess != "" || set.StartTab:
			g.openTab("", *sess)
		default:
			g.home = true // the welcome page, as in RexDesk
		}
		if err := g.run(w); err != nil {
			fmt.Fprintln(os.Stderr, "ptscry-gui:", err)
			os.Exit(1)
		}
		os.Exit(0)
	})
	gioapp.Main()
}

func (g *gui) addRemoteProfile(target string) {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	if g.set.profile(target) == nil {
		g.set.Profiles = append(g.set.Profiles, profile{Name: target, Mode: "remote", Target: target})
	}
}
