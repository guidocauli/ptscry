// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// colors.go: the 16 ANSI colours (readable on the dark background), the 256
// colour cube and the translation of the emulator's colours.

package main

import (
	"image/color"

	"github.com/charmbracelet/x/ansi"
)

var ansi16 = [16]color.NRGBA{
	{0x2b, 0x2e, 0x33, 0xff}, {0xe0, 0x6c, 0x75, 0xff}, {0x98, 0xc3, 0x79, 0xff}, {0xe5, 0xc0, 0x7b, 0xff},
	{0x61, 0xaf, 0xef, 0xff}, {0xc6, 0x78, 0xdd, 0xff}, {0x56, 0xb6, 0xc2, 0xff}, {0xab, 0xb2, 0xbf, 0xff},
	{0x5c, 0x63, 0x70, 0xff}, {0xff, 0x7b, 0x86, 0xff}, {0xb5, 0xe8, 0x90, 0xff}, {0xf0, 0xd0, 0x8c, 0xff},
	{0x7e, 0xc0, 0xff, 0xff}, {0xd6, 0x8e, 0xf0, 0xff}, {0x6f, 0xcd, 0xd8, 0xff}, {0xff, 0xff, 0xff, 0xff},
}

func indexed(i int) color.NRGBA {
	switch {
	case i < 16:
		return ansi16[i]
	case i < 232:
		i -= 16
		lv := func(n int) uint8 {
			if n == 0 {
				return 0
			}
			return uint8(55 + 40*n)
		}
		return color.NRGBA{R: lv(i / 36), G: lv(i / 6 % 6), B: lv(i % 6), A: 0xff}
	}
	g := uint8(8 + 10*(i-232))
	return color.NRGBA{R: g, G: g, B: g, A: 0xff}
}

// toNRGBA translates a colour of the emulator; nil means def (the theme's).
func toNRGBA(c color.Color, def color.NRGBA) color.NRGBA {
	switch v := c.(type) {
	case nil:
		return def
	case ansi.BasicColor:
		return indexed(int(v))
	case ansi.IndexedColor:
		return indexed(int(v))
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return def
	}
	return color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff}
}
