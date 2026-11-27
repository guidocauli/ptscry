// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// serverdlg.go: adding and editing the servers and groups of the side bar
// (as in RexDesk: name, address, access, group, colour, description,
// favourite), and the small dialogs they need: a question with a dangerous
// answer, a name to type.

package main

import (
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"ptscry/internal/rclient"
)

// the ready-made colours of servers and groups ("" = none)
var swatchColors = []string{"", "#e05252", "#e8913a", "#e3c53b", "#4caf50", "#26a69a", "#3d8bfd", "#9c6ade", "#e05297", "#9e9e9e"}

type serverDialog struct {
	orig    string // the server being changed ("" = a new one)
	kind    string // kindPtscry or kindSSH
	kindBtn [2]widget.Clickable

	name, host, port, user widget.Editor
	jump, command, desc    widget.Editor
	hex                    widget.Editor
	auth                   widget.Enum
	keyCombo               *combo
	groupCombo             *combo
	browse, manageKeys     widget.Clickable
	picked                 chan string
	keysChanged            bool
	fav                    widget.Bool
	swatch                 []widget.Clickable
	ok, okOpen, can        widget.Clickable
	focused                bool
}

func groupItems(gs []group) []comboItem {
	items := []comboItem{{Label: "(nessun gruppo)", Detail: "al livello principale", Value: ""}}
	for _, gr := range gs {
		det := gr.Description
		if gr.Parent != "" {
			det = strings.TrimSpace("in " + gr.Parent + "  " + det)
		}
		items = append(items, comboItem{Label: gr.Name, Detail: det, Value: gr.Name})
	}
	return items
}

// openServerDialog adds a server (orig "") in the group gr, or changes orig.
func (g *gui) openServerDialog(orig, gr string) {
	d := &serverDialog{
		orig:       orig,
		kind:       kindPtscry,
		keyCombo:   newCombo("vuoto = chiavi di ssh-agent e di ~/.ssh", "nessuna chiave: importala con Importa"),
		groupCombo: newCombo("nessun gruppo (scrivi un nome per crearne uno)", "nessun gruppo"),
		picked:     make(chan string, 1),
		swatch:     make([]widget.Clickable, len(swatchColors)),
	}
	for _, e := range []*widget.Editor{&d.name, &d.host, &d.port, &d.user, &d.jump, &d.command, &d.desc, &d.hex} {
		e.SingleLine = true
	}
	d.name.Submit = true
	d.auth.Value = "agent"
	d.port.SetText(rclient.DefaultPort)
	if u, err := rclient.CurrentUser(); err == nil {
		d.user.SetText(u)
	}
	d.keyCombo.SetItems(keyItems())
	g.setMu.Lock()
	d.groupCombo.SetItems(groupItems(g.set.Groups))
	var p profile
	found := false
	if orig != "" {
		if pp := g.set.profile(orig); pp != nil {
			p, found = *pp, true
		}
	}
	g.setMu.Unlock()
	if orig != "" && !found {
		g.flash("il server %q non esiste piu'", orig)
		return
	}
	if found {
		d.fill(p)
	} else {
		d.groupCombo.Choose(gr)
	}
	g.setDialog(d)
}

func (d *serverDialog) fill(p profile) {
	d.kind = kindPtscry
	parse := rclient.ParseTarget
	if p.Mode == "ssh" {
		d.kind = kindSSH
		parse = parseSSHTarget
	}
	if t, err := parse(p.Target); err == nil {
		d.host.SetText(strings.Trim(t.Host, "[]"))
		d.port.SetText(t.Port)
		if strings.Contains(p.Target, "@") {
			d.user.SetText(t.User)
		}
	}
	d.name.SetText(p.Name)
	if p.Identity != "" && !d.keyCombo.Choose(p.Identity) {
		d.keyCombo.ed.SetText(p.Identity)
	}
	d.auth.Value = p.Auth
	if d.auth.Value == "" {
		d.auth.Value = "agent"
		if p.Identity != "" {
			d.auth.Value = "key"
		}
	}
	d.jump.SetText(p.Jump)
	d.command.SetText(p.Command)
	d.desc.SetText(p.Description)
	d.hex.SetText(p.Color)
	d.fav.Value = p.Favorite
	d.groupCombo.Choose(p.Group)
}

