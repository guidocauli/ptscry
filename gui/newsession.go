// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// newsession.go: the "Nuova sessione" dialog. Four kinds, chosen with one click:
//   - Terminale locale: a shell of this computer (optionally a persistent ptscry
//     session without server, on Linux and macOS)
//   - Server locale: the ptscry front-end of this computer (127.0.0.1:2222)
//   - SSH: a plain SSH terminal to any server, with its connection options
//   - ptscry: a ptscry server reached over SSH: new session or an existing one
// Saved connections are picked from a searchable combo box.

package main

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"ptscry/internal/rclient"
)

const (
	kindShell  = "shell"
	kindLocal  = "local"
	kindSSH    = "ssh"
	kindPtscry = "ptscry"
)

type newSessionDialog struct {
	split   bool // the session opens in a new pane of the current tab
	kind    string
	kindBtn [4]widget.Clickable

	// terminale locale
	shell      *combo
	persistent widget.Bool // ptscry without server (Linux, macOS)

	// SSH and ptscry: the server
	saved                widget.Clickable
	savedCombo           *combo
	forget               widget.Clickable
	forgetArmed          bool
	host, port, user     widget.Editor
	auth                 widget.Enum // agent, key, password (SSH)
	keyCombo             *combo
	browse, manageKeys   widget.Clickable
	picked               chan string
	keysChanged          bool
	advanced             widget.Bool
	jump, command, cname widget.Editor

	// the ptscry session
	sessMode    widget.Enum // new, attach
	sessName    widget.Editor
	existing    *combo
	refresh     widget.Clickable
	listing     bool
	listMsg     string
	listFor     string
	readOnly    widget.Bool
	env         *envEditor
	sessionsCh  chan sessList
	ok, can     widget.Clickable
	focusOnName bool
}

