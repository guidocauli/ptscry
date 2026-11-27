// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// importdlg.go: importing servers into the side bar from RexDesk (its
// config.json: groups, user, port, access, key, jump host, colour,
// favourites) and from ~/.ssh/config (Host blocks with HostName, User, Port,
// IdentityFile, ProxyJump, Include). The files are only read; nothing is
// imported twice; passwords are never in those files and never asked.

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// importItem is a server found in a file.
type importItem struct {
	p      profile
	status string // "" new, or why it is skipped
	take   widget.Bool
}

type importSet struct {
	items  []*importItem
	groups []group
	warn   []string
}

// rexdeskPath is where RexDesk keeps its servers.
func rexdeskPath() string {
	if d := os.Getenv("REXDESK_HOME"); d != "" {
		return filepath.Join(d, "config.json")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "RexDesk", "config.json")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[min(2, len(p)):])
		}
	}
	return p
}

// the parts of RexDesk's config.json that matter here
type rdDefaults struct {
	SSHUser    string `json:"ssh_user"`
	SSHPort    int    `json:"ssh_port"`
	AuthMethod string `json:"auth_method"`
	KeyFile    string `json:"key_file"`
	CertFile   string `json:"cert_file"`
	JumpHost   string `json:"jump_host"`
	Color      string `json:"color"`
}

type rdGroup struct {
	Name        string     `json:"name"`
	Parent      string     `json:"parent"`
	Description string     `json:"description"`
	Defaults    rdDefaults `json:"defaults"`
}

type rdServer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Protocol    string   `json:"protocol"`
	Host        string   `json:"host"`
	Groups      []string `json:"groups"`
	Disabled    bool     `json:"disabled"`
	Favorite    bool     `json:"favorite"`
	rdDefaults
}