func (d *serverDialog) Cancel() {}

// buildTarget is user@host:port, checked.
func buildTarget(host, port, user string) (string, error) {
	host, port, user = strings.TrimSpace(host), strings.TrimSpace(port), strings.TrimSpace(user)
	if host == "" {
		return "", fmt.Errorf("scrivi il nome o l'indirizzo del server")
	}
	if !hostRe.MatchString(host) {
		return "", fmt.Errorf("server non valido: %q", host)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("porta non valida: %q", port)
	}
	if !userRe.MatchString(user) {
		return "", fmt.Errorf("utente non valido: %q", user)
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6
	}
	t := host + ":" + port
	if user != "" {
		t = user + "@" + t
	}
	return t, nil
}

// save checks the fields and writes the server; it returns its name.
func (d *serverDialog) save(g *gui) (string, error) {
	t, err := buildTarget(d.host.Text(), d.port.Text(), d.user.Text())
	if err != nil {
		return "", err
	}
	mode := "remote"
	if d.kind == kindSSH {
		mode = "ssh"
		if _, err := parseSSHTarget(t); err != nil {
			return "", err
		}
	} else if _, err := rclient.ParseTarget(t); err != nil {
		return "", err
	}
	p := profile{Mode: mode, Target: t}
	key := strings.TrimSpace(d.keyCombo.Value())
	if mode == "ssh" {
		p.Auth = d.auth.Value
		if p.Auth == "key" {
			if key == "" {
				return "", fmt.Errorf("scegli il file della chiave, o un altro metodo di accesso")
			}
			p.Identity = key
		}
		if j := strings.TrimSpace(d.jump.Text()); j != "" {
			if _, err := parseSSHTarget(j); err != nil {
				return "", fmt.Errorf("jump host: %v", err)
			}
			p.Jump = j
		}
		p.Command = strings.TrimSpace(d.command.Text())
		if strings.ContainsAny(p.Command, "\r\n\x00") || len(p.Command) > 1024 {
			return "", fmt.Errorf("il comando deve stare su una riga")
		}
	} else {
		p.Identity = key
	}
	name := strings.TrimSpace(d.name.Text())
	if name == "" {
		name = hostPortOnly(t)
		if i := strings.LastIndexByte(name, ':'); i > 0 && !strings.HasSuffix(name, "]") {
			name = name[:i]
		}
	}
	if len(name) > 64 || strings.ContainsAny(name, "\x00\r\n\t") {
		return "", fmt.Errorf("nome del server non valido")
	}
	p.Description = cleanLine(d.desc.Text(), 200)
	p.Color = strings.TrimSpace(d.hex.Text())
	if !validColor(p.Color) {
		return "", fmt.Errorf("colore non valido: scrivilo come #RRGGBB o sceglilo tra quelli pronti")
	}
	p.Favorite = d.fav.Value
	gr := strings.TrimSpace(d.groupCombo.Value())
	if gr != "" && !validGroupName(gr) {
		return "", fmt.Errorf("nome del gruppo non valido")
	}
	p.Group = gr

	g.setMu.Lock()
	defer g.setMu.Unlock()
	if gr != "" && g.set.groupByName(gr) == nil {
		g.set.Groups = append(g.set.Groups, group{Name: gr}) // a new group, typed in the field
	}
	if old := g.set.profile(name); old != nil && name != d.orig {
		return "", fmt.Errorf("esiste gia' un server (o una voce predefinita) di nome %q: scegline un altro", name)
	}
	p.Name = name
	if d.orig == "" {
		g.set.Profiles = append(g.set.Profiles, p)
		delete(g.hubs, name) // a stale connection under this name, if any
		return name, nil
	}
	old := g.set.profile(d.orig)
	if old == nil {
		return "", fmt.Errorf("il server %q non esiste piu'", d.orig)
	}
	if old.Target == p.Target && old.Mode == p.Mode {
		p.Pinned = old.Pinned
	}
	p.Cert = old.Cert
	*old = p
	if d.orig != name {
		g.renameProfileLocked(d.orig, name)
	}
	delete(g.hubs, name) // the next tab connects with the new settings; the open ones keep theirs
	return name, nil
}

