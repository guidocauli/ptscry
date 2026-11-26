// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// keysdlg.go: the "Chiavi private" dialog: a searchable list of the keys with
// the actions on the selected one (use it, copy its public key, rename,
// remove), import from a file and generation of a new key. Opened from the
// "Nuova sessione" dialog it shows over it and goes back to it.

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gioui.org/font"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/crypto/ssh"
)

// publicKeyLine is the authorized_keys line of an imported key: from its .pub
// file, or derived from the private key (OpenSSH keys carry the public part
// in clear even when they have a passphrase).
func publicKeyLine(k keyInfo) (string, error) {
	if raw, err := os.ReadFile(k.Path + ".pub"); err == nil {
		if pk, c, _, _, err := ssh.ParseAuthorizedKey(raw); err == nil {
			return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + commentSuffix(c), nil
		}
	}
	data, err := os.ReadFile(k.Path)
	if err != nil {
		return "", err
	}
	var pub ssh.PublicKey
	if s, err := ssh.ParsePrivateKey(data); err == nil {
		pub = s.PublicKey()
	} else {
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) && pm.PublicKey != nil {
			pub = pm.PublicKey
		}
	}
	if pub == nil {
		return "", errors.New("chiave con passphrase in formato vecchio: la chiave pubblica non si ricava senza la passphrase")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + k.Name, nil
}

func commentSuffix(c string) string {
	if c = strings.TrimSpace(c); c != "" {
		return " " + c
	}
	return ""
}

// generateKey makes a new ed25519 key (with passphrase if given) in the key
// directory, with its .pub next to it.
func generateKey(name, passphrase string) (keyInfo, error) {
	name = strings.Trim(badKeyChars.ReplaceAllString(strings.TrimSpace(name), "_"), "._")
	if name == "" {
		name = "id_ed25519"
	}
	dir, err := keysDir()
	if err != nil {
		return keyInfo{}, err
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		return keyInfo{}, fmt.Errorf("esiste gia' una chiave %q", name)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return keyInfo{}, err
	}
	comment := name
	if h, err := os.Hostname(); err == nil {
		comment = name + "@" + h
	}
	var blk *pem.Block
	if passphrase != "" {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, comment, []byte(passphrase))
	} else {
		blk, err = ssh.MarshalPrivateKey(priv, comment)
	}
	if err != nil {
		return keyInfo{}, err
	}
	if err := os.WriteFile(dst, pem.EncodeToMemory(blk), 0o600); err != nil {
		return keyInfo{}, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return keyInfo{}, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment + "\n"
	if err := os.WriteFile(dst+".pub", []byte(line), 0o644); err != nil {
		return keyInfo{}, err
	}
	return keyInfo{Name: name, Path: dst, Fingerprint: ssh.FingerprintSHA256(sp), Kind: sp.Type(), Encrypted: passphrase != ""}, nil
}

// renameKey renames an imported key (and its .pub).
func renameKey(old, name string) (string, error) {
	name = strings.Trim(badKeyChars.ReplaceAllString(strings.TrimSpace(name), "_"), "._")
	if name == "" || old != filepath.Base(old) {
		return "", errors.New("nome non valido")
	}
	dir, err := keysDir()
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
		return "", fmt.Errorf("esiste gia' una chiave %q", name)
	}
	if err := os.Rename(filepath.Join(dir, old), filepath.Join(dir, name)); err != nil {
		return "", err
	}
	_ = os.Rename(filepath.Join(dir, old+".pub"), filepath.Join(dir, name+".pub"))
	return name, nil
}

type keysDialog struct {
	filter             widget.Editor
	list               widget.List
	rows               []widget.Clickable
	keys               []keyInfo
	sel                string // name of the selected key
	use, copyPub, ren  widget.Clickable
	del, imp, gen      widget.Clickable
	close              widget.Clickable
	delArmed           bool
	renaming           bool
	renameEd           widget.Editor
	renameOK, renameNo widget.Clickable
	genOpen            bool
	genName, genPass   widget.Editor
	genPass2           widget.Editor
	genOK, genNo       widget.Clickable
	imported           chan keyInfo
	onUse              func(keyInfo) // set when opened from "Nuova sessione"
	onClose            func()
	clipText           string
}

// openKeysDialog shows the keys over the open dialog (if any) and goes back
// to it on close. onUse, if not nil, adds "Usa questa chiave".
func (g *gui) openKeysDialog() { g.openKeysDialogFor(nil, nil) }

