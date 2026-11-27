// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// layouts.go: the layouts of the panes kept on the server ("ptscry layout"),
// and the dialog that saves, opens and removes them.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"

	"ptscry/internal/rclient"
)

// execInput runs a ptscry command with a standard input (layout save) and
// returns what it printed.
func (h *hub) execInput(args []string, in []byte, d time.Duration) (string, error) {
	switch h.p.Mode {
	case "shell", "ssh":
		return "", errors.New("questa connessione non ha un server ptscry")
	}
	if h.p.Mode == "standalone" {
		bin, err := ptscryBinary(h.p)
		if err != nil {
			return "", err
		}
		conf, _, err := localConf()
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(cmdEnv(), "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf)
		cmd.Stdin = bytes.NewReader(in)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err = cmd.Run()
		if ctx.Err() != nil {
			return out.String(), errors.New("tempo scaduto")
		}
		return strings.TrimSpace(out.String()), err
	}
	line, _, _, err := rclient.Cmdline(args)
	if err != nil {
		return "", err
	}
	cl, err := h.client()
	if err != nil {
		return "", err
	}
	type res struct {
		out string
		r   rclient.Result
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var out bytes.Buffer
		var r rclient.Result
		var err error
		if in != nil {
			r, err = rclient.RunInput(cl, line, bytes.NewReader(in), &out, &out)
		} else {
			r, err = rclient.Run(cl, line, &out, &out)
		}
		ch <- res{strings.TrimSpace(out.String()), r, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			return x.out, x.err
		}
		if x.r.Code != 0 {
			if x.out != "" {
				return x.out, errors.New(strings.TrimPrefix(x.out, "ptscry: "))
			}
			return x.out, fmt.Errorf("uscita %d", x.r.Code)
		}
		return x.out, nil
	case <-time.After(d):
		return "", errors.New("tempo scaduto")
	}
}

func (h *hub) listLayouts() ([]string, error) {
	out, err := h.execInput([]string{"layout", "list", "-j"}, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return nil, errors.New("risposta del server non valida (ptscry aggiornato?)")
	}
	return names, nil
}

// loadServerLayout opens the layout name of the server of h as a new tab.
func (g *gui) loadServerLayout(h *hub, name string, onlyHere bool) error {
	out, err := h.execInput([]string{"layout", "show", name}, nil, 10*time.Second)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	g.runOnUI(func() { done <- g.applyLayout(h.p.Name, []byte(out), onlyHere) })
	return <-done
}

// ---------------------------------------------------------------- dialog

type layoutDialog struct {
	names   *combo
	newName widget.Editor
	save    widget.Clickable
	load    widget.Clickable
	del     widget.Clickable
	closeB  widget.Clickable
	h       *hub
	lead    *tab
	msg     string
	busy    bool
	loaded  bool
}

// openLayoutDialog: save the panes of the current tab on its server, or open
// a layout saved there.
func (g *gui) openLayoutDialog() {
	l := g.tabs[g.active]
	d := &layoutDialog{h: l.h, lead: l, names: newCombo("disposizione salvata", "nessuna disposizione salvata sul server")}
	d.newName.SingleLine = true
	d.newName.SetText(l.session)
	g.setDialog(d)
	d.refresh(g)
}

func (d *layoutDialog) refresh(g *gui) {
	d.busy = true
	go func() {
		names, err := d.h.listLayouts()
		g.runOnUI(func() {
			d.busy, d.loaded = false, true
			if err != nil {
				d.msg = "elenco non disponibile: " + err.Error()
				return
			}
			var items []comboItem
			for _, n := range names {
				items = append(items, comboItem{Label: n, Value: n})
			}
			d.names.SetItems(items)
		})
	}()
}

func (d *layoutDialog) Cancel() {}

func (d *layoutDialog) run(g *gui, what string, f func() error) {
	d.busy, d.msg = true, what+"..."
	go func() {
		err := f()
		g.runOnUI(func() {
			d.busy = false
			if err != nil {
				d.msg = err.Error()
				return
			}
			d.msg = what + ": fatto"
			d.refresh(g)
		})
	}()
}

func (d *layoutDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.closeB.Clicked(gtx) {
		g.closeDialog()
		return layout.Dimensions{}
	}
	if d.save.Clicked(gtx) && !d.busy {
		name := strings.TrimSpace(d.newName.Text())
		if !nameRe.MatchString(name) || name[0] == '.' {
			d.msg = "nome non valido: lettere, cifre, . _ - (non all'inizio un punto)"
		} else if b, err := g.layoutOf(d.lead); err != nil {
			d.msg = err.Error()
		} else {
			d.run(g, "salvataggio di "+name, func() error {
				_, err := d.h.execInput([]string{"layout", "save", name}, b, 10*time.Second)
				return err
			})
		}
	}
	if d.load.Clicked(gtx) && !d.busy {
		if it, ok := d.names.Chosen(); ok {
			name := it.Value
			d.busy, d.msg = true, "apertura di "+name+"..."
			go func() {
				err := g.loadServerLayout(d.h, name, false)
				g.runOnUI(func() {
					d.busy = false
					if err != nil {
						d.msg = err.Error()
						return
					}
					g.closeDialog()
				})
			}()
		} else {
			d.msg = "scegli una disposizione dall'elenco"
		}
	}
	if d.del.Clicked(gtx) && !d.busy {
		if it, ok := d.names.Chosen(); ok {
			name := it.Value
			d.run(g, "rimozione di "+name, func() error {
				_, err := d.h.execInput([]string{"layout", "rm", name}, nil, 10*time.Second)
				return err
			})
			d.names.Clear()
		}
	}
	n := len(d.lead.panes())
	kids := []layout.FlexChild{
		g.row(gtx, g.label(fmt.Sprintf("Disposizioni sul server di \"%s\" (pannelli e sessioni, anche per il client da terminale: ptscry attach -l NOME)", d.h.p.Name))),
		g.row(gtx, small(g, fmt.Sprintf("Questa scheda: %d pannell%s", n, map[bool]string{true: "o", false: "i"}[n == 1]))),
		g.row(gtx, labeled(g, "Salva come", g.field(&d.newName, "nome"))),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
				layout.Rigid(g.smallButton(&d.save, "Salva sul server", theme.accent)))
		}),
		g.row(gtx, labeled(g, "Salvate", func(gtx layout.Context) layout.Dimensions { return d.names.Layout(gtx, g) })),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.del, "Rimuovi", theme.input)),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
				layout.Rigid(g.smallButton(&d.load, "Apri in una nuova scheda", theme.accent)))
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			s := d.msg
			if d.busy && s == "" {
				s = "..."
			}
			if s == "" {
				return layout.Dimensions{}
			}
			return small(g, s)(gtx)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
				layout.Rigid(g.smallButton(&d.closeB, "Chiudi", theme.input)))
		}),
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}