// renameProfileLocked follows a server's new name everywhere (setMu held).
func (g *gui) renameProfileLocked(old, name string) {
	if h, ok := g.hubs[old]; ok {
		delete(g.hubs, old)
		g.hubs[name] = h
	}
	if g.set.Default == old {
		g.set.Default = name
	}
	for i := range g.set.Workspaces {
		for j := range g.set.Workspaces[i].Tabs {
			if g.set.Workspaces[i].Tabs[j].Profile == old {
				g.set.Workspaces[i].Tabs[j].Profile = name
			}
		}
	}
	for i, k := range g.set.Open {
		for _, pre := range []string{"p:", "f/p:"} {
			if k == pre+old {
				g.set.Open[i] = pre + name
			}
		}
	}
	// the tabs belong to the UI goroutine, which is the one running this
	for _, l := range g.tabs {
		for _, t := range l.panes() {
			if t.profile == old {
				t.mu.Lock() // its goroutine reads it under t.mu
				t.profile = name
				t.mu.Unlock()
			}
		}
	}
	g.side.mu.Lock() // lock order: setMu, then side.mu
	if c, ok := g.side.sess[old]; ok {
		delete(g.side.sess, old)
		g.side.sess[name] = c
	}
	g.side.mu.Unlock()
}

func (d *serverDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	for i, k := range []string{kindPtscry, kindSSH} {
		if d.kindBtn[i].Clicked(gtx) && d.kind != k {
			d.kind = k
			if k == kindSSH && d.port.Text() == rclient.DefaultPort {
				d.port.SetText("22")
			} else if k == kindPtscry && d.port.Text() == "22" {
				d.port.SetText(rclient.DefaultPort)
			}
		}
	}
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	submit := false
	for {
		ev, ok := d.name.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			submit = true
		}
	}
	openToo := d.okOpen.Clicked(gtx)
	if d.ok.Clicked(gtx) || submit || openToo {
		name, err := d.save(g)
		if err != nil {
			g.flash("%v", err)
		} else {
			g.closeDialog()
			g.saveWorkspace()
			if d.orig == "" {
				g.logAct(false, "server %q aggiunto", name)
			} else {
				g.logAct(false, "server %q modificato", name)
			}
			g.side.sel = "p:" + name
			if openToo {
				g.setMu.Lock()
				mode := ""
				if p := g.set.profile(name); p != nil {
					mode = p.Mode
				}
				g.setMu.Unlock()
				g.openRow(treeRow{kind: nodeServer, profile: name, label: name, mode: mode})
			}
		}
	}
	for i := range d.swatch {
		if d.swatch[i].Clicked(gtx) {
			d.hex.SetText(swatchColors[i])
		}
	}
	select {
	case p := <-d.picked:
		d.keyCombo.SetItems(keyItems())
		if !d.keyCombo.Choose(p) {
			d.keyCombo.ed.SetText(p)
		}
		d.auth.Value = "key"
	default:
	}
	if d.browse.Clicked(gtx) {
		g.pickAndImport(func(k keyInfo) {
			select {
			case d.picked <- k.Path:
			default:
			}
		})
	}
	if d.manageKeys.Clicked(gtx) {
		g.openKeysDialogFor(func(k keyInfo) {
			select {
			case d.picked <- k.Path:
			default:
			}
		}, func() { d.keysChanged = true })
	}
	if d.keysChanged {
		d.keysChanged = false
		cur := d.keyCombo.Value()
		d.keyCombo.SetItems(keyItems())
		if cur != "" && !d.keyCombo.Choose(cur) {
			d.keyCombo.ed.SetText(cur)
		}
	}
	if !d.focused {
		d.focused = true
		focusEditor(gtx, &d.host)
		if d.orig != "" {
			focusEditor(gtx, &d.name)
		}
	}
	head := func(s string) layout.FlexChild { return g.row(gtx, g.heading(s)) }
	title := "Aggiungi server"
	if d.orig != "" {
		title = "Modifica il server " + d.orig
	}
	kids := []layout.FlexChild{
		g.row(gtx, g.title(title)),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			labels := []string{"ptscry (sessioni persistenti)", "SSH (terminale semplice)"}
			var ch []layout.FlexChild
			for i, k := range []string{kindPtscry, kindSSH} {
				i, k := i, k
				ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return (&newSessionDialog{}).kindButton(gtx, g, &d.kindBtn[i], labels[i], d.kind == k)
				}))
				if i == 0 {
					ch = append(ch, layout.Rigid(layout.Spacer{Width: 4}.Layout))
				}
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, ch...)
		}),
		g.row(gtx, labeled(g, "Nome (come compare nell'elenco; vuoto = l'indirizzo)", g.field(&d.name, "es. web01"))),
		head("Server"),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(0.55, labeled(g, "Indirizzo", g.field(&d.host, "server.example.org"))),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Flexed(0.15, labeled(g, "Porta", g.field(&d.port, "22"))),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Flexed(0.30, labeled(g, "Utente", g.field(&d.user, "utente"))),
			)
		}),
		head("Accesso"),
	}
	if d.kind == kindSSH {
		kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.auth, "agent", "Agente SSH e chiavi di ~/.ssh").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.auth, "key", "File di chiave").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.auth, "password", "Password").Layout),
			)
		}))
	}
	if d.kind == kindPtscry || d.auth.Value == "key" {
		kids = append(kids,
			g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.keyCombo.Layout(gtx, g) }),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(g.smallButton(&d.browse, "Importa una chiave…", theme.input)),
					layout.Rigid(layout.Spacer{Width: 8}.Layout),
					layout.Rigid(g.smallButton(&d.manageKeys, "Elenco delle chiavi…", theme.input)),
				)
			}),
		)
	}
	kids = append(kids, g.row(gtx, small(g, "Password, passphrase e codici TOTP si chiedono alla connessione e non vengono salvati.")))
	if d.kind == kindSSH {
		kids = append(kids,
			g.row(gtx, labeled(g, "Passa per (jump host, come ssh -J; facoltativo)", g.field(&d.jump, "utente@bastion.example.org:22"))),
			g.row(gtx, labeled(g, "Comando al posto della shell (facoltativo)", g.field(&d.command, "es. tmux attach || tmux"))),
		)
	}
	kids = append(kids,
		head("Nell'elenco"),
		g.row(gtx, labeled(g, "Gruppo", func(gtx layout.Context) layout.Dimensions { return d.groupCombo.Layout(gtx, g) })),
		g.row(gtx, labeled(g, "Descrizione (facoltativa)", g.field(&d.desc, "es. produzione, rack 3"))),
		g.row(gtx, labeled(g, "Colore (sull'elenco e sui tab; vuoto = quello del gruppo)", func(gtx layout.Context) layout.Dimensions {
			return g.colorPicker(gtx, d.swatch, &d.hex)
		})),
		g.row(gtx, material.CheckBox(g.th, &d.fav, "Nei Preferiti").Layout),
		layout.Rigid(layout.Spacer{Height: 4}.Layout),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.can, "Annulla", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.okOpen, map[bool]string{true: "Salva e connetti", false: "Salva e apri una sessione"}[d.kind == kindSSH], theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.ok, "Salva", theme.accent)),
			)
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// colorPicker: the ready-made colours and the #RRGGBB field.
func (g *gui) colorPicker(gtx layout.Context, sw []widget.Clickable, hex *widget.Editor) layout.Dimensions {
	cur := strings.ToLower(strings.TrimSpace(hex.Text()))
	var ch []layout.FlexChild
	for i := range sw {
		i := i
		ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sw[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				s := gtx.Dp(22)
				r := image.Rect(0, 0, s, s)
				if strings.ToLower(swatchColors[i]) == cur {
					paint.FillShape(gtx.Ops, theme.fg, clip.UniformRRect(r, gtx.Dp(5)).Op(gtx.Ops))
				}
				in := r.Inset(gtx.Dp(2))
				if c, ok := parseColor(swatchColors[i]); ok {
					paint.FillShape(gtx.Ops, c, clip.UniformRRect(in, gtx.Dp(4)).Op(gtx.Ops))
				} else { // none: an empty box with a slash
					paint.FillShape(gtx.Ops, theme.input, clip.UniformRRect(in, gtx.Dp(4)).Op(gtx.Ops))
					paint.FillShape(gtx.Ops, theme.chromeText, clip.Stroke{Path: clip.UniformRRect(in, gtx.Dp(4)).Path(gtx.Ops), Width: 1}.Op())
				}
				return layout.Dimensions{Size: image.Pt(s+gtx.Dp(4), s)}
			})
		}))
	}
	ch = append(ch, layout.Rigid(layout.Spacer{Width: 6}.Layout), layout.Flexed(1, g.field(hex, "#RRGGBB")))
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, ch...)
}