func (g *gui) openKeysDialogFor(onUse func(keyInfo), onClose func()) {
	d := &keysDialog{onUse: onUse, onClose: onClose, imported: make(chan keyInfo, 1)}
	d.filter.SingleLine = true
	d.renameEd.SingleLine, d.genName.SingleLine = true, true
	d.genPass.SingleLine, d.genPass2.SingleLine = true, true
	d.genPass.Mask, d.genPass2.Mask = '•', '•'
	d.list.Axis = layout.Vertical
	d.reload()
	g.dlgMu.Lock()
	if g.dlg != nil {
		g.dlgStack = append(g.dlgStack, g.dlg)
	}
	g.dlgMu.Unlock()
	g.setDialog(d)
}

func (d *keysDialog) reload() {
	d.keys = listKeys()
	if len(d.rows) < len(d.keys) {
		d.rows = make([]widget.Clickable, len(d.keys))
	}
	if d.selected() == nil && len(d.keys) > 0 {
		d.sel = d.keys[0].Name
	}
	d.delArmed = false
}

func (d *keysDialog) selected() *keyInfo {
	for i := range d.keys {
		if d.keys[i].Name == d.sel {
			return &d.keys[i]
		}
	}
	return nil
}

func (d *keysDialog) Cancel() {
	if d.onClose != nil {
		d.onClose()
	}
}

func (d *keysDialog) finish(g *gui) {
	d.Cancel()
	g.closeDialog()
}

