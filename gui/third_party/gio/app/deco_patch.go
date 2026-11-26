// SPDX-License-Identifier: Unlicense OR MIT

// ptscry-gui patch: who draws the title bar on Wayland. Gio only listens to
// the compositor's preference; with xdg-decoration the program may ask for
// one. Asking for client-side decorations means "no bar from the compositor":
// with Decorated(true) Gio then draws its own bar, with Decorated(false) there
// is none. Some compositors (Niri with prefer-no-csd) answer server-side and
// draw nothing: asking for client-side is the only way to get a bar there.

package app

const (
	DecorationAuto   = 0 // the compositor decides
	DecorationClient = 1 // the program (Gio, or nobody)
	DecorationServer = 2 // the compositor
)

// InitialDecorationMode is asked for when the window is created.
var InitialDecorationMode = DecorationAuto

// SetDecorationMode asks the compositor for a decoration mode now (Wayland
// with xdg-decoration; elsewhere it does nothing).
func (w *Window) SetDecorationMode(m int) {
	if w.driver == nil {
		InitialDecorationMode = m
		return
	}
	w.Run(func() {
		if d, ok := w.driver.(interface{ setDecorationMode(int) }); ok {
			d.setDecorationMode(m)
		}
	})
}