// title and heading of the dialogs, as in RexDesk.
func (g *gui) title(s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Label(g.th, 17, s)
		l.Font.Weight = font.Bold
		l.Color = theme.fg
		return l.Layout(gtx)
	}
}

func (g *gui) heading(s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Label(g.th, 13, s)
		l.Color = theme.accent
		l.Font.Weight = font.Bold
		return layout.Inset{Top: 6}.Layout(gtx, l.Layout)
	}
}

// duplicateServer opens the dialog with a copy of a server, under a free name.
func (g *gui) duplicateServer(name string) {
	g.setMu.Lock()
	p := g.set.profile(name)
	var cp profile
	if p != nil {
		cp = *p
	}
	free := ""
	for i := 2; p != nil && i < 1000; i++ {
		n := fmt.Sprintf("%s (%d)", name, i)
		if g.set.profile(n) == nil {
			free = n
			break
		}
	}
	g.setMu.Unlock()
	if p == nil {
		return
	}
	g.openServerDialog("", cp.Group)
	g.dlgMu.Lock()
	if d, ok := g.dlg.(*serverDialog); ok {
		d.fill(cp)
		d.name.SetText(free)
	}
	g.dlgMu.Unlock()
}

// ---- groups ----

type groupDialog struct {
	orig      string
	name      widget.Editor
	desc, hex widget.Editor
	parent    *combo
	swatch    []widget.Clickable
	ok, can   widget.Clickable
	focused   bool
}

