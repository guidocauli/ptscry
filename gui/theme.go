// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// theme.go: colours of the window and of the terminal.

package main

import "image/color"

type themeColors struct {
	fg, bg     color.NRGBA // terminal default text and background
	cursor     color.NRGBA
	selection  color.NRGBA // background of selected text
	chrome     color.NRGBA // tab bar and dialogs
	chromeText color.NRGBA
	tabActive  color.NRGBA
	accent     color.NRGBA // buttons, headings, the active tab: the blue of RexDesk
	accentFg   color.NRGBA // text on the accent
	input      color.NRGBA // text fields, secondary buttons
	hover      color.NRGBA // a row or button under the pointer
	line       color.NRGBA // separators between the parts of the window
	ok         color.NRGBA // connected
	busy       color.NRGBA // connecting
	warn       color.NRGBA
	disabled   color.NRGBA // text of what cannot be used now: grey, still readable
}

var theme = themeColors{
	fg:        color.NRGBA{R: 0xd8, G: 0xdc, B: 0xe0, A: 0xff},
	bg:        color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xff},
	cursor:    color.NRGBA{R: 0xe6, G: 0xb4, B: 0x50, A: 0xff},
	selection: color.NRGBA{R: 0x2f, G: 0x54, B: 0x8c, A: 0xff},
	// the window around the terminals: the dark palette of RexDesk (Fyne)
	chrome:     color.NRGBA{R: 0x17, G: 0x17, B: 0x18, A: 0xff},
	chromeText: color.NRGBA{R: 0xc8, G: 0xc8, B: 0xcc, A: 0xff},
	tabActive:  color.NRGBA{R: 0x17, G: 0x17, B: 0x18, A: 0xff},
	accent:     color.NRGBA{R: 0x29, G: 0x6f, B: 0xf6, A: 0xff},
	accentFg:   color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff},
	input:      color.NRGBA{R: 0x20, G: 0x20, B: 0x23, A: 0xff},
	hover:      color.NRGBA{R: 0x2c, G: 0x2c, B: 0x30, A: 0xff},
	line:       color.NRGBA{R: 0x2b, G: 0x2b, B: 0x2e, A: 0xff},
	ok:         color.NRGBA{R: 0x4c, G: 0xc0, B: 0x5a, A: 0xff},
	busy:       color.NRGBA{R: 0xf0, G: 0x9a, B: 0x30, A: 0xff},
	warn:       color.NRGBA{R: 0xe0, G: 0x6c, B: 0x60, A: 0xff},
	disabled:   color.NRGBA{R: 0x80, G: 0x87, B: 0x92, A: 0xff},
}
