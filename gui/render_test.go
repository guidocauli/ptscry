// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"image"
	"image/png"
	"os"
	"testing"

	"gioui.org/font/gofont"
	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
)

// shot renders a termView to a PNG in dir (when GUI_SHOTS is set): the way to
// look at the result without a display.
func renderTo(t *testing.T, tv *termView, w, h int, name string) *image.RGBA {
	t.Helper()
	win, err := headless.NewWindow(w, h)
	if err != nil {
		t.Skipf("no headless GL here (try EGL_PLATFORM=surfaceless): %v", err)
	}
	defer win.Release()
	var ops op.Ops
	gtx := layout.Context{Ops: &ops, Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(w, h))}
	tv.Layout(gtx, false)
	if err := win.Frame(&ops); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := win.Screenshot(img); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("GUI_SHOTS"); dir != "" {
		f, _ := os.Create(dir + "/" + name + ".png")
		_ = png.Encode(f, img)
		f.Close()
	}
	return img
}

func TestRenderSample(t *testing.T) {
	shaper := text.NewShaper(text.NoSystemFonts(), text.WithCollection(gofont.Collection()))
	m := newTermModel(10, 5, 1000, func([]byte) {})
	defer m.Close()
	tv := newTermView(m, shaper, 14)
	tv.input = func([]byte) {}
	renderTo(t, tv, 640, 220, "empty") // the first layout gives the terminal its real size
	m.Feed([]byte("\x1b[1;32mguido@srv\x1b[0m:\x1b[1;34m~/ptscry\x1b[0m$ ls --color\r\n"))
	m.Feed([]byte("\x1b[34mdocs\x1b[0m  \x1b[32mptscry\x1b[0m  README.md  \x1b[7mselected\x1b[0m  \x1b[4munder\x1b[0m\r\n"))
	m.Feed([]byte("┌──────┐ 世界 café ✓\r\n└──────┘ \x1b[38;2;230;180;80mtruecolor\x1b[0m \x1b[48;5;196m red bg \x1b[0m\r\n"))
	m.Feed([]byte("╔═══╦═══╗ ┏━━┳━━┓ ╭──╮ ▁▂▃▄▅▆▇█ ░▒▓ ▀▄▌▐\r\n║ a ║ b ║ ┃ a┃ b┃ │  │\r\n╚═══╩═══╝ ┗━━┻━━┛ ╰──╯ ▖▗▘▙▚▛▜▝▞▟\r\n$ "))
	img := renderTo(t, tv, 640, 220, "sample")
	// the background must be the theme's and some pixels must be text
	if c := img.RGBAAt(639, 219); c.R != theme.bg.R || c.G != theme.bg.G {
		t.Fatalf("background pixel %v", c)
	}
	lit := 0
	for y := 0; y < 220; y++ {
		for x := 0; x < 640; x++ {
			if c := img.RGBAAt(x, y); c.R > 150 && c.G > 150 {
				lit++
			}
		}
	}
	if lit < 200 {
		t.Fatalf("hardly any text was drawn (%d lit pixels)", lit)
	}
	cols, rows := m.Size()
	if cols < 60 || rows < 8 {
		t.Fatalf("the terminal did not grow to the window: %dx%d", cols, rows)
	}
}

// Regression: measuring at a scale other than 1 must not change the size in dp,
// or the font grows at every frame on HiDPI screens.
func TestMeasureKeepsDpSize(t *testing.T) {
	shaper := text.NewShaper(text.NoSystemFonts(), text.WithCollection(gofont.Collection()))
	m := newTermModel(80, 24, 100, func([]byte) {})
	defer m.Close()
	tv := newTermView(m, shaper, 14)
	tv.measure(28)
	tv.measure(28)
	if tv.fontPx != 14 {
		t.Fatalf("fontPx = %v after measuring, want 14 (dp)", tv.fontPx)
	}
	if tv.cellW < 14 || tv.cellW > 20 {
		t.Fatalf("cell width %d does not match a 28px font", tv.cellW)
	}
}