func (d *keysDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	select {
	case k := <-d.imported:
		d.reload()
		d.sel = k.Name
	default:
	}
	if d.close.Clicked(gtx) {
		d.finish(g)
		return layout.Dimensions{}
	}
	if d.imp.Clicked(gtx) {
		g.pickAndImport(func(k keyInfo) {
			select {
			case d.imported <- k:
			default:
			}
		})
	}
	if d.gen.Clicked(gtx) {
		d.genOpen = !d.genOpen
		d.renaming = false
	}
	if d.genNo.Clicked(gtx) {
		d.genOpen = false
	}
	if d.genOK.Clicked(gtx) {
		if d.genPass.Text() != d.genPass2.Text() {
			g.flash("le due passphrase non coincidono")
		} else if k, err := generateKey(d.genName.Text(), d.genPass.Text()); err != nil {
			g.flash("%v", err)
		} else {
			d.genOpen = false
			d.genName.SetText("")
			d.genPass.SetText("")
			d.genPass2.SetText("")
			d.reload()
			d.sel = k.Name
			g.flashInfo("chiave %q creata: copia la chiave pubblica sul server (ptscry setup keys add UTENTE FILE)", k.Name)
		}
	}
	sel := d.selected()
	if sel != nil {
		if d.use.Clicked(gtx) && d.onUse != nil {
			k := *sel
			d.onUse(k)
			d.finish(g)
			return layout.Dimensions{}
		}
		if d.copyPub.Clicked(gtx) {
			if line, err := publicKeyLine(*sel); err != nil {
				g.flash("%v", err)
			} else {
				gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(line + "\n"))})
				d.clipText = line
				g.flashInfo("chiave pubblica di %q copiata: incollala nel server (ptscry setup keys add UTENTE -)", sel.Name)
			}
		}
		if d.ren.Clicked(gtx) {
			d.renaming = !d.renaming
			d.genOpen = false
			d.renameEd.SetText(sel.Name)
		}
		if d.renameNo.Clicked(gtx) {
			d.renaming = false
		}
		if d.renameOK.Clicked(gtx) {
			if n, err := renameKey(sel.Name, d.renameEd.Text()); err != nil {
				g.flash("%v", err)
			} else {
				d.renaming = false
				d.sel = n
				d.reload()
				sel = d.selected()
			}
		}
		if d.del.Clicked(gtx) && sel != nil {
			if d.delArmed {
				if err := removeKey(sel.Name); err != nil {
					g.flash("%v", err)
				}
				_ = os.Remove(sel.Path + ".pub")
				d.sel = ""
				d.reload()
				sel = d.selected()
			} else {
				d.delArmed = true
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(d.filter.Text()))
	var shown []int
	for i, k := range d.keys {
		if q == "" || strings.Contains(strings.ToLower(k.Name+" "+k.Kind+" "+k.Fingerprint), q) {
			shown = append(shown, i)
		}
	}
	for _, i := range shown {
		if d.rows[i].Clicked(gtx) {
			if d.sel != d.keys[i].Name {
				d.delArmed, d.renaming = false, false
			}
			d.sel = d.keys[i].Name
			sel = d.selected()
		}
	}
	dir, _ := keysDir()
	kids := []layout.FlexChild{
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(g.th, 16, "Chiavi private")
			l.Color = theme.fg
			l.Font.Weight = font.Bold
			return l.Layout(gtx)
		}),
		g.row(gtx, small(g, "Copie private (0600) in "+dir+"; l'originale non si tocca. Una passphrase si chiede alla connessione.")),
		g.row(gtx, g.field(&d.filter, "cerca per nome, tipo o impronta…")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			if len(d.keys) == 0 {
				return small(g, "nessuna chiave: importane una da file o generane una nuova")(gtx)
			}
			if len(shown) == 0 {
				return small(g, "nessuna corrispondenza")(gtx)
			}
			box := gtx
			box.Constraints.Max.Y = min(gtx.Constraints.Max.Y, gtx.Dp(unit.Dp(230)))
			box.Constraints.Min.Y = 0
			return material.List(g.th, &d.list).Layout(box, len(shown), func(gtx layout.Context, n int) layout.Dimensions {
				i := shown[n]
				k := d.keys[i]
				on := k.Name == d.sel
				return d.rows[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{}.Layout(gtx,
						layout.Expanded(func(gtx layout.Context) layout.Dimensions {
							switch {
							case on:
								paint.FillShape(gtx.Ops, theme.selection, clip.Rect{Max: gtx.Constraints.Min}.Op())
							case d.rows[i].Hovered():
								paint.FillShape(gtx.Ops, theme.bg, clip.Rect{Max: gtx.Constraints.Min}.Op())
							}
							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							return layout.Inset{Left: 8, Right: 8, Top: 5, Bottom: 5}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										t := k.Name + "    " + k.Kind
										if k.Encrypted {
											t += "    con passphrase"
										}
										l := material.Label(g.th, 14, t)
										l.Color = theme.fg
										return l.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										fp := k.Fingerprint
										if fp == "" {
											fp = "impronta non leggibile senza passphrase"
										}
										l := material.Label(g.th, 11, fp)
										l.Font.Typeface = "Go Mono"
										l.Color = theme.chromeText
										return l.Layout(gtx)
									}),
								)
							})
						}),
					)
				})
			})
		}),
	}
	if sel != nil {
		delLabel, delBg := "Rimuovi", theme.bg
		if d.delArmed {
			delLabel, delBg = "Sicuro?", theme.warn
		}
		kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			var b []layout.FlexChild
			if d.onUse != nil {
				b = append(b, layout.Rigid(g.smallButton(&d.use, "Usa questa chiave", theme.accent)), layout.Rigid(layout.Spacer{Width: 6}.Layout))
			}
			b = append(b,
				layout.Rigid(g.smallButton(&d.copyPub, "Copia la chiave pubblica", theme.bg)),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(g.smallButton(&d.ren, "Rinomina", theme.bg)),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(g.smallButton(&d.del, delLabel, delBg)),
			)
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, b...)
		}))
		if d.renaming {
			kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, g.field(&d.renameEd, "nuovo nome")),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Rigid(g.smallButton(&d.renameOK, "OK", theme.accent)),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Rigid(g.smallButton(&d.renameNo, "Annulla", theme.bg)),
				)
			}))
		}
	}
	if d.genOpen {
		kids = append(kids,
			g.row(gtx, labeled(g, "Nome della nuova chiave (ed25519)", g.field(&d.genName, "id_ed25519"))),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(1, labeled(g, "Passphrase (consigliata; vuota = nessuna)", g.field(&d.genPass, ""))),
					layout.Rigid(layout.Spacer{Width: 8}.Layout),
					layout.Flexed(1, labeled(g, "Ripeti la passphrase", g.field(&d.genPass2, ""))),
				)
			}),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(g.smallButton(&d.genOK, "Crea la chiave", theme.accent)),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Rigid(g.smallButton(&d.genNo, "Annulla", theme.bg)),
				)
			}),
		)
	}
	kids = append(kids, layout.Rigid(layout.Spacer{Height: 6}.Layout), g.row(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
			layout.Rigid(g.smallButton(&d.imp, "Importa da file…", theme.bg)),
			layout.Rigid(layout.Spacer{Width: 6}.Layout),
			layout.Rigid(g.smallButton(&d.gen, "Genera una nuova chiave…", theme.bg)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
			layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.accent)),
		)
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}
