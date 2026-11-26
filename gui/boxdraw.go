// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// boxdraw.go: box-drawing and block characters drawn as geometry, so that
// lines and blocks join exactly from cell to cell whatever the font is.

package main

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// isBoxOrBlock says whether the terminal draws r itself.
func isBoxOrBlock(r rune) bool { return (r >= 0x2500 && r <= 0x257f) || (r >= 0x2580 && r <= 0x259f) }

// drawBox draws r in the cell whose top-left corner is (x, y) and size w x h.
func drawBox(gtx layout.Context, r rune, x, y, w, h int, fg color.NRGBA) {
	fill := func(x0, y0, x1, y1 int, c color.NRGBA) {
		if x1 > x0 && y1 > y0 {
			paint.FillShape(gtx.Ops, c, clip.Rect(image.Rect(x0, y0, x1, y1)).Op())
		}
	}
	if r >= 0x2580 {
		drawBlock(r, x, y, w, h, fg, fill)
		return
	}
	arms := boxArms[r-0x2500]
	light := max(1, h/14)
	heavy := light * 2
	cx, cy := x+w/2, y+h/2
	gap := light + max(1, light/2) // distance between the two lines of a double
	// the end of each arm is stretched by half the thickness of the crossing line
	// so that corners are filled
	thick := func(a uint8) int {
		switch a {
		case 2:
			return heavy
		case 3:
			return gap*2 + light
		}
		return light
	}
	maxV := max(thick(arms[2]), thick(arms[3]))
	maxH := max(thick(arms[0]), thick(arms[1]))
	for dir, a := range arms {
		if a == 0 {
			continue
		}
		var t int
		switch a {
		case 1:
			t = light
		case 2:
			t = heavy
		}
		hz := dir < 2 // left, right
		draw := func(off, th int) {
			if hz {
				y0 := cy - th/2 + off
				if dir == 0 {
					fill(x, y0, cx+maxV/2+(maxV%2), y0+th, fg)
				} else {
					fill(cx-maxV/2, y0, x+w, y0+th, fg)
				}
			} else {
				x0 := cx - th/2 + off
				if dir == 2 {
					fill(x0, y, x0+th, cy+maxH/2+(maxH%2), fg)
				} else {
					fill(x0, cy-maxH/2, x0+th, y+h, fg)
				}
			}
		}
		if a == 3 {
			draw(-gap, light)
			draw(gap, light)
		} else {
			draw(0, t)
		}
	}
}

// drawBlock draws U+2580..U+259F.
func drawBlock(r rune, x, y, w, h int, fg color.NRGBA, fill func(x0, y0, x1, y1 int, c color.NRGBA)) {
	px := func(n, of, size int) int { return n * size / of } // n eighths
	switch {
	case r == 0x2580: // upper half
		fill(x, y, x+w, y+h/2, fg)
	case r >= 0x2581 && r <= 0x2588: // lower 1/8 .. full
		n := int(r - 0x2580)
		fill(x, y+h-px(n, 8, h), x+w, y+h, fg)
	case r >= 0x2589 && r <= 0x258f: // left 7/8 .. 1/8
		n := 8 - int(r-0x2589) - 1
		fill(x, y, x+px(n, 8, w), y+h, fg)
	case r == 0x2590: // right half
		fill(x+w/2, y, x+w, y+h, fg)
	case r >= 0x2591 && r <= 0x2593: // light, medium, dark shade
		a := []uint8{0x40, 0x80, 0xc0}[r-0x2591]
		c := fg
		c.A = a
		fill(x, y, x+w, y+h, color.NRGBA{R: uint8(uint16(c.R) * uint16(a) / 255), G: uint8(uint16(c.G) * uint16(a) / 255), B: uint8(uint16(c.B) * uint16(a) / 255), A: a})
	case r == 0x2594: // upper 1/8
		fill(x, y, x+w, y+max(1, h/8), fg)
	case r == 0x2595: // right 1/8
		fill(x+w-max(1, w/8), y, x+w, y+h, fg)
	case r >= 0x2596 && r <= 0x259f: // quadrants: UL, UR, LL, LR
		const (
			ul = 1 << iota
			ur
			ll
			lr
		)
		q := map[rune]int{0x2596: ll, 0x2597: lr, 0x2598: ul, 0x2599: ul | ll | lr, 0x259a: ul | lr, 0x259b: ul | ur | ll,
			0x259c: ul | ur | lr, 0x259d: ur, 0x259e: ur | ll, 0x259f: ur | ll | lr}[r]
		hw, hh := w/2, h/2
		if q&ul != 0 {
			fill(x, y, x+hw, y+hh, fg)
		}
		if q&ur != 0 {
			fill(x+hw, y, x+w, y+hh, fg)
		}
		if q&ll != 0 {
			fill(x, y+hh, x+hw, y+h, fg)
		}
		if q&lr != 0 {
			fill(x+hw, y+hh, x+w, y+h, fg)
		}
	}
}
