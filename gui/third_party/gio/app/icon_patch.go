// SPDX-License-Identifier: Unlicense OR MIT

// ptscry-gui patch: the window icon, for the platforms where the program
// itself sets it (X11). Windows takes it from the executable's resources,
// Wayland and macOS from the desktop entry and the application bundle.

package app

import (
	"image"
	"image/draw"
)

// WindowIcons are the images of the window icon (several sizes are fine).
var WindowIcons []image.Image

// iconCardinals is the _NET_WM_ICON data: width, height, then ARGB pixels,
// for every image.
func iconCardinals() []uint32 {
	var out []uint32
	for _, im := range WindowIcons {
		b := im.Bounds()
		rgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), im, b.Min, draw.Src)
		out = append(out, uint32(b.Dx()), uint32(b.Dy()))
		for i := 0; i+3 < len(rgba.Pix); i += 4 {
			p := rgba.Pix[i : i+4]
			out = append(out, uint32(p[3])<<24|uint32(p[0])<<16|uint32(p[1])<<8|uint32(p[2]))
		}
	}
	return out
}
