// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// input.go: from Gio's keys to the keys the emulator encodes.

package main

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"gioui.org/io/key"
	uv "github.com/charmbracelet/ultraviolet"
)

var specialKeys = map[key.Name]rune{
	key.NameUpArrow: uv.KeyUp, key.NameDownArrow: uv.KeyDown, key.NameLeftArrow: uv.KeyLeft, key.NameRightArrow: uv.KeyRight,
	key.NameHome: uv.KeyHome, key.NameEnd: uv.KeyEnd, key.NamePageUp: uv.KeyPgUp, key.NamePageDown: uv.KeyPgDown,
	key.NameDeleteForward: uv.KeyDelete, "Insert": uv.KeyInsert,
	key.NameReturn: uv.KeyEnter, key.NameEnter: uv.KeyEnter, key.NameEscape: uv.KeyEscape,
	key.NameDeleteBackward: uv.KeyBackspace, key.NameTab: uv.KeyTab, key.NameSpace: uv.KeySpace,
	key.NameF1: uv.KeyF1, key.NameF2: uv.KeyF2, key.NameF3: uv.KeyF3, key.NameF4: uv.KeyF4, key.NameF5: uv.KeyF5,
	key.NameF6: uv.KeyF6, key.NameF7: uv.KeyF7, key.NameF8: uv.KeyF8, key.NameF9: uv.KeyF9, key.NameF10: uv.KeyF10,
	key.NameF11: uv.KeyF11, key.NameF12: uv.KeyF12,
}

// keyEvent turns a key press that is not plain text into an emulator key. Plain
// text (no Ctrl, no Alt) arrives as key.EditEvent, so it returns false for it.
func keyEvent(name key.Name, mods key.Modifiers) (uv.KeyPressEvent, bool) {
	m := uvMods(mods)
	if code, ok := specialKeys[name]; ok {
		ev := uv.KeyPressEvent{Code: code, Mod: m}
		if code == uv.KeySpace {
			ev.Text = " "
		}
		if code == uv.KeyTab && mods&key.ModShift != 0 { // Shift-Tab = backtab
			ev.Mod = m &^ uv.ModShift
			ev.Code = uv.KeyTab
			ev.Mod |= uv.ModShift
		}
		if code == uv.KeySpace && mods&key.ModCtrl == 0 && mods&key.ModAlt == 0 {
			return ev, false // plain space comes as text
		}
		return ev, true
	}
	if mods&(key.ModCtrl|key.ModAlt) == 0 {
		return uv.KeyPressEvent{}, false
	}
	s := strings.ToLower(string(name))
	if utf8.RuneCountInString(s) != 1 {
		return uv.KeyPressEvent{}, false
	}
	r, _ := utf8.DecodeRuneInString(s)
	if !unicode.IsPrint(r) {
		return uv.KeyPressEvent{}, false
	}
	ev := uv.KeyPressEvent{Code: r, Mod: m &^ uv.ModShift}
	if mods&key.ModShift != 0 {
		ev.Mod |= uv.ModShift
	}
	if mods&key.ModCtrl == 0 { // Alt+x: ESC x
		ev.Text = string(r)
		if mods&key.ModShift != 0 {
			ev.Text = strings.ToUpper(ev.Text)
		}
	}
	return ev, true
}