func (g *gui) openGroupDialog(orig, parent string) {
	d := &groupDialog{orig: orig, parent: newCombo("nessuno (livello principale)", "nessun gruppo"), swatch: make([]widget.Clickable, len(swatchColors))}
	for _, e := range []*widget.Editor{&d.name, &d.desc, &d.hex} {
		e.SingleLine = true
	}
	d.name.Submit = true
	g.setMu.Lock()
	var others []group
	for _, gr := range g.set.Groups {
		if orig == "" || !g.set.groupInside(gr.Name, orig) {
			others = append(others, gr) // a group cannot go inside itself
		}
	}
	items := groupItems(others)
	items[0].Label = "(nessuno)"
	d.parent.SetItems(items)
	if orig != "" {
		if gr := g.set.groupByName(orig); gr != nil {
			d.name.SetText(gr.Name)
			d.desc.SetText(gr.Description)
			d.hex.SetText(gr.Color)
			parent = gr.Parent
		}
	}
	g.setMu.Unlock()
	d.parent.Choose(parent)
	g.setDialog(d)
}

// groupInside: is gr the group anc or inside it?
func (s *settings) groupInside(gr, anc string) bool {
	for i := 0; gr != "" && i < 100; i++ {
		if gr == anc {
			return true
		}
		g := s.groupByName(gr)
		if g == nil {
			return false
		}
		gr = g.Parent
	}
	return false
}