// parseRexDesk reads RexDesk's servers; asPtscry makes them ptscry servers
// (its front-end on port 2222) instead of plain SSH logins.
func parseRexDesk(data []byte, asPtscry bool) (importSet, error) {
	var cfg struct {
		Groups  []rdGroup   `json:"groups"`
		Servers []*rdServer `json:"servers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return importSet{}, fmt.Errorf("non e' un config.json di RexDesk: %v", err)
	}
	var out importSet
	gidx := map[string]*rdGroup{}
	for i := range cfg.Groups {
		gr := &cfg.Groups[i]
		if validGroupName(strings.TrimSpace(gr.Name)) {
			gidx[gr.Name] = gr
			ng := group{Name: strings.TrimSpace(gr.Name), Parent: gr.Parent, Description: cleanLine(gr.Description, 200)}
			if validColor(gr.Defaults.Color) {
				ng.Color = gr.Defaults.Color
			}
			out.groups = append(out.groups, ng)
		}
	}
	// what a server inherits from its groups (and their parents), as RexDesk does
	effective := func(s *rdServer) rdDefaults {
		e := s.rdDefaults
		seen := map[string]bool{}
		for _, gn := range s.Groups {
			for gr := gidx[gn]; gr != nil && !seen[gr.Name]; gr = gidx[gr.Parent] {
				seen[gr.Name] = true
				d := gr.Defaults
				if e.SSHUser == "" {
					e.SSHUser = d.SSHUser
				}
				if e.SSHPort == 0 {
					e.SSHPort = d.SSHPort
				}
				if e.AuthMethod == "" {
					e.AuthMethod = d.AuthMethod
				}
				if e.KeyFile == "" {
					e.KeyFile = d.KeyFile
				}
				if e.CertFile == "" {
					e.CertFile = d.CertFile
				}
				if e.JumpHost == "" {
					e.JumpHost = d.JumpHost
				}
			}
		}
		return e
	}
	byName := map[string]*rdServer{}
	for _, s := range cfg.Servers {
		if s != nil {
			byName[s.Name] = s
			byName[s.ID] = s
		}
	}
	for _, s := range cfg.Servers {
		if s == nil {
			continue
		}
		switch strings.ToLower(s.Protocol) {
		case "", "ssh", "sftp":
		default:
			out.warn = append(out.warn, fmt.Sprintf("%s: protocollo %s, non un terminale: saltato", s.Name, s.Protocol))
			continue
		}
		if s.Disabled {
			out.warn = append(out.warn, fmt.Sprintf("%s: disattivato in RexDesk: saltato", s.Name))
			continue
		}
		e := effective(s)
		port := e.SSHPort
		if port == 0 {
			port = 22
		}
		mode := "ssh"
		if asPtscry {
			mode, port = "remote", 2222
		}
		target, err := buildTarget(strings.Trim(s.Host, "[]"), strconv.Itoa(port), e.SSHUser)
		if err != nil {
			out.warn = append(out.warn, fmt.Sprintf("%s: %v", s.Name, err))
			continue
		}
		p := profile{Name: cleanLine(s.Name, 64), Mode: mode, Target: target, Description: cleanLine(s.Description, 200),
			Favorite: s.Favorite}
		if validColor(s.Color) {
			p.Color = s.Color
		}
		if p.Name == "" {
			p.Name = s.Host
		}
		if len(s.Groups) > 0 {
			p.Group = s.Groups[0]
		}
		if e.KeyFile != "" {
			p.Identity = expandHome(e.KeyFile)
		}
		if e.CertFile != "" {
			p.Cert = expandHome(e.CertFile)
		}
		if mode == "ssh" {
			switch e.AuthMethod {
			case "key", "password":
				p.Auth = e.AuthMethod
			default:
				p.Auth = "agent"
			}
			if p.Auth == "key" && p.Identity == "" {
				p.Auth = "agent"
			}
			if j := strings.TrimSpace(e.JumpHost); j != "" {
				// the name of another server of RexDesk, or user@host:port
				if js, ok := byName[j]; ok && js != s {
					je := effective(js)
					jp := je.SSHPort
					if jp == 0 {
						jp = 22
					}
					j, _ = buildTarget(strings.Trim(js.Host, "[]"), strconv.Itoa(jp), je.SSHUser)
				}
				if _, err := parseSSHTarget(j); err == nil && j != "" {
					p.Jump = j
				} else {
					out.warn = append(out.warn, fmt.Sprintf("%s: jump host %q non utilizzabile: tolto", s.Name, e.JumpHost))
				}
			}
		} else if e.JumpHost != "" {
			out.warn = append(out.warn, fmt.Sprintf("%s: il jump host non vale per i server ptscry: tolto", s.Name))
		}
		out.items = append(out.items, &importItem{p: p})
	}
	return out, nil
}

// parseSSHConfig reads the Host blocks of an ssh_config file (and of the
// files it includes); patterns (* ? !) are not servers and are skipped.
func parseSSHConfig(path string, asPtscry bool) (importSet, error) {
	var out importSet
	type block struct {
		names                            []string
		host, user, port, identity, jump string
	}
	var blocks []*block
	var cur *block
	seen := map[string]bool{}
	var read func(p string, depth int) error
	read = func(p string, depth int) error {
		if depth > 8 || seen[p] {
			return nil
		}
		seen[p] = true
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v := line, ""
			if i := strings.IndexAny(line, " \t="); i > 0 {
				k, v = line[:i], strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
			}
			v = strings.Trim(v, `"`)
			switch strings.ToLower(k) {
			case "include":
				for _, pat := range strings.Fields(v) {
					pat = expandHome(pat)
					if !filepath.IsAbs(pat) {
						pat = filepath.Join(filepath.Dir(path), pat)
					}
					ms, _ := filepath.Glob(pat)
					sort.Strings(ms)
					for _, m := range ms {
						_ = read(m, depth+1)
					}
				}
			case "host":
				cur = &block{names: strings.Fields(v)}
				blocks = append(blocks, cur)
			case "match":
				cur = nil // conditional blocks: not a server
			default:
				if cur == nil {
					continue
				}
				switch strings.ToLower(k) {
				case "hostname":
					cur.host = v
				case "user":
					cur.user = v
				case "port":
					cur.port = v
				case "identityfile":
					if cur.identity == "" {
						cur.identity = expandHome(v)
					}
				case "proxyjump":
					if cur.jump == "" && !strings.EqualFold(v, "none") {
						cur.jump = strings.Split(v, ",")[0]
					}
				}
			}
		}
		return sc.Err()
	}
	if err := read(path, 0); err != nil {
		return out, err
	}
	for _, b := range blocks {
		for _, n := range b.names {
			if strings.ContainsAny(n, "*?!") {
				continue
			}
			host := b.host
			if host == "" {
				host = n
			}
			if strings.Contains(host, "%") {
				out.warn = append(out.warn, fmt.Sprintf("%s: HostName con %%: saltato", n))
				continue
			}
			port := b.port
			if port == "" {
				port = "22"
			}
			mode := "ssh"
			if asPtscry {
				mode, port = "remote", "2222"
			}
			target, err := buildTarget(host, port, b.user)
			if err != nil {
				out.warn = append(out.warn, fmt.Sprintf("%s: %v", n, err))
				continue
			}
			p := profile{Name: cleanLine(n, 64), Mode: mode, Target: target}
			if b.identity != "" {
				p.Identity = b.identity
			}
			if mode == "ssh" {
				p.Auth = "agent"
				if p.Identity != "" {
					p.Auth = "key"
				}
				if b.jump != "" {
					if _, err := parseSSHTarget(b.jump); err == nil {
						p.Jump = b.jump
					} else {
						out.warn = append(out.warn, fmt.Sprintf("%s: ProxyJump %q non utilizzabile: tolto", n, b.jump))
					}
				}
			}
			out.items = append(out.items, &importItem{p: p})
		}
	}
	return out, nil
}