type sessList struct {
	key   string
	items []comboItem
	err   error
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,253}$`)
var userRe = regexp.MustCompile(`^[A-Za-z0-9._@\\-]{0,64}$`)

func (g *gui) openNewTabDialog() { g.openNewSessionDialog() }

func (g *gui) openNewSessionDialog() {
	d := &newSessionDialog{
		shell:      newCombo("shell o comando (es. /usr/bin/fish -l)", "nessuna shell trovata"),
		savedCombo: newCombo("cerca tra le connessioni salvate…", "nessuna connessione salvata"),
		keyCombo:   newCombo("vuoto = chiavi di ssh-agent e di ~/.ssh", "nessuna chiave: importala con Sfoglia"),
		existing:   newCombo("cerca la sessione…", "nessuna sessione su questo server"),
		env:        newEnvEditor(nil),
		picked:     make(chan string, 1),
		sessionsCh: make(chan sessList, 1),
	}
	for _, e := range []*widget.Editor{&d.host, &d.port, &d.user, &d.jump, &d.command, &d.cname, &d.sessName} {
		e.SingleLine = true
	}
	d.sessName.Submit = true
	d.sessName.SetText(g.freeSessionName())
	d.sessMode.Value = "new"
	d.auth.Value = "agent"
	if u, err := rclient.CurrentUser(); err == nil {
		d.user.SetText(u)
	}
	// the shells, the default first
	var sh []comboItem
	def := autoShellName()
	g.setMu.Lock()
	if p := g.set.profile(shellName); p != nil && p.Target != "" {
		def = p.Target
	}
	kind := kindShell
	switch p := g.set.profile(g.set.Default); {
	case p == nil:
	case p.Mode == "local":
		kind = kindLocal
	case p.Mode == "remote":
		kind = kindPtscry
	case p.Mode == "ssh":
		kind = kindSSH
	case p.Mode == "standalone":
		d.persistent.Value = true
	}
	g.setMu.Unlock()
	sh = append(sh, comboItem{Label: "predefinita", Detail: def, Value: ""})
	for _, s := range availableShells() {
		sh = append(sh, comboItem{Label: s.Name, Detail: s.Cmd, Value: s.Cmd})
	}
	d.shell.SetItems(sh)
	d.shell.Choose("")
	d.keyCombo.SetItems(keyItems())
	d.savedCombo.onPick = func(it comboItem) { d.fillFrom(g, it.Value) }
	d.setKind(g, kind)
	g.setDialog(d)
}

// keyItems: the imported keys, then the usual ones of ~/.ssh.
func keyItems() []comboItem {
	var out []comboItem
	seen := map[string]bool{}
	for _, k := range listKeys() {
		seen[k.Path] = true
		det := k.Kind
		if k.Encrypted {
			det += ", con passphrase"
		}
		out = append(out, comboItem{Label: k.Name, Detail: det, Value: k.Path})
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_ed25519_sk", "id_ecdsa_sk"} {
			p := filepath.Join(home, ".ssh", n)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && !seen[p] {
				out = append(out, comboItem{Label: "~/.ssh/" + n, Detail: "chiave di ssh", Value: p})
			}
		}
	}
	return out
}

func (d *newSessionDialog) setKind(g *gui, k string) {
	d.kind = k
	d.forgetArmed = false
	d.savedCombo.Clear()
	d.listMsg, d.listFor = "", ""
	d.existing.SetItems(nil)
	d.existing.Clear()
	// the saved connections of this kind
	mode := map[string]string{kindSSH: "ssh", kindPtscry: "remote"}[k]
	var items []comboItem
	g.setMu.Lock()
	for _, p := range g.set.Profiles {
		if mode != "" && p.Mode == mode {
			det := p.Target
			if p.Jump != "" {
				det += "  via " + p.Jump
			}
			items = append(items, comboItem{Label: p.Name, Detail: det, Value: p.Name})
		}
	}
	g.setMu.Unlock()
	d.savedCombo.SetItems(items)
	// a clean form for each kind: a saved SSH connection is not a ptscry server
	d.host.SetText("")
	d.port.SetText(map[bool]string{true: "22", false: rclient.DefaultPort}[k == kindSSH])
	if u, err := rclient.CurrentUser(); err == nil {
		d.user.SetText(u)
	}
	d.cname.SetText("")
	d.keyCombo.Clear()
	d.auth.Value = "agent"
	d.jump.SetText("")
	d.command.SetText("")
	d.advanced.Value = false
	if (k == kindSSH || k == kindPtscry) && len(items) > 0 {
		d.savedCombo.open = true
	}
}

// fillFrom puts a saved connection in the fields.
func (d *newSessionDialog) fillFrom(g *gui, name string) {
	g.setMu.Lock()
	p := g.set.profile(name)
	var cp profile
	if p != nil {
		cp = *p
	}
	g.setMu.Unlock()
	if p == nil {
		return
	}
	parse := rclient.ParseTarget
	if cp.Mode == "ssh" {
		parse = parseSSHTarget
	}
	if t, err := parse(cp.Target); err == nil {
		d.host.SetText(t.Host)
		d.port.SetText(t.Port)
		if strings.Contains(cp.Target, "@") {
			d.user.SetText(t.User)
		}
	}
	d.cname.SetText(cp.Name)
	d.keyCombo.Clear()
	if cp.Identity != "" && !d.keyCombo.Choose(cp.Identity) {
		d.keyCombo.ed.SetText(cp.Identity)
	}
	d.auth.Value = cp.Auth
	if d.auth.Value == "" {
		d.auth.Value = "agent"
		if cp.Identity != "" {
			d.auth.Value = "key"
		}
	}
	d.jump.SetText(cp.Jump)
	d.command.SetText(cp.Command)
	d.advanced.Value = cp.Jump != "" || cp.Command != ""
	d.forgetArmed = false
	d.listMsg, d.listFor = "", ""
	d.existing.SetItems(nil)
}

func (d *newSessionDialog) Cancel() {}

// target is user@host:port from the fields.
func (d *newSessionDialog) target() (string, error) {
	host := strings.TrimSpace(d.host.Text())
	port := strings.TrimSpace(d.port.Text())
	user := strings.TrimSpace(d.user.Text())
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

// profileFromFields builds (and saves) the connection the fields describe.
func (d *newSessionDialog) saveProfile(g *gui) (string, error) {
	t, err := d.target()
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
		if d.advanced.Value {
			if j := strings.TrimSpace(d.jump.Text()); j != "" {
				if _, err := parseSSHTarget(j); err != nil {
					return "", fmt.Errorf("jump host: %v", err)
				}
				p.Jump = j
			}
			p.Command = strings.TrimSpace(d.command.Text())
			if strings.ContainsAny(p.Command, "\r\n") {
				return "", fmt.Errorf("il comando deve stare su una riga")
			}
		}
	} else {
		p.Identity = key
	}
	name := strings.TrimSpace(d.cname.Text())
	if name == "" {
		name = hostPortOnly(t)
		if mode == "ssh" {
			name = "ssh " + name
		}
	}
	if len(name) > 64 || strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("nome della connessione non valido")
	}
	g.setMu.Lock()
	defer g.setMu.Unlock()
	if old := g.set.profile(name); old != nil && old.Mode != mode {
		if old.Mode != "ssh" && old.Mode != "remote" {
			return "", fmt.Errorf("%q e' il nome di un profilo predefinito: scegline un altro", name)
		}
		return "", fmt.Errorf("esiste gia' una connessione %q di un altro tipo: cambia il nome", name)
	}
	p.Name = name
	if old := g.set.profile(name); old != nil {
		if old.Target == p.Target {
			p.Pinned = old.Pinned
		}
		p.Cert = old.Cert
		*old = p
	} else {
		g.set.Profiles = append(g.set.Profiles, p)
	}
	delete(g.hubs, name)
	if mode == "remote" {
		g.set.Default = name // a new tab opens there; a plain SSH login is not a good default
	}
	return name, nil
}

// sessionProfile is the profile whose sessions the session part is about.
func (d *newSessionDialog) sessionProfile(g *gui) (string, error) {
	switch d.kind {
	case kindLocal:
		return localName, nil
	case kindShell:
		return standaloneName, nil
	case kindPtscry:
		return d.saveProfile(g)
	}
	return "", fmt.Errorf("nessuna sessione ptscry per questo tipo")
}

func (d *newSessionDialog) loadSessions(g *gui) {
	name, err := d.sessionProfile(g)
	if err != nil {
		d.listMsg = err.Error()
		return
	}
	h := g.hubFor(name)
	d.listing, d.listMsg, d.listFor = true, "lettura delle sessioni…", name
	go func() {
		list, err := h.sessions(true)
		var items []comboItem
		for _, s := range list {
			det := fmt.Sprintf("%dx%d", s.Cols, s.Rows)
			if s.Attached > 0 {
				det += fmt.Sprintf(", %d collegati", s.Attached)
			}
			if s.Frozen {
				det += ", congelata"
			}
			if s.Cwd != "" {
				det += ", " + s.Cwd
			}
			if !s.Last.IsZero() {
				det += ", attiva " + s.Last.Local().Format("02/01 15:04")
			}
			items = append(items, comboItem{Label: s.Name, Detail: det, Value: s.Name})
		}
		d.sessionsCh <- sessList{key: name, items: items, err: err}
		g.invalidate()
	}()
}

func (d *newSessionDialog) open(g *gui) {
	vars, err := d.env.Values()
	if err != nil {
		g.flash("%v", err)
		return
	}
	sess := strings.TrimSpace(d.sessName.Text())
	ro := false
	if d.sessMode.Value == "attach" {
		sess = strings.TrimSpace(d.existing.Value())
		ro = d.readOnly.Value
		if sess == "" {
			g.flash("scegli la sessione a cui collegarti")
			return
		}
	}
	var sp tabSpec
	switch d.kind {
	case kindShell:
		if d.persistent.Value && runtime.GOOS != "windows" {
			sp = tabSpec{profile: standaloneName, session: sess, env: vars, ro: ro}
			break
		}
		cmd := d.shell.Value()
		if !validShellCmd(cmd) {
			g.flash("comando della shell non valido")
			return
		}
		sp = tabSpec{profile: shellName, session: g.freeSessionName(), shell: cmd}
		if it, ok := d.shell.Chosen(); ok && it.Value != "" {
			sp.label = it.Label
		}
	case kindLocal:
		sp = tabSpec{profile: localName, session: sess, env: vars, ro: ro}
	case kindPtscry:
		name, err := d.saveProfile(g)
		if err != nil {
			g.flash("%v", err)
			return
		}
		sp = tabSpec{profile: name, session: sess, env: vars, ro: ro}
	case kindSSH:
		name, err := d.saveProfile(g)
		if err != nil {
			g.flash("%v", err)
			return
		}
		h := strings.TrimSpace(d.host.Text())
		sp = tabSpec{profile: name, session: sessionNameFor(h), label: h}
	}
	g.closeDialog()
	if d.split {
		g.splitPane(true, &sp)
		return
	}
	g.openSpec(sp)
}

// sessionNameFor makes a valid session name (tab id) out of a host name.
func sessionNameFor(s string) string {
	b := []byte{}
	for i := 0; i < len(s) && len(b) < 40; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return "ssh"
	}
	return string(b)
}

func (d *newSessionDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	kinds := []struct{ k, label string }{
		{kindShell, "Terminale locale"}, {kindLocal, "Server locale"}, {kindSSH, "SSH"}, {kindPtscry, "ptscry"},
	}
	for i, k := range kinds {
		if d.kindBtn[i].Clicked(gtx) && d.kind != k.k {
			d.setKind(g, k.k)
		}
	}
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	if d.ok.Clicked(gtx) {
		d.open(g)
	}
	for {
		ev, ok := d.sessName.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			d.open(g)
		}
	}
	if d.refresh.Clicked(gtx) {
		d.loadSessions(g)
	}
	if d.forget.Clicked(gtx) {
		if it, ok := d.savedCombo.Chosen(); ok {
			if d.forgetArmed {
				g.forgetProfile(it.Value)
				d.setKind(g, d.kind)
			} else {
				d.forgetArmed = true
			}
		}
	}
	select {
	case l := <-d.sessionsCh:
		d.listing = false
		if l.key == d.listFor {
			if l.err != nil {
				d.listMsg = "elenco non disponibile: " + l.err.Error()
			} else {
				d.listMsg = map[bool]string{true: "1 sessione", false: fmt.Sprintf("%d sessioni", len(l.items))}[len(l.items) == 1]
				d.existing.SetItems(l.items)
				d.existing.open = true
			}
		}
	default:
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
		// over this dialog, which comes back when the keys are closed
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
	if !d.focusOnName {
		d.focusOnName = true
		if d.kind == kindSSH || d.kind == kindPtscry {
			gtx.Execute(key.FocusCmd{Tag: &d.savedCombo.ed})
		}
	}

	head := func(s string) layout.FlexChild {
		return g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(g.th, 13, s)
			l.Color = theme.accent
			l.Font.Weight = font.Bold
			return layout.Inset{Top: 6}.Layout(gtx, l.Layout)
		})
	}
	kids := []layout.FlexChild{
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			var ch []layout.FlexChild
			for i, k := range kinds {
				i, k := i, k
				ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return d.kindButton(gtx, g, &d.kindBtn[i], k.label, d.kind == k.k)
				}))
				if i < len(kinds)-1 {
					ch = append(ch, layout.Rigid(layout.Spacer{Width: 4}.Layout))
				}
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, ch...)
		}),
		g.row(gtx, g.label(map[string]string{
			kindShell:  "Una shell di questo computer.",
			kindLocal:  "Il server ptscry di questo computer (front-end SSH su 127.0.0.1:" + rclient.DefaultPort + "): sessioni persistenti e auditate.",
			kindSSH:    "Un terminale su un server SSH qualsiasi (non persistente: alla chiusura la shell remota termina).",
			kindPtscry: "Un server ptscry remoto: sessioni persistenti, che sopravvivono alla finestra e alla rete.",
		}[d.kind])),
	}

	switch d.kind {
	case kindShell:
		if runtime.GOOS != "windows" {
			kids = append(kids, g.row(gtx, material.CheckBox(g.th, &d.persistent, "Sessione persistente (ptscry senza server: sopravvive alla chiusura della finestra)").Layout))
		}
		if !d.persistent.Value || runtime.GOOS == "windows" {
			kids = append(kids, head("Shell"),
				g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.shell.Layout(gtx, g) }))
		} else {
			kids = append(kids, d.sessionPart(gtx, g, head)...)
		}
	case kindLocal:
		kids = append(kids, d.sessionPart(gtx, g, head)...)
	case kindSSH, kindPtscry:
		kids = append(kids, d.serverPart(gtx, g, head)...)
		if d.kind == kindPtscry {
			kids = append(kids, d.sessionPart(gtx, g, head)...)
		}
	}
	okLabel := "Apri"
	if d.kind == kindSSH {
		okLabel = "Connetti"
	}
	kids = append(kids, layout.Rigid(layout.Spacer{Height: 6}.Layout), g.row(gtx, g.buttons(&d.ok, &d.can, okLabel)))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// kindButton is one segment of the choice of the kind.
func (d *newSessionDialog) kindButton(gtx layout.Context, g *gui, c *widget.Clickable, label string, on bool) layout.Dimensions {
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		bg, fg := theme.bg, theme.chromeText
		if on {
			bg, fg = theme.accent, theme.chrome
		} else if c.Hovered() {
			bg, fg = theme.selection, theme.fg
		}
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				paint.FillShape(gtx.Ops, bg, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(5)).Op(gtx.Ops))
				return layout.Dimensions{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Inset{Top: 8, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Label(g.th, 14, label)
					l.Color = fg
					l.Alignment = text.Middle
					if on {
						l.Font.Weight = font.Bold
					}
					return l.Layout(gtx)
				})
			}),
		)
	})
}

// serverPart: saved connections, address, access, advanced options.
func (d *newSessionDialog) serverPart(gtx layout.Context, g *gui, head func(string) layout.FlexChild) []layout.FlexChild {
	kids := []layout.FlexChild{
		head("Connessioni salvate"),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return d.savedCombo.Layout(gtx, g) }),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if _, ok := d.savedCombo.Chosen(); !ok {
						return layout.Dimensions{}
					}
					label, bg := "Elimina", theme.bg
					if d.forgetArmed {
						label, bg = "Sicuro?", theme.warn
					}
					return layout.Inset{Left: 6}.Layout(gtx, g.smallButton(&d.forget, label, bg))
				}),
			)
		}),
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
					layout.Rigid(g.smallButton(&d.browse, "Importa una chiave…", theme.bg)),
					layout.Rigid(layout.Spacer{Width: 8}.Layout),
					layout.Rigid(g.smallButton(&d.manageKeys, "Elenco delle chiavi…", theme.bg)),
				)
			}),
		)
	}
	note := "La password (o la passphrase della chiave) si chiede alla connessione e non viene salvata."
	if d.kind == kindPtscry {
		note = "Chiave vuota = ssh-agent e ~/.ssh. Passphrase e codice TOTP si chiedono alla connessione e non vengono salvati."
	} else if d.auth.Value != "password" {
		note = "Se la chiave non basta, il server puo' chiedere la password. Nulla viene salvato."
	}
	kids = append(kids, g.row(gtx, small(g, note)))
	if d.kind == kindSSH {
		kids = append(kids, g.row(gtx, material.CheckBox(g.th, &d.advanced, "Opzioni avanzate (jump host, comando)").Layout))
		if d.advanced.Value {
			kids = append(kids,
				g.row(gtx, labeled(g, "Passa per (jump host, come ssh -J)", g.field(&d.jump, "utente@bastion.example.org:22"))),
				g.row(gtx, labeled(g, "Comando da eseguire al posto della shell", g.field(&d.command, "es. tmux attach || tmux"))),
			)
		}
	}
	kids = append(kids, g.row(gtx, labeled(g, "Nome della connessione (per ritrovarla)", g.field(&d.cname, map[bool]string{true: "vuoto = ssh server:porta", false: "vuoto = server:porta"}[d.kind == kindSSH]))))
	return kids
}

// sessionPart: new session or an existing one, read-only, environment.
func (d *newSessionDialog) sessionPart(gtx layout.Context, g *gui, head func(string) layout.FlexChild) []layout.FlexChild {
	kids := []layout.FlexChild{
		head("Sessione"),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.sessMode, "new", "Nuova sessione").Layout),
				layout.Rigid(layout.Spacer{Width: 14}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.sessMode, "attach", "Collegati a una sessione esistente").Layout),
			)
		}),
	}
	if d.sessMode.Value == "attach" {
		if d.listFor == "" && !d.listing {
			d.loadSessions(g)
		}
		kids = append(kids,
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return d.existing.Layout(gtx, g) }),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Rigid(g.smallButton(&d.refresh, "Aggiorna", theme.bg)),
				)
			}),
			g.row(gtx, small(g, d.listMsg)),
			g.row(gtx, material.CheckBox(g.th, &d.readOnly, "Sola lettura (guardare senza poter scrivere)").Layout),
		)
		return kids
	}
	kids = append(kids,
		g.row(gtx, labeled(g, "Nome (se esiste gia', ci si collega)", g.field(&d.sessName, "main"))),
		head("Variabili d'ambiente"),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.env.Layout(gtx, g) }),
	)
	return kids
}

func labeled(g *gui, label string, w layout.Widget) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(small(g, label)),
			layout.Rigid(layout.Spacer{Height: 2}.Layout),
			layout.Rigid(w),
		)
	}
}

func small(g *gui, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if s == "" {
			return layout.Dimensions{}
		}
		l := material.Label(g.th, 12, s)
		l.Color = theme.chromeText
		return l.Layout(gtx)
	}
}

// forgetProfile removes a saved connection (not the built-in ones).
func (g *gui) forgetProfile(name string) {
	g.setMu.Lock()
	var ps []profile
	for _, p := range g.set.Profiles {
		if p.Name == name && (p.Mode == "remote" || p.Mode == "ssh") {
			continue
		}
		ps = append(ps, p)
	}
	g.set.Profiles = ps
	delete(g.hubs, name)
	if g.set.Default == name {
		g.set.Default = defaultSettings().Default
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.flashInfo("connessione %q eliminata", name)
}
