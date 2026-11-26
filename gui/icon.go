// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// icon.go: the window icon embedded in the program (X11 shows it in the title
// bar and in Alt+Tab; elsewhere it comes from the executable or the bundle).

package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"

	gioapp "gioui.org/app"
)

var (
	//go:embed assets/ptscry-gui-32.png
	icon32 []byte
	//go:embed assets/ptscry-gui-64.png
	icon64 []byte
	//go:embed assets/ptscry-gui-128.png
	icon128 []byte
)

func setWindowIcon() {
	for _, b := range [][]byte{icon32, icon64, icon128} {
		if im, err := png.Decode(bytes.NewReader(b)); err == nil {
			gioapp.WindowIcons = append(gioapp.WindowIcons, image.Image(im))
		}
	}
}