// markImport tells, for each server, whether it is already there (same kind
// and address) and gives a free name to the others.
func markImport(set *importSet, existing []profile) {
	have := map[string]bool{}
	names := map[string]bool{}
	for _, p := range existing {
		have[p.Mode+"\x00"+p.Target] = true
		names[p.Name] = true
	}
	for _, it := range set.items {
		key := it.p.Mode + "\x00" + it.p.Target
		if have[key] {
			it.status = "già presente"
			it.take.Value = false
			continue
		}
		have[key] = true
		base := it.p.Name
		for i := 2; names[it.p.Name] && i < 1000; i++ {
			it.p.Name = fmt.Sprintf("%s (%d)", base, i)
		}
		names[it.p.Name] = true
		if it.p.Name != base {
			it.status = "rinominato: " + base + " c'era già"
		}
		it.take.Value = true
	}
}

// ---- the dialog ----

type importDialog struct {
	source   widget.Enum // rexdesk, ssh, file
	as       widget.Enum // ssh, ptscry
	group    widget.Editor
	path     string
	set      importSet
	err      string
	browse   widget.Clickable
	all, non widget.Clickable
	ok, can  widget.Clickable
	picked   chan string
	loadedAs string
}

func (g *gui) openImportDialog() {
	d := &importDialog{picked: make(chan string, 1)}
	d.group.SingleLine = true
	d.as.Value = "ssh"
	d.source.Value = "rexdesk"
	if _, err := os.Stat(rexdeskPath()); err != nil {
		d.source.Value = "ssh"
	}
	d.group.SetText("")
	g.setDialog(d)
	d.load(g)
}

func (d *importDialog) Cancel() {}

func (d *importDialog) load(g *gui) {
	d.err, d.set = "", importSet{}
	var err error
	switch d.source.Value {
	case "rexdesk":
		d.path = rexdeskPath()
	case "ssh":
		if h, e := os.UserHomeDir(); e == nil {
			d.path = filepath.Join(h, ".ssh", "config")
		}
	}
	d.loadedAs = d.source.Value + "/" + d.as.Value + "/" + d.path
	if d.path == "" {
		d.err = "scegli il file con Sfoglia"
		return
	}
	st, e := os.Stat(d.path)
	switch {
	case e != nil:
		d.err = "file non trovato: " + d.path
		return
	case st.Size() > 8<<20:
		d.err = "file troppo grande: " + d.path
		return
	}
	asPtscry := d.as.Value == "ptscry"
	isRexDesk := d.source.Value == "rexdesk" || (d.source.Value == "file" && strings.HasSuffix(strings.ToLower(d.path), ".json"))
	if isRexDesk {
		var data []byte
		if data, err = os.ReadFile(d.path); err == nil {
			d.set, err = parseRexDesk(data, asPtscry)
		}
	} else {
		d.set, err = parseSSHConfig(d.path, asPtscry)
	}
	if err != nil {
		d.err = err.Error()
		return
	}
	g.setMu.Lock()
	markImport(&d.set, g.set.Profiles)
	g.setMu.Unlock()
	if len(d.set.items) == 0 && d.err == "" {
		d.err = "nessun server da importare in " + d.path
	}
}

func (d *importDialog) chosen() int {
	n := 0
	for _, it := range d.set.items {
		if it.take.Value && it.status != "già presente" {
			n++
		}
	}
	return n
}

// apply adds the chosen servers (and the groups they need).
func (d *importDialog) apply(g *gui) (int, error) {
	grp := strings.TrimSpace(d.group.Text())
	if grp != "" && !validGroupName(grp) {
		return 0, errors.New("nome del gruppo non valido")
	}
	g.setMu.Lock()
	defer g.setMu.Unlock()
	// recheck: the list may have changed while the dialog was open
	markNames := map[string]bool{}
	have := map[string]bool{}
	for _, p := range g.set.Profiles {
		markNames[p.Name] = true
		have[p.Mode+"\x00"+p.Target] = true
	}
	need := map[string]bool{}
	n := 0
	for _, it := range d.set.items {
		if !it.take.Value || it.status == "già presente" || have[it.p.Mode+"\x00"+it.p.Target] || markNames[it.p.Name] {
			continue
		}
		p := it.p
		if grp != "" {
			p.Group = grp
		}
		if p.Group != "" {
			need[p.Group] = true
		}
		g.set.Profiles = append(g.set.Profiles, p)
		markNames[p.Name], have[p.Mode+"\x00"+p.Target] = true, true
		n++
	}
	// the groups: those of the file (with their parents), then any other needed
	byName := map[string]group{}
	for _, gr := range d.set.groups {
		byName[gr.Name] = gr
	}
	var add func(name string, depth int)
	add = func(name string, depth int) {
		if name == "" || depth > 50 || g.set.groupByName(name) != nil {
			return
		}
		gr, ok := byName[name]
		if !ok {
			gr = group{Name: name}
		}
		g.set.Groups = append(g.set.Groups, gr)
		add(gr.Parent, depth+1)
	}
	for name := range need {
		add(name, 0)
	}
	g.set.Groups = cleanGroups(g.set.Groups)
	for name := range need {
		key := "g:" + name
		found := false
		for _, k := range g.set.Open {
			found = found || k == key
		}
		if !found {
			g.set.Open = append(g.set.Open, key)
		}
	}
	return n, nil
}