func (d *groupDialog) Cancel() {}

func (d *groupDialog) save(g *gui) error {
	name := strings.TrimSpace(d.name.Text())
	if !validGroupName(name) {
		return fmt.Errorf("scrivi il nome del gruppo (al massimo 64 caratteri)")
	}
	col := strings.TrimSpace(d.hex.Text())
	if !validColor(col) {
		return fmt.Errorf("colore non valido: scrivilo come #RRGGBB o sceglilo tra quelli pronti")
	}
	parent := strings.TrimSpace(d.parent.Value())
	g.setMu.Lock()
	defer g.setMu.Unlock()
	if parent != "" && g.set.groupByName(parent) == nil {
		return fmt.Errorf("il gruppo superiore %q non esiste", parent)
	}
	if d.orig != "" && parent != "" && g.set.groupInside(parent, d.orig) {
		return fmt.Errorf("un gruppo non puo' stare dentro se stesso")
	}
	if name != d.orig && g.set.groupByName(name) != nil {
		return fmt.Errorf("esiste gia' un gruppo %q", name)
	}
	gr := group{Name: name, Parent: parent, Color: col, Description: cleanLine(d.desc.Text(), 200)}
	if d.orig == "" {
		g.set.Groups = append(g.set.Groups, gr)
		return nil
	}
	old := g.set.groupByName(d.orig)
	if old == nil {
		return fmt.Errorf("il gruppo %q non esiste piu'", d.orig)
	}
	*old = gr
	if name != d.orig {
		for i := range g.set.Groups {
			if g.set.Groups[i].Parent == d.orig {
				g.set.Groups[i].Parent = name
			}
		}
		for i := range g.set.Profiles {
			if g.set.Profiles[i].Group == d.orig {
				g.set.Profiles[i].Group = name
			}
		}
		for i, k := range g.set.Open {
			if k == "g:"+d.orig {
				g.set.Open[i] = "g:" + name
			}
		}
	}
	return nil
}

func (d *groupDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	submit := false
	for {
		ev, ok := d.name.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			submit = true
		}
	}
	if d.ok.Clicked(gtx) || submit {
		if err := d.save(g); err != nil {
			g.flash("%v", err)
		} else {
			name := strings.TrimSpace(d.name.Text())
			g.closeDialog()
			g.setOpen("g:"+name, true)
			if p := strings.TrimSpace(d.parent.Value()); p != "" {
				g.setOpen("g:"+p, true)
			}
			g.side.sel = "g:" + name
			g.saveWorkspace()
		}
	}
	for i := range d.swatch {
		if d.swatch[i].Clicked(gtx) {
			d.hex.SetText(swatchColors[i])
		}
	}
	if !d.focused {
		d.focused = true
		focusEditor(gtx, &d.name)
	}
	title := "Nuovo gruppo"
	if d.orig != "" {
		title = "Modifica il gruppo " + d.orig
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.title(title)),
		g.row(gtx, labeled(g, "Nome", g.field(&d.name, "es. produzione"))),
		g.row(gtx, labeled(g, "Dentro il gruppo", func(gtx layout.Context) layout.Dimensions { return d.parent.Layout(gtx, g) })),
		g.row(gtx, labeled(g, "Descrizione (facoltativa)", g.field(&d.desc, ""))),
		g.row(gtx, labeled(g, "Colore (vale per i suoi server senza un colore proprio)", func(gtx layout.Context) layout.Dimensions {
			return g.colorPicker(gtx, d.swatch, &d.hex)
		})),
		g.row(gtx, g.buttons(&d.ok, &d.can, "Salva")),
	)
}

