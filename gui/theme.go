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
	accent     color.NRGBA
	warn       color.NRGBA
	disabled   color.NRGBA // text of what cannot be used now: grey, still readable
}

var theme = themeColors{
	fg:         color.NRGBA{R: 0xd8, G: 0xdc, B: 0xe0, A: 0xff},
	bg:         color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xff},
	cursor:     color.NRGBA{R: 0xe6, G: 0xb4, B: 0x50, A: 0xff},
	selection:  color.NRGBA{R: 0x2f, G: 0x54, B: 0x8c, A: 0xff},
	chrome:     color.NRGBA{R: 0x1d, G: 0x20, B: 0x26, A: 0xff},
	chromeText: color.NRGBA{R: 0xb4, G: 0xba, B: 0xc4, A: 0xff},
	tabActive:  color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xff},
	accent:     color.NRGBA{R: 0xe6, G: 0xb4, B: 0x50, A: 0xff},
	warn:       color.NRGBA{R: 0xe0, G: 0x6c, B: 0x60, A: 0xff},
	disabled:   color.NRGBA{R: 0x80, G: 0x87, B: 0x92, A: 0xff},
}