func (d *importDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	if d.browse.Clicked(gtx) {
		go func() {
			p, err := pickFile("Scegli il config.json di RexDesk o un file di configurazione di ssh", "")
			if err == nil && p != "" {
				d.picked <- p
				g.invalidate()
			}
		}()
	}
	select {
	case p := <-d.picked:
		d.source.Value = "file"
		d.path = p
		d.load(g)
	default:
	}
	if d.source.Update(gtx) || d.as.Update(gtx) {
		if d.source.Value == "file" && d.path == "" {
			d.err = "scegli il file con Sfoglia"
		} else {
			d.load(g)
		}
	}
	selAll, selNone := d.all.Clicked(gtx), d.non.Clicked(gtx)
	if selAll || selNone {
		on := selAll
		for _, it := range d.set.items {
			if it.status != "già presente" {
				it.take.Value = on
			}
		}
	}
	if d.ok.Clicked(gtx) {
		n, err := d.apply(g)
		if err != nil {
			g.flash("%v", err)
		} else {
			g.closeDialog()
			g.saveWorkspace()
			g.logAct(false, "importati %d server da %s", n, d.path)
			g.flashInfo("importati %d server", n)
		}
	}
	kids := []layout.FlexChild{
		g.row(gtx, g.title("Importa server")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.source, "rexdesk", "RexDesk").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.source, "ssh", "~/.ssh/config").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.source, "file", "Un altro file").Layout),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(g.smallButton(&d.browse, "Sfoglia…", theme.input)),
			)
		}),
		g.row(gtx, small(g, d.path)),
		g.row(gtx, g.heading("Come")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.as, "ssh", "Server SSH (terminale semplice)").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.as, "ptscry", "Server ptscry (front-end sulla porta 2222)").Layout),
			)
		}),
		g.row(gtx, labeled(g, "Metti tutti nel gruppo (vuoto = i gruppi del file, se ci sono)", g.field(&d.group, "es. importati"))),
	}
	if d.err != "" {
		kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(g.th, 13, d.err)
			l.Color = theme.warn
			return l.Layout(gtx)
		}))
	}
	if len(d.set.items) > 0 {
		kids = append(kids,
			g.row(gtx, g.heading(fmt.Sprintf("Server trovati: %d", len(d.set.items)))),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(g.flatButton(&d.all, "Seleziona tutti")),
					layout.Rigid(g.flatButton(&d.non, "Nessuno")),
				)
			}))
		for i, it := range d.set.items {
			if i >= 300 {
				kids = append(kids, g.row(gtx, small(g, fmt.Sprintf("… e altri %d (importati anche loro se selezionati)", len(d.set.items)-300))))
				break
			}
			it := it
			label := it.p.Name + "   " + it.p.Target
			if it.p.Group != "" && strings.TrimSpace(d.group.Text()) == "" {
				label += "   [" + it.p.Group + "]"
			}
			if it.p.Jump != "" {
				label += "   via " + it.p.Jump
			}
			if it.status != "" {
				label += "   (" + it.status + ")"
			}
			kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if it.status == "già presente" {
					gtx = gtx.Disabled()
				}
				cb := material.CheckBox(g.th, &it.take, label)
				cb.TextSize = 13
				cb.Color = theme.fg
				if it.status == "già presente" {
					cb.Color = theme.disabled
				}
				return cb.Layout(gtx)
			}))
		}
	}
	for i, w := range d.set.warn {
		if i >= 20 {
			kids = append(kids, g.row(gtx, small(g, fmt.Sprintf("… e altri %d avvisi", len(d.set.warn)-20))))
			break
		}
		kids = append(kids, g.row(gtx, small(g, "⚠ "+w)))
	}
	n := d.chosen()
	kids = append(kids, layout.Rigid(layout.Spacer{Height: 8}.Layout),
		g.row(gtx, small(g, "I file vengono solo letti. Password e passphrase non ci sono e non vengono chieste ora.")),
		g.row(gtx, g.buttons(&d.ok, &d.can, fmt.Sprintf("Importa %d server", n))))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}