// ---- small dialogs ----

// confirmDialog asks before doing something; danger paints the button red.
type confirmDialog struct {
	head, text, okLabel string
	danger              bool
	do                  func()
	ok, can             widget.Clickable
}

func (g *gui) confirm(head, text, okLabel string, danger bool, do func()) {
	g.setDialog(&confirmDialog{head: head, text: text, okLabel: okLabel, danger: danger, do: do})
}

func (d *confirmDialog) Cancel() {}

func (d *confirmDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	if d.ok.Clicked(gtx) {
		g.closeDialog()
		d.do()
	}
	bg := theme.accent
	if d.danger {
		bg = theme.warn
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.title(d.head)),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(g.th, 14, d.text)
			l.Color = theme.fg
			return l.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: 6}.Layout),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.can, "Annulla", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.ok, d.okLabel, bg)),
			)
		}),
	)
}

// inputDialog asks for one line; done returns an error to keep it open.
type inputDialog struct {
	head, label, hint string
	ed                widget.Editor
	done              func(string) error
	ok, can           widget.Clickable
	focused           bool
}

func (g *gui) askLine(head, label, hint, initial string, done func(string) error) {
	d := &inputDialog{head: head, label: label, hint: hint, done: done}
	d.ed.SingleLine, d.ed.Submit = true, true
	d.ed.SetText(initial)
	d.ed.SetCaret(len([]rune(initial)), 0)
	g.setDialog(d)
}

func (d *inputDialog) Cancel() {}

func (d *inputDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	submit := d.ok.Clicked(gtx)
	for {
		ev, ok := d.ed.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			submit = true
		}
	}
	if submit {
		g.closeDialog()
		if err := d.done(strings.TrimSpace(d.ed.Text())); err != nil {
			g.flash("%v", err)
			g.setDialog(d) // again, with what was typed
		}
	}
	if !d.focused {
		d.focused = true
		focusEditor(gtx, &d.ed)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.title(d.head)),
		g.row(gtx, labeled(g, d.label, g.field(&d.ed, d.hint))),
		g.row(gtx, g.buttons(&d.ok, &d.can, "OK")),
	)
}

// openNewSessionFor asks the name of a new session of a server.
func (g *gui) openNewSessionFor(prof string) {
	g.askLine("Nuova sessione su "+prof, "Nome della sessione (se esiste gia', ci si collega)", "main", g.freeSessionName(), func(s string) error {
		if !nameRe.MatchString(s) {
			return fmt.Errorf("nome non valido: lettere, cifre e _ . : = @ + , - (non all'inizio)")
		}
		g.home = false
		g.openSession(prof, s, false)
		return nil
	})
}

// openSessionRename renames a session on its server.
func (g *gui) openSessionRename(prof, old string) {
	g.askLine("Rinomina la sessione", "Nuovo nome della sessione "+old+" (sul server: vale per tutti)", "", old, func(s string) error {
		if s == old || s == "" {
			return nil
		}
		if !nameRe.MatchString(s) {
			return fmt.Errorf("nome non valido: lettere, cifre e _ . : = @ + , - (non all'inizio)")
		}
		go func() {
			if _, err := g.hubFor(prof).execCmd([]string{"rename", old, s}, 15*time.Second); err != nil {
				g.flash("sessione non rinominata: %v", err)
				return
			}
			for _, t := range g.allTabs() {
				if t.profile == prof && t.sessionName() == old {
					t.mu.Lock()
					t.session = s
					t.mu.Unlock()
				}
			}
			g.logAct(false, "%s: sessione %q rinominata in %q", prof, old, s)
			g.saveWorkspace()
			g.refreshTree()
		}()
		return nil
	})
}

var _ = color.NRGBA{}
