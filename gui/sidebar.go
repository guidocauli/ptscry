// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// sidebar.go: the side bar of the servers, as in RexDesk: a toolbar, a filter
// and a tree with "Questo computer", the Preferiti, the groups (nested) and
// the servers; under a ptscry server, its sessions. Double click (or Enter)
// opens; right click gives the menu; a server is dragged onto a group (or
// onto Preferiti) to move it; F2 edits, Canc removes.

package main

import (
	"fmt"
	"image"
	"image/color"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/exp/shiny/materialdesign/icons"
)

func dpOf(v int) unit.Dp { return unit.Dp(v) }

// node kinds
const (
	nodeComputer = iota // "Questo computer"
	nodeFav             // "Preferiti"
	nodeGroup
	nodeServer
	nodeSession
	nodeNote // a line of information (loading, error): not selectable
)

type treeRow struct {
	key      string
	kind     int
	depth    int
	label    string
	detail   string
	branch   bool
	open     bool
	profile  string
	mode     string // of the profile
	group    string
	session  string
	dot      color.NRGBA // state: zero = none
	hollow   bool        // the dot is a ring (a tab is open but not connected)
	swatch   color.NRGBA // colour of the server or group: zero = none
	icon     *widget.Icon
	frozen   bool
	attached bool // a tab of this window shows the session
}

type sessCache struct {
	list    []sessInfo
	at      time.Time
	loading bool
	err     string
	want    bool // the person opened the node: connect if needed
}

type sideState struct {
	tag     byte // pointer and keys of the tree
	focus   bool
	sel     string
	filter  widget.Editor
	clear   widget.Clickable
	list    widget.List
	tools   [7]widget.Clickable
	rows    []treeRow
	rowH    int
	listY   int  // where the list starts in the side bar (px)
	toolY   int  // where the toolbar is
	blocked bool // a dialog or a menu was open in the last frame
	hover   string
	hoverT  int // toolbar button under the pointer (-1 none)
	hoverS  time.Time

	down     bool
	downKey  string
	downPos  f32.Point
	lastKey  string
	lastAt   time.Time
	drag     bool
	dragPos  f32.Point
	dropKey  string
	edgeTag  byte
	resizing bool
	resizeX  float32
	resizeW  int

	mu    sync.Mutex
	sess  map[string]*sessCache
	icons map[string]*widget.Icon
}

func (s *sideState) icon(name string, data []byte) *widget.Icon {
	if s.icons == nil {
		s.icons = map[string]*widget.Icon{}
	}
	if ic, ok := s.icons[name]; ok {
		return ic
	}
	ic, err := widget.NewIcon(data)
	if err != nil {
		ic = nil
	}
	s.icons[name] = ic
	return ic
}

func (g *gui) sidebarOn() bool {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	return g.set.Sidebar != "off"
}

func (g *gui) toggleSidebar() {
	g.setMu.Lock()
	if g.set.Sidebar == "off" {
		g.set.Sidebar = "on"
	} else {
		g.set.Sidebar = "off"
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

// sidebarWidth in px (0 when hidden); the terminals keep at least 300 dp.
func (g *gui) sidebarWidth(gtx layout.Context, total int) int {
	if !g.sidebarOn() {
		return 0
	}
	g.setMu.Lock()
	w := g.set.SidebarW
	g.setMu.Unlock()
	if w == 0 {
		w = 250
	}
	px := gtx.Dp(dpOf(w))
	return max(min(px, total-gtx.Dp(300)), min(gtx.Dp(140), total/2))
}

func (g *gui) isOpen(key string) bool {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	for _, k := range g.set.Open {
		if k == key {
			return true
		}
	}
	return false
}

func (g *gui) setOpen(key string, on bool) {
	g.setMu.Lock()
	var out []string
	for _, k := range g.set.Open {
		if k != key {
			out = append(out, k)
		}
	}
	if on {
		out = append(out, key)
	}
	g.set.Open = out
	g.setMu.Unlock()
	if prof, ok := strings.CutPrefix(strings.TrimPrefix(key, "f/"), "p:"); on && ok {
		// a server opened by the person: read its sessions, connecting if needed
		g.side.mu.Lock()
		c := g.side.sess[prof]
		if c == nil {
			c = &sessCache{}
			if g.side.sess == nil {
				g.side.sess = map[string]*sessCache{}
			}
			g.side.sess[prof] = c
		}
		c.want, c.at = true, time.Time{}
		g.side.mu.Unlock()
	}
}

// loadSessions refreshes the sessions of an open server node, in the
// background: every 10 s, connecting only when the person asked.
func (g *gui) treeSessions(prof string) *sessCache {
	g.side.mu.Lock()
	defer g.side.mu.Unlock()
	if g.side.sess == nil {
		g.side.sess = map[string]*sessCache{}
	}
	c := g.side.sess[prof]
	if c == nil {
		c = &sessCache{want: true}
		g.side.sess[prof] = c
	}
	if !c.loading && time.Since(c.at) > 10*time.Second {
		c.loading = true
		connect := c.want
		c.want = false
		go func() {
			g.setMu.Lock()
			known := g.set.profile(prof) != nil
			g.setMu.Unlock()
			if !known { // removed meanwhile: hubFor would give the default connection
				g.side.mu.Lock()
				c.loading = false
				g.side.mu.Unlock()
				return
			}
			h := g.hubFor(prof) // not under side.mu: hubFor takes setMu (order: setMu, then side.mu)
			list, err := h.sessions(connect)
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
			g.side.mu.Lock()
			c.loading, c.at = false, time.Now()
			switch {
			case err == nil:
				c.list, c.err = list, ""
			case isNotConnected(err):
				if c.list == nil {
					c.err = "non connesso: apri il nodo di nuovo per collegarti"
				}
			default:
				c.err = err.Error()
			}
			g.side.mu.Unlock()
			g.invalidate()
		}()
	}
	cp := *c
	cp.list = append([]sessInfo(nil), c.list...)
	return &cp
}

// cachedSessions are the sessions already read, without reading them again.
func (g *gui) cachedSessions(prof string) *sessCache {
	g.side.mu.Lock()
	defer g.side.mu.Unlock()
	cp := sessCache{}
	if c := g.side.sess[prof]; c != nil {
		cp.list = append([]sessInfo(nil), c.list...)
		cp.at = c.at
	}
	return &cp
}

func isNotConnected(err error) bool { return err != nil && err.Error() == "EOF" }

// refreshTree reads again the sessions of the open servers.
func (g *gui) refreshTree() {
	g.side.mu.Lock()
	for _, c := range g.side.sess {
		c.at, c.want = time.Time{}, true
	}
	g.side.mu.Unlock()
	g.invalidate()
}

// builtinLabel is how the profiles every installation has show in the tree.
func builtinLabel(p profile) (string, string) {
	switch p.Mode {
	case "shell":
		return "Terminale locale", "una shell di questo computer"
	case "standalone":
		return "Sessioni senza server", "persistenti, su questo computer"
	case "local":
		return "Server locale", localTarget(p)
	}
	return p.Name, ""
}

func parseColor(s string) (color.NRGBA, bool) {
	if s == "" || !validColor(s) {
		return color.NRGBA{}, false
	}
	var c color.NRGBA
	fmt.Sscanf(s[1:], "%02x%02x%02x", &c.R, &c.G, &c.B)
	c.A = 0xff
	return c, true
}

// tabStates: for each profile, the best state of its tabs; for each
// profile+session, whether a tab shows it.
func (g *gui) tabStates() (map[string]string, map[string]bool) {
	prof := map[string]string{}
	sess := map[string]bool{}
	rank := map[string]int{"live": 3, "connecting": 2, "lost": 1, "error": 1, "ended": 0, "": 0}
	for _, t := range g.allTabs() {
		st, _ := t.State()
		if old, ok := prof[t.profile]; !ok || rank[st] > rank[old] {
			prof[t.profile] = st
		}
		sess[t.profile+"\x00"+t.sessionName()] = true
	}
	return prof, sess
}

// buildRows makes the visible rows of the tree.
func (g *gui) buildRows() []treeRow {
	s := &g.side
	g.setMu.Lock()
	profiles := append([]profile(nil), g.set.Profiles...)
	groups := append([]group(nil), g.set.Groups...)
	open := map[string]bool{}
	for _, k := range g.set.Open {
		open[k] = true
	}
	colorOf := map[string]string{}
	for _, p := range profiles {
		colorOf[p.Name] = g.set.colorOf(p)
	}
	groupColor := map[string]string{}
	for _, gr := range groups {
		groupColor[gr.Name] = g.set.colorOf(profile{Group: gr.Name})
	}
	g.setMu.Unlock()
	filter := strings.ToLower(strings.TrimSpace(s.filter.Text()))
	states, shown := g.tabStates()

	icServer := s.icon("server", icons.ActionDNS)
	icSSH := s.icon("ssh", icons.ActionSettingsEthernet)
	icShell := s.icon("shell", icons.HardwareDesktopWindows)
	icSess := s.icon("sess", icons.AVWebAsset)
	icFolder := s.icon("folder", icons.FileFolder)
	icFolderOpen := s.icon("folderOpen", icons.FileFolderOpen)
	icStar := s.icon("star", icons.ToggleStar)
	icComputer := s.icon("computer", icons.HardwareComputer)

	match := func(parts ...string) bool {
		if filter == "" {
			return true
		}
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), filter) {
				return true
			}
		}
		return false
	}
	// groups whose name matches show everything inside
	children := map[string][]group{}
	for _, gr := range groups {
		children[gr.Parent] = append(children[gr.Parent], gr)
	}
	for k := range children {
		sort.Slice(children[k], func(i, j int) bool {
			return strings.ToLower(children[k][i].Name) < strings.ToLower(children[k][j].Name)
		})
	}
	inGroup := map[string][]profile{}
	for _, p := range profiles {
		if p.Mode == "remote" || p.Mode == "ssh" {
			inGroup[p.Group] = append(inGroup[p.Group], p)
		}
	}
	for k := range inGroup {
		sort.Slice(inGroup[k], func(i, j int) bool { return strings.ToLower(inGroup[k][i].Name) < strings.ToLower(inGroup[k][j].Name) })
	}

	var rows []treeRow
	dotOf := func(name string) (color.NRGBA, bool) {
		st, ok := states[name]
		if !ok {
			return color.NRGBA{}, false
		}
		switch st {
		case "live":
			return theme.ok, false
		case "connecting":
			return theme.busy, false
		}
		return theme.disabled, true
	}

	// sessionRows: the sessions of an open server
	sessionRows := func(prefix string, p profile, depth int, onlyMatching bool) []treeRow {
		var c *sessCache
		if onlyMatching && !open[prefix+"p:"+p.Name] {
			// filtering a closed server: only the sessions already read, no connection
			c = g.cachedSessions(p.Name)
		} else {
			c = g.treeSessions(p.Name)
		}
		var out []treeRow
		for _, si := range c.list {
			if onlyMatching && !match(si.Name, si.Cwd) {
				continue
			}
			det := ""
			if si.Attached > 0 {
				det = fmt.Sprintf("%d collegat%s", si.Attached, map[bool]string{true: "o", false: "i"}[si.Attached == 1])
			}
			if si.Cwd != "" {
				if det != "" {
					det += " · "
				}
				det += si.Cwd
			}
			r := treeRow{key: prefix + "s:" + p.Name + "\x00" + si.Name, kind: nodeSession, depth: depth, label: si.Name, detail: det,
				profile: p.Name, mode: p.Mode, session: si.Name, icon: icSess, frozen: si.Frozen}
			if shown[p.Name+"\x00"+si.Name] {
				r.attached = true
				r.dot = theme.ok
			} else if si.Attached > 0 {
				r.dot, r.hollow = theme.ok, true
			}
			if si.Frozen {
				r.detail = strings.TrimPrefix("congelata · "+r.detail, " · ")
				r.detail = strings.TrimSuffix(r.detail, " · ")
			}
			out = append(out, r)
		}
		switch {
		case onlyMatching:
			// no notes for a server shown only because of its sessions
		case c.loading && len(c.list) == 0:
			out = append(out, treeRow{key: prefix + "n:" + p.Name, kind: nodeNote, depth: depth, label: "lettura delle sessioni…"})
		case c.err != "" && len(c.list) == 0:
			out = append(out, treeRow{key: prefix + "n:" + p.Name, kind: nodeNote, depth: depth, label: c.err})
		case len(c.list) == 0 && !c.at.IsZero() && !onlyMatching:
			out = append(out, treeRow{key: prefix + "n:" + p.Name, kind: nodeNote, depth: depth, label: "nessuna sessione: doppio clic sul server per crearne una"})
		}
		return out
	}

	// serverRows: a server and, when open, its sessions
	serverRows := func(prefix string, p profile, depth int, force bool) []treeRow {
		label, detail := p.Name, p.Target
		if p.Mode == "shell" || p.Mode == "standalone" || p.Mode == "local" {
			label, detail = builtinLabel(p)
		}
		if p.Mode == "ssh" {
			detail = "SSH " + hostPortOnly(p.Target)
		}
		if p.Description != "" {
			detail = p.Description
		}
		key := prefix + "p:" + p.Name
		persistent := p.persistent()
		selfMatch := force || match(p.Name, label, p.Target, p.Description)
		var subs []treeRow
		isOpen := open[key]
		if persistent && (isOpen || (filter != "" && !selfMatch)) {
			// with a filter, a server that does not match shows only the
			// sessions that match (those already read)
			if isOpen || filter != "" {
				subs = sessionRows(prefix, p, depth+1, !selfMatch)
			}
		}
		if !selfMatch && len(subs) == 0 {
			return nil
		}
		ic := icServer
		switch p.Mode {
		case "ssh":
			ic = icSSH
		case "shell":
			ic = icShell
		}
		r := treeRow{key: key, kind: nodeServer, depth: depth, label: label, detail: detail, branch: persistent,
			open: persistent && (isOpen || (!selfMatch && len(subs) > 0)), profile: p.Name, mode: p.Mode, group: p.Group, icon: ic}
		r.dot, r.hollow = dotOf(p.Name)
		if c, ok := parseColor(colorOf[p.Name]); ok {
			r.swatch = c
		}
		out := []treeRow{r}
		if r.open {
			out = append(out, subs...)
		}
		return out
	}

	// Questo computer
	{
		var subs []treeRow
		for _, p := range profiles {
			if p.Mode == "shell" || p.Mode == "standalone" || p.Mode == "local" {
				subs = append(subs, serverRows("", p, 1, match("questo computer"))...)
			}
		}
		if len(subs) > 0 || filter == "" {
			isOpen := open["c:"] || filter != ""
			rows = append(rows, treeRow{key: "c:", kind: nodeComputer, label: "Questo computer", branch: true, open: isOpen, icon: icComputer})
			if isOpen {
				rows = append(rows, subs...)
			}
		}
	}
	// Preferiti
	{
		var favs []profile
		for _, p := range profiles {
			if p.Favorite {
				favs = append(favs, p)
			}
		}
		sort.Slice(favs, func(i, j int) bool { return strings.ToLower(favs[i].Name) < strings.ToLower(favs[j].Name) })
		var subs []treeRow
		for _, p := range favs {
			subs = append(subs, serverRows("f/", p, 1, false)...)
		}
		if len(subs) > 0 || filter == "" {
			isOpen := (open["f:"] || filter != "") && len(favs) > 0
			rows = append(rows, treeRow{key: "f:", kind: nodeFav, label: "Preferiti", detail: fmt.Sprintf("(%d)", len(favs)), branch: true, open: isOpen, icon: icStar,
				swatch: color.NRGBA{R: 0xf0, G: 0xc0, B: 0x30, A: 0xff}})
			if isOpen {
				rows = append(rows, subs...)
			}
		}
	}
	// the groups, nested, then the servers without a group
	var groupRows func(parent string, depth int, force bool) []treeRow
	groupRows = func(parent string, depth int, force bool) []treeRow {
		var out []treeRow
		for _, gr := range children[parent] {
			key := "g:" + gr.Name
			selfMatch := force || match(gr.Name, gr.Description)
			var subs []treeRow
			subs = append(subs, groupRows(gr.Name, depth+1, selfMatch)...)
			for _, p := range inGroup[gr.Name] {
				subs = append(subs, serverRows("", p, depth+1, selfMatch)...)
			}
			if filter != "" && !selfMatch && len(subs) == 0 {
				continue
			}
			isOpen := open[key] || filter != ""
			n := countServers(gr.Name, children, inGroup)
			r := treeRow{key: key, kind: nodeGroup, depth: depth, label: gr.Name, detail: fmt.Sprintf("(%d)", n), branch: true, open: isOpen, group: gr.Name, icon: icFolder}
			if isOpen {
				r.icon = icFolderOpen
			}
			if gr.Description != "" {
				r.detail += "  " + gr.Description
			}
			if c, ok := parseColor(groupColor[gr.Name]); ok {
				r.swatch = c
			}
			out = append(out, r)
			if isOpen {
				out = append(out, subs...)
			}
		}
		return out
	}
	rows = append(rows, groupRows("", 0, false)...)
	for _, p := range inGroup[""] {
		rows = append(rows, serverRows("", p, 0, false)...)
	}
	return rows
}

func countServers(gr string, children map[string][]group, inGroup map[string][]profile) int {
	n := len(inGroup[gr])
	for _, c := range children[gr] {
		n += countServers(c.Name, children, inGroup)
	}
	return n
}

func (g *gui) rowByKey(key string) (treeRow, bool) {
	for _, r := range g.side.rows {
		if r.key == key {
			return r, true
		}
	}
	return treeRow{}, false
}

// selectedRow is the row the toolbar and the keys act on.
func (g *gui) selectedRow() (treeRow, bool) {
	if g.side.sel == "" {
		return treeRow{}, false
	}
	return g.rowByKey(g.side.sel)
}

// ---- drawing ----

var toolNames = [...]string{"Aggiungi server", "Nuovo gruppo", "Modifica (F2)", "Elimina (Canc)", "Importa server", "Aggiorna le sessioni", "Apri (Invio)"}

// sidebar draws the side bar; gtx.Constraints are its exact size. origin is
// its position in the window (for the context menu).
func (g *gui) sidebar(gtx layout.Context, origin image.Point) {
	s := &g.side
	sz := gtx.Constraints.Max
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: sz}.Op())
	// the right edge: a line, and a handle to change the width
	paint.FillShape(gtx.Ops, theme.line, clip.Rect{Min: image.Pt(sz.X-gtx.Dp(1), 0), Max: sz}.Op())
	g.sidebarEdge(gtx, sz)
	s.rows = g.buildRows()
	if s.sel != "" {
		if _, ok := g.rowByKey(s.sel); !ok {
			s.sel = ""
		}
	}
	for i := range s.tools {
		if s.tools[i].Clicked(gtx) {
			g.toolAction(i)
		}
	}
	if s.clear.Clicked(gtx) {
		s.filter.SetText("")
	}
	for {
		ev, ok := s.filter.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok && len(s.rows) > 0 {
			// Enter in the filter: the first server found
			for _, r := range s.rows {
				if r.kind == nodeServer || r.kind == nodeSession {
					s.sel = r.key
					gtx.Execute(key.FocusCmd{Tag: &s.tag})
					break
				}
			}
		}
	}
	s.filter.SingleLine, s.filter.Submit = true, true
	inner := gtx
	inner.Constraints = layout.Exact(image.Pt(sz.X-gtx.Dp(1), sz.Y))
	y := 0
	// title
	{
		l := material.Label(g.th, 14, "Server")
		l.Font.Weight = font.Bold
		l.Color = theme.fg
		tg := inner
		tg.Constraints.Min = image.Point{}
		d := layout.Inset{Left: 8, Top: 8, Bottom: 4}.Layout(tg, l.Layout)
		y += d.Size.Y
	}
	// toolbar
	{
		st := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
		tb := inner
		tb.Constraints = layout.Exact(image.Pt(inner.Constraints.Max.X, gtx.Dp(32)))
		g.sideToolbar(tb)
		st.Pop()
		s.toolY = y
		y += gtx.Dp(32)
	}
	// filter
	{
		st := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
		fg := inner
		fg.Constraints.Min = image.Pt(inner.Constraints.Max.X, 0)
		fg.Constraints.Max.Y = gtx.Dp(60)
		d := layout.Inset{Left: 6, Right: 6, Top: 2, Bottom: 6}.Layout(fg, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return g.trackHover(gtx, &s.filter, func(gtx layout.Context) layout.Dimensions {
						e := material.Editor(g.th, &s.filter, "Filtra server e gruppi…")
						e.TextSize = 13
						e.Color, e.HintColor = theme.fg, theme.disabled
						return layout.Background{}.Layout(gtx,
							func(gtx layout.Context) layout.Dimensions {
								paint.FillShape(gtx.Ops, theme.input, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4)).Op(gtx.Ops))
								return layout.Dimensions{Size: gtx.Constraints.Min}
							},
							func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: 8, Right: 8, Top: 6, Bottom: 6}.Layout(gtx, e.Layout)
							})
					})
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if s.filter.Text() == "" {
						return layout.Dimensions{}
					}
					return s.clear.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(4).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							sz := gtx.Dp(16)
							gtx.Constraints = layout.Exact(image.Pt(sz, sz))
							if ic := s.icon("clear", icons.ContentClear); ic != nil {
								c := theme.chromeText
								if s.clear.Hovered() {
									c = theme.fg
								}
								return ic.Layout(gtx, c)
							}
							return layout.Dimensions{Size: image.Pt(sz, sz)}
						})
					})
				}),
			)
		})
		st.Pop()
		y += d.Size.Y
	}
	s.listY = y
	// the tree
	st := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
	lg := inner
	lg.Constraints = layout.Exact(image.Pt(inner.Constraints.Max.X, max(sz.Y-y, 1)))
	g.treeList(lg, origin.Add(image.Pt(0, y)))
	st.Pop()
}

// sidebarEdge lets the person drag the right edge.
func (g *gui) sidebarEdge(gtx layout.Context, sz image.Point) {
	s := &g.side
	r := image.Rect(sz.X-gtx.Dp(4), 0, sz.X, sz.Y)
	st := clip.Rect(r).Push(gtx.Ops)
	pointer.CursorColResize.Add(gtx.Ops)
	event.Op(gtx.Ops, &s.edgeTag)
	st.Pop()
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &s.edgeTag, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			s.resizing, s.resizeX, s.resizeW = true, e.Position.X, sz.X
		case pointer.Drag:
			if s.resizing {
				w := s.resizeW + int(e.Position.X-s.resizeX)
				g.setMu.Lock()
				g.set.SidebarW = min(max(int(float32(w)/gtx.Metric.PxPerDp), 140), 700)
				g.setMu.Unlock()
				g.invalidate()
			}
		case pointer.Release, pointer.Cancel:
			if s.resizing {
				s.resizing = false
				g.saveWorkspace()
			}
		}
	}
}

func (g *gui) sideToolbar(gtx layout.Context) {
	s := &g.side
	ics := []struct {
		name string
		data []byte
	}{{"add", icons.ContentAdd}, {"newGroup", icons.FileCreateNewFolder}, {"edit", icons.EditorModeEdit}, {"delete", icons.ActionDelete},
		{"import", icons.FileFileDownload}, {"refresh", icons.NavigationRefresh}, {"open", icons.AVPlayArrow}}
	sel, haveSel := g.selectedRow()
	enabled := []bool{true, true,
		haveSel && (sel.kind == nodeGroup || sel.kind == nodeServer && (sel.mode == "remote" || sel.mode == "ssh") || sel.kind == nodeSession),
		haveSel && (sel.kind == nodeGroup || sel.kind == nodeServer && (sel.mode == "remote" || sel.mode == "ssh") || sel.kind == nodeSession),
		true, true, haveSel && (sel.kind == nodeServer || sel.kind == nodeSession)}
	var kids []layout.FlexChild
	hovered := -1
	for i, ic := range ics {
		i, ic := i, ic
		if s.tools[i].Hovered() {
			hovered = i
		}
		if i == 4 || i == 6 { // separators, as in RexDesk
			kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				h := gtx.Dp(18)
				paint.FillShape(gtx.Ops, theme.line, clip.Rect{Min: image.Pt(gtx.Dp(3), (gtx.Constraints.Max.Y-h)/2), Max: image.Pt(gtx.Dp(4), (gtx.Constraints.Max.Y+h)/2)}.Op())
				return layout.Dimensions{Size: image.Pt(gtx.Dp(7), gtx.Constraints.Max.Y)}
			}))
		}
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := &s.tools[i]
			if !enabled[i] {
				gtx = gtx.Disabled()
			}
			return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				box := image.Pt(gtx.Dp(30), gtx.Dp(28))
				if c.Hovered() && enabled[i] {
					paint.FillShape(gtx.Ops, theme.hover, clip.UniformRRect(image.Rectangle{Max: box}, gtx.Dp(4)).Op(gtx.Ops))
				}
				col := theme.fg
				if !enabled[i] {
					col = theme.disabled
					col.A = 0x90
				}
				off := op.Offset(image.Pt((box.X-gtx.Dp(18))/2, (box.Y-gtx.Dp(18))/2)).Push(gtx.Ops)
				ig := gtx
				ig.Constraints = layout.Exact(image.Pt(gtx.Dp(18), gtx.Dp(18)))
				if icn := s.icon(ic.name, ic.data); icn != nil {
					icn.Layout(ig, col)
				}
				off.Pop()
				return layout.Dimensions{Size: box}
			})
		}))
	}
	if hovered != s.hoverT {
		s.hoverT, s.hoverS = hovered, time.Now()
	}
	layout.Inset{Left: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, kids...)
	})
}

// toolTip draws the name of the toolbar button under the pointer (after a
// moment), over everything; called at the end of the frame.
func (g *gui) toolTip(gtx layout.Context, origin image.Point) {
	s := &g.side
	if s.hoverT < 0 || time.Since(s.hoverS) < 450*time.Millisecond || g.menu.open {
		return
	}
	x := origin.X + gtx.Dp(4) + s.hoverT*gtx.Dp(30)
	if s.hoverT >= 4 {
		x += gtx.Dp(7)
	}
	if s.hoverT >= 6 {
		x += gtx.Dp(7)
	}
	l := material.Label(g.th, 12, toolNames[s.hoverT])
	l.Color = theme.fg
	macro := op.Record(gtx.Ops)
	tg := gtx
	tg.Constraints.Min = image.Point{}
	d := layout.Inset{Left: 6, Right: 6, Top: 3, Bottom: 3}.Layout(tg, l.Layout)
	call := macro.Stop()
	y := origin.Y + gtx.Dp(32)
	x = min(x, gtx.Constraints.Max.X-d.Size.X-gtx.Dp(2))
	st := op.Offset(image.Pt(x, y)).Push(gtx.Ops)
	r := image.Rectangle{Max: d.Size}
	paint.FillShape(gtx.Ops, theme.line, clip.UniformRRect(r, gtx.Dp(4)).Op(gtx.Ops))
	paint.FillShape(gtx.Ops, theme.input, clip.UniformRRect(r.Inset(1), gtx.Dp(3)).Op(gtx.Ops))
	call.Add(gtx.Ops)
	st.Pop()
}

// treeList draws the rows and handles the pointer and the keys of the tree.
func (g *gui) treeList(gtx layout.Context, origin image.Point) {
	s := &g.side
	s.rowH = gtx.Dp(26)
	sz := gtx.Constraints.Max
	g.treeEvents(gtx, origin)
	// the keyboard belongs to the side bar (last click there): back to the tree
	// when a dialog or a menu opened from it closes
	g.dlgMu.Lock()
	blocked := g.dlg != nil || g.menu.open
	g.dlgMu.Unlock()
	if s.blocked && !blocked && g.kbdSide {
		gtx.Execute(key.FocusCmd{Tag: &s.tag})
	}
	s.blocked = blocked
	// one area over the whole list (on top of it, letting the scroll through):
	// clicks, double clicks, drags, the menu
	defer func() {
		area := clip.Rect(image.Rectangle{Max: sz}).Push(gtx.Ops)
		pass := pointer.PassOp{}.Push(gtx.Ops)
		event.Op(gtx.Ops, &s.tag)
		pass.Pop()
		area.Pop()
	}()
	s.list.Axis = layout.Vertical
	rows := s.rows
	if len(rows) == 0 {
		layout.UniformInset(10).Layout(gtx, small(g, "Nessun server: aggiungine uno con + o importali."))
		return
	}
	material.List(g.th, &s.list).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
		return g.treeRowView(gtx, rows[i])
	})
	// what is being dragged follows the pointer
	if s.drag && s.downKey != "" {
		if r, ok := g.rowByKey(s.downKey); ok {
			l := material.Label(g.th, 13, "→ "+r.label)
			l.Color = theme.accentFg
			macro := op.Record(gtx.Ops)
			tg := gtx
			tg.Constraints.Min = image.Point{}
			d := layout.Inset{Left: 8, Right: 8, Top: 3, Bottom: 3}.Layout(tg, l.Layout)
			call := macro.Stop()
			st := op.Offset(image.Pt(int(s.dragPos.X)+gtx.Dp(12), int(s.dragPos.Y)+gtx.Dp(4))).Push(gtx.Ops)
			paint.FillShape(gtx.Ops, theme.accent, clip.UniformRRect(image.Rectangle{Max: d.Size}, gtx.Dp(4)).Op(gtx.Ops))
			call.Add(gtx.Ops)
			st.Pop()
		}
	}
}

// rowAt is the row under y (list coordinates).
func (g *gui) rowAt(y float32) (treeRow, bool) {
	s := &g.side
	if s.rowH <= 0 || y < 0 {
		return treeRow{}, false
	}
	i := s.list.Position.First + (int(y)+s.list.Position.Offset)/s.rowH
	if i < 0 || i >= len(s.rows) {
		return treeRow{}, false
	}
	return s.rows[i], true
}

func (g *gui) treeRowView(gtx layout.Context, r treeRow) layout.Dimensions {
	s := &g.side
	w := gtx.Constraints.Max.X
	h := s.rowH
	sel := r.key == s.sel
	switch {
	case s.drag && r.key == s.dropKey:
		paint.FillShape(gtx.Ops, theme.accent, clip.UniformRRect(image.Rect(2, 1, w-2, h-1), gtx.Dp(3)).Op(gtx.Ops))
	case sel:
		c := theme.hover
		if s.focus {
			c = theme.selection
		}
		paint.FillShape(gtx.Ops, c, clip.UniformRRect(image.Rect(2, 1, w-2, h-1), gtx.Dp(3)).Op(gtx.Ops))
	case r.key == s.hover && r.kind != nodeNote:
		paint.FillShape(gtx.Ops, theme.hover, clip.UniformRRect(image.Rect(2, 1, w-2, h-1), gtx.Dp(3)).Op(gtx.Ops))
	}
	x := gtx.Dp(4) + r.depth*gtx.Dp(16)
	isz := gtx.Dp(16)
	cy := (h - isz) / 2
	drawIcon := func(ic *widget.Icon, x int, c color.NRGBA) {
		if ic == nil {
			return
		}
		st := op.Offset(image.Pt(x, cy)).Push(gtx.Ops)
		ig := gtx
		ig.Constraints = layout.Exact(image.Pt(isz, isz))
		ic.Layout(ig, c)
		st.Pop()
	}
	if r.branch {
		ic := s.icon("chevR", icons.NavigationChevronRight)
		if r.open {
			ic = s.icon("chevD", icons.NavigationExpandMore)
		}
		drawIcon(ic, x, theme.chromeText)
	}
	x += isz + gtx.Dp(2)
	if r.kind == nodeNote {
		l := material.Label(g.th, 12, r.label)
		l.Color = theme.disabled
		l.MaxLines = 1
		st := op.Offset(image.Pt(x, gtx.Dp(5))).Push(gtx.Ops)
		lg := gtx
		lg.Constraints = layout.Exact(image.Pt(max(w-x-gtx.Dp(4), 1), h))
		l.Layout(lg)
		st.Pop()
		return layout.Dimensions{Size: image.Pt(w, h)}
	}
	ic := theme.chromeText
	if r.swatch.A != 0 {
		ic = r.swatch
	}
	drawIcon(r.icon, x, ic)
	x += isz + gtx.Dp(6)
	// state: ● connected, ● connecting, ○ open but not connected
	if r.dot.A != 0 {
		d := gtx.Dp(8)
		rc := image.Rect(x, (h-d)/2, x+d, (h+d)/2)
		if r.hollow {
			paint.FillShape(gtx.Ops, r.dot, clip.Stroke{Path: clip.Ellipse(rc).Path(gtx.Ops), Width: float32(gtx.Dp(1.5))}.Op())
		} else {
			paint.FillShape(gtx.Ops, r.dot, clip.Ellipse(rc).Op(gtx.Ops))
		}
		x += d + gtx.Dp(6)
	}
	lab := material.Label(g.th, 13, r.label)
	lab.Color = theme.fg
	lab.MaxLines = 1
	if r.kind == nodeGroup || r.kind == nodeFav || r.kind == nodeComputer {
		lab.Font.Weight = font.SemiBold
	}
	if r.frozen {
		lab.Color = theme.disabled
	}
	macro := op.Record(gtx.Ops)
	lg := gtx
	lg.Constraints = layout.Constraints{Max: image.Pt(max(w-x-gtx.Dp(6), 1), h)}
	ld := lab.Layout(lg)
	call := macro.Stop()
	st := op.Offset(image.Pt(x, (h-ld.Size.Y)/2)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	st.Pop()
	x += ld.Size.X + gtx.Dp(6)
	if r.detail != "" && x < w-gtx.Dp(30) {
		dl := material.Label(g.th, 11, r.detail)
		dl.Color = theme.disabled
		dl.MaxLines = 1
		macro := op.Record(gtx.Ops)
		dg := gtx
		dg.Constraints = layout.Constraints{Max: image.Pt(w-x-gtx.Dp(6), h)}
		dd := dl.Layout(dg)
		call := macro.Stop()
		st := op.Offset(image.Pt(x, (h-dd.Size.Y)/2+1)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		st.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, h)}
}

// treeEvents: pointer and keys of the tree.
func (g *gui) treeEvents(gtx layout.Context, origin image.Point) {
	s := &g.side
	filters := []event.Filter{
		pointer.Filter{Target: &s.tag, Kinds: pointer.Press | pointer.Release | pointer.Drag | pointer.Move | pointer.Leave | pointer.Cancel},
		key.FocusFilter{Target: &s.tag},
	}
	for _, n := range []key.Name{key.NameUpArrow, key.NameDownArrow, key.NameLeftArrow, key.NameRightArrow, key.NameReturn, key.NameEnter,
		key.NameF2, key.NameDeleteForward, key.NameHome, key.NameEnd, key.NamePageUp, key.NamePageDown, key.NameF5, key.NameEscape} {
		filters = append(filters, key.Filter{Focus: &s.tag, Name: n})
	}
	filters = append(filters, key.Filter{Focus: &s.tag, Name: "F", Required: key.ModShortcut})
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		switch e := ev.(type) {
		case key.FocusEvent:
			s.focus = e.Focus
		case key.Event:
			if e.State == key.Press {
				g.treeKey(gtx, e)
			}
		case pointer.Event:
			g.treePointer(gtx, e, origin)
		}
	}
}

func (g *gui) treePointer(gtx layout.Context, e pointer.Event, origin image.Point) {
	s := &g.side
	switch e.Kind {
	case pointer.Move:
		if r, ok := g.rowAt(e.Position.Y); ok {
			s.hover = r.key
		} else {
			s.hover = ""
		}
	case pointer.Leave:
		s.hover = ""
	case pointer.Press:
		gtx.Execute(key.FocusCmd{Tag: &s.tag})
		r, ok := g.rowAt(e.Position.Y)
		if e.Buttons.Contain(pointer.ButtonSecondary) {
			if ok && r.kind != nodeNote {
				s.sel = r.key
				g.treeMenu(r, origin.Add(image.Pt(int(e.Position.X), int(e.Position.Y))))
			} else {
				g.treeMenu(treeRow{}, origin.Add(image.Pt(int(e.Position.X), int(e.Position.Y))))
			}
			return
		}
		if !e.Buttons.Contain(pointer.ButtonPrimary) {
			return
		}
		s.down, s.downPos, s.drag, s.downKey = true, e.Position, false, ""
		if !ok || r.kind == nodeNote {
			s.sel = ""
			return
		}
		s.downKey = r.key
		// the arrow opens and closes; a click on a group row too
		depthX := float32(gtx.Dp(4) + r.depth*gtx.Dp(16))
		onArrow := r.branch && e.Position.X >= depthX && e.Position.X < depthX+float32(gtx.Dp(18))
		double := s.lastKey == r.key && time.Since(s.lastAt) < 450*time.Millisecond
		s.lastKey, s.lastAt = r.key, time.Now()
		s.sel = r.key
		switch {
		case onArrow:
			g.setOpen(r.key, !r.open)
			s.lastKey = ""
		case r.kind == nodeGroup || r.kind == nodeFav || r.kind == nodeComputer:
			g.setOpen(r.key, !r.open)
		case double:
			s.lastKey = ""
			g.openRow(r)
		}
	case pointer.Drag:
		if !s.down || s.downKey == "" {
			return
		}
		if !s.drag {
			d := e.Position.Sub(s.downPos)
			if d.X*d.X+d.Y*d.Y < float32(gtx.Dp(6)*gtx.Dp(6)) {
				return
			}
			r, ok := g.rowByKey(s.downKey)
			if !ok || r.kind != nodeServer || (r.mode != "remote" && r.mode != "ssh") {
				s.down = false
				return
			}
			s.drag = true
		}
		s.dragPos = e.Position
		s.dropKey = ""
		if t, ok := g.rowAt(e.Position.Y); ok && (t.kind == nodeGroup || t.kind == nodeFav) {
			s.dropKey = t.key
		} else if e.Position.Y >= 0 && (!ok || t.kind == nodeServer && t.depth == 0) {
			s.dropKey = "root"
		}
		g.invalidate()
	case pointer.Release, pointer.Cancel:
		if s.drag && e.Kind == pointer.Release {
			g.dropOn(s.downKey, s.dropKey)
		}
		s.down, s.drag, s.dropKey = false, false, ""
	}
}

// dropOn moves a server into a group (or the top level), or marks it as a favourite.
func (g *gui) dropOn(srcKey, dst string) {
	src, ok := g.rowByKey(srcKey)
	if !ok || src.kind != nodeServer || dst == "" {
		return
	}
	g.setMu.Lock()
	p := g.set.profile(src.profile)
	if p == nil {
		g.setMu.Unlock()
		return
	}
	msg := ""
	switch {
	case dst == "f:":
		p.Favorite = true
		msg = fmt.Sprintf("%s aggiunto ai Preferiti", p.Name)
	case dst == "root":
		if p.Group == "" {
			g.setMu.Unlock()
			return
		}
		p.Group = ""
		msg = fmt.Sprintf("%s spostato fuori dai gruppi", p.Name)
	case strings.HasPrefix(dst, "g:"):
		gr := dst[2:]
		if p.Group == gr {
			g.setMu.Unlock()
			return
		}
		p.Group = gr
		msg = fmt.Sprintf("%s spostato nel gruppo %s", p.Name, gr)
	}
	g.setMu.Unlock()
	if msg != "" {
		if strings.HasPrefix(dst, "g:") {
			g.setOpen(dst, true)
		}
		g.saveWorkspace()
		g.flashInfo("%s", msg)
	}
}

func (g *gui) treeKey(gtx layout.Context, e key.Event) {
	s := &g.side
	rows := s.rows
	cur := -1
	for i, r := range rows {
		if r.key == s.sel {
			cur = i
		}
	}
	move := func(to int) {
		for to >= 0 && to < len(rows) && rows[to].kind == nodeNote {
			if to > cur {
				to++
			} else {
				to--
			}
		}
		if to >= 0 && to < len(rows) {
			s.sel = rows[to].key
			// keep it in view
			if s.rowH > 0 {
				if to < s.list.Position.First {
					s.list.Position.First, s.list.Position.Offset = to, 0
				} else if last := s.list.Position.First + s.list.Position.Count - 1; to >= last {
					s.list.Position.First = max(to-s.list.Position.Count+2, 0)
					s.list.Position.Offset = 0
				}
			}
		}
	}
	page := max(s.list.Position.Count-1, 1)
	switch e.Name {
	case key.NameDownArrow:
		move(cur + 1)
	case key.NameUpArrow:
		if cur < 0 {
			cur = len(rows)
		}
		move(cur - 1)
	case key.NamePageDown:
		move(min(cur+page, len(rows)-1))
	case key.NamePageUp:
		move(max(cur-page, 0))
	case key.NameHome:
		move(0)
	case key.NameEnd:
		move(len(rows) - 1)
	case key.NameRightArrow:
		if cur >= 0 && rows[cur].branch && !rows[cur].open {
			g.setOpen(rows[cur].key, true)
		} else {
			move(cur + 1)
		}
	case key.NameLeftArrow:
		if cur >= 0 && rows[cur].branch && rows[cur].open {
			g.setOpen(rows[cur].key, false)
		} else if cur > 0 {
			for i := cur - 1; i >= 0; i-- { // to the parent
				if rows[i].depth < rows[cur].depth {
					move(i)
					break
				}
			}
		}
	case key.NameReturn, key.NameEnter:
		if cur >= 0 {
			r := rows[cur]
			if r.kind == nodeServer || r.kind == nodeSession {
				g.openRow(r)
			} else if r.branch {
				g.setOpen(r.key, !r.open)
			}
		}
	case key.NameF2:
		if cur >= 0 {
			g.editRow(rows[cur])
		}
	case key.NameDeleteForward:
		if cur >= 0 {
			g.deleteRow(rows[cur])
		}
	case key.NameF5:
		g.refreshTree()
	case key.NameEscape:
		if s.filter.Text() != "" {
			s.filter.SetText("")
		} else {
			s.sel = ""
		}
	case "F":
		gtx.Execute(key.FocusCmd{Tag: &s.filter})
	}
	g.invalidate()
}

// ---- actions ----

func (g *gui) toolAction(i int) {
	r, ok := g.selectedRow()
	switch i {
	case 0:
		gr := ""
		if ok {
			gr = r.group
		}
		g.openServerDialog("", gr)
	case 1:
		parent := ""
		if ok && r.kind == nodeGroup {
			parent = r.group
		}
		g.openGroupDialog("", parent)
	case 2:
		if ok {
			g.editRow(r)
		}
	case 3:
		if ok {
			g.deleteRow(r)
		}
	case 4:
		g.openImportDialog()
	case 5:
		g.refreshTree()
	case 6:
		if ok {
			g.openRow(r)
		}
	}
}

// openRow: a server opens a new tab (a new session for ptscry, a shell, an
// SSH login); a session shows its tab, or attaches it.
func (g *gui) openRow(r treeRow) {
	g.home = false
	switch r.kind {
	case nodeSession:
		g.openSession(r.profile, r.session, false)
	case nodeServer:
		switch r.mode {
		case "ssh":
			g.setMu.Lock()
			p := g.set.profile(r.profile)
			host := ""
			if p != nil {
				host = hostPortOnly(p.Target)
				if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.HasSuffix(host, "]") {
					host = host[:i]
				}
			}
			g.setMu.Unlock()
			g.openSpec(tabSpec{profile: r.profile, session: sessionNameFor(host), label: host})
		default:
			g.openSpec(tabSpec{profile: r.profile, session: g.freeSessionName()})
		}
		g.logAct(false, "%s: nuovo tab", r.label)
	}
}

func (g *gui) editRow(r treeRow) {
	switch r.kind {
	case nodeServer:
		if r.mode == "remote" || r.mode == "ssh" {
			g.openServerDialog(r.profile, "")
		} else {
			g.flash("%s e' una voce predefinita: si regola in Impostazioni", r.label)
		}
	case nodeGroup:
		g.openGroupDialog(r.group, "")
	case nodeSession:
		g.openSessionRename(r.profile, r.session)
	}
}

func (g *gui) deleteRow(r treeRow) {
	switch r.kind {
	case nodeServer:
		if r.mode != "remote" && r.mode != "ssh" {
			g.flash("%s e' una voce predefinita e non si elimina", r.label)
			return
		}
		if strings.HasPrefix(r.key, "f/") {
			g.setFavorite(r.profile, false)
			return
		}
		n := 0
		for _, t := range g.allTabs() {
			if t.profile == r.profile {
				n++
			}
		}
		text := fmt.Sprintf("Eliminare il server %q dall'elenco?", r.profile)
		if r.mode == "remote" {
			text += " Le sue sessioni sul server non vengono toccate."
		}
		if n > 0 {
			text += fmt.Sprintf(" %d tab aperti restano aperti fino alla chiusura.", n)
		}
		g.confirm("Elimina il server", text, "Elimina", true, func() { g.forgetProfile(r.profile) })
	case nodeGroup:
		g.setMu.Lock()
		parent := ""
		if gr := g.set.groupByName(r.group); gr != nil {
			parent = gr.Parent
		}
		g.setMu.Unlock()
		where := "al livello principale"
		if parent != "" {
			where = "nel gruppo " + parent
		}
		g.confirm("Elimina il gruppo", fmt.Sprintf("Eliminare il gruppo %q? I suoi server e sottogruppi passano %s (non vengono eliminati).", r.group, where),
			"Elimina", true, func() { g.deleteGroup(r.group) })
	case nodeSession:
		g.confirm("Termina la sessione", fmt.Sprintf("Terminare la sessione %q su %s? I suoi programmi ricevono la chiusura (hangup) e il lavoro non salvato si perde.", r.session, r.profile),
			"Termina", true, func() { g.killSession(r.profile, r.session) })
	case nodeFav:
		g.flash("i Preferiti si svuotano togliendo i server uno per uno")
	}
}

func (g *gui) killSession(prof, name string) {
	go func() {
		if err := g.hubFor(prof).kill(name); err != nil {
			g.flash("sessione %q non terminata: %v", name, err)
		} else {
			g.logAct(false, "%s: sessione %q terminata", prof, name)
		}
		time.Sleep(600 * time.Millisecond)
		g.refreshTree()
	}()
}

func (g *gui) setFavorite(prof string, on bool) {
	g.setMu.Lock()
	if p := g.set.profile(prof); p != nil {
		p.Favorite = on
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	if on {
		g.flashInfo("%s aggiunto ai Preferiti", prof)
	} else {
		g.flashInfo("%s tolto dai Preferiti", prof)
	}
}

func (g *gui) deleteGroup(name string) {
	g.setMu.Lock()
	parent := ""
	var gs []group
	for _, gr := range g.set.Groups {
		if gr.Name == name {
			parent = gr.Parent
			continue
		}
		gs = append(gs, gr)
	}
	for i := range gs {
		if gs[i].Parent == name {
			gs[i].Parent = parent
		}
	}
	g.set.Groups = gs
	for i := range g.set.Profiles {
		if g.set.Profiles[i].Group == name {
			g.set.Profiles[i].Group = parent
		}
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.flashInfo("gruppo %q eliminato", name)
}

// treeMenu is the context menu of a row (or of the empty space).
func (g *gui) treeMenu(r treeRow, pos image.Point) {
	var items []menuItem
	sep := menuItem{sep: true}
	switch r.kind {
	case nodeServer:
		g.setMu.Lock()
		fav := false
		if p := g.set.profile(r.profile); p != nil {
			fav = p.Favorite
		}
		g.setMu.Unlock()
		own := r.mode == "remote" || r.mode == "ssh"
		openLabel := "Nuova sessione"
		switch r.mode {
		case "ssh":
			openLabel = "Connetti"
		case "shell":
			openLabel = "Nuovo terminale"
		}
		items = append(items, menuItem{label: r.label, head: true},
			menuItem{label: openLabel, enabled: true, key: "Invio", do: func(layout.Context) { g.openRow(r) }})
		if r.mode != "ssh" && r.mode != "shell" {
			items = append(items,
				menuItem{label: "Nuova sessione con nome…", enabled: true, do: func(layout.Context) { g.openNewSessionFor(r.profile) }},
				menuItem{label: "Sessioni del server…", enabled: true, do: func(layout.Context) { g.openSessionsDialogFor(r.profile) }},
				menuItem{label: "Aggiorna le sessioni", enabled: true, key: "F5", do: func(layout.Context) { g.setOpen(r.key, true); g.refreshTree() }})
		}
		items = append(items, sep,
			menuItem{label: map[bool]string{true: "Togli dai Preferiti", false: "Aggiungi ai Preferiti"}[fav], enabled: true, do: func(layout.Context) { g.setFavorite(r.profile, !fav) }})
		if own {
			items = append(items,
				menuItem{label: "Modifica…", enabled: true, key: "F2", do: func(layout.Context) { g.openServerDialog(r.profile, "") }},
				menuItem{label: "Duplica…", enabled: true, do: func(layout.Context) { g.duplicateServer(r.profile) }},
				sep,
				menuItem{label: "Elimina…", enabled: true, key: "Canc", do: func(layout.Context) {
					g.deleteRow(treeRow{kind: nodeServer, key: "p:" + r.profile, profile: r.profile, mode: r.mode})
				}})
		}
	case nodeSession:
		items = append(items, menuItem{label: r.session, head: true},
			menuItem{label: "Apri", enabled: true, key: "Invio", do: func(layout.Context) { g.openSession(r.profile, r.session, false) }},
			menuItem{label: "Apri in sola lettura", enabled: true, do: func(layout.Context) { g.openSession(r.profile, r.session, true) }},
			menuItem{label: "Apri in un nuovo pannello", enabled: len(g.tabs) > 0 && !g.home, do: func(layout.Context) {
				sp := tabSpec{profile: r.profile, session: r.session}
				g.splitPane(true, &sp)
			}},
			sep,
			menuItem{label: "Rinomina…", enabled: true, key: "F2", do: func(layout.Context) { g.openSessionRename(r.profile, r.session) }},
			menuItem{label: map[bool]string{true: "Scongela", false: "Congela"}[r.frozen], enabled: true, do: func(layout.Context) {
				g.freezeSession(r.profile, r.session, !r.frozen)
				go func() { time.Sleep(900 * time.Millisecond); g.refreshTree() }()
			}},
			sep,
			menuItem{label: "Termina…", enabled: true, key: "Canc", do: func(layout.Context) { g.deleteRow(r) }})
	case nodeGroup:
		items = append(items, menuItem{label: r.group, head: true},
			menuItem{label: "Aggiungi server nel gruppo…", enabled: true, do: func(layout.Context) { g.openServerDialog("", r.group) }},
			menuItem{label: "Nuovo sottogruppo…", enabled: true, do: func(layout.Context) { g.openGroupDialog("", r.group) }},
			menuItem{label: "Apri tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(true) }},
			menuItem{label: "Chiudi tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(false) }},
			sep,
			menuItem{label: "Modifica…", enabled: true, key: "F2", do: func(layout.Context) { g.openGroupDialog(r.group, "") }},
			menuItem{label: "Elimina il gruppo…", enabled: true, key: "Canc", do: func(layout.Context) { g.deleteRow(r) }})
	default:
		items = append(items,
			menuItem{label: "Aggiungi server…", enabled: true, do: func(layout.Context) { g.openServerDialog("", "") }},
			menuItem{label: "Nuovo gruppo…", enabled: true, do: func(layout.Context) { g.openGroupDialog("", "") }},
			menuItem{label: "Importa server…", enabled: true, do: func(layout.Context) { g.openImportDialog() }},
			sep,
			menuItem{label: "Apri tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(true) }},
			menuItem{label: "Chiudi tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(false) }})
	}
	g.menu = ctxMenu{open: true, pos: pos, items: items, clk: make([]widget.Clickable, len(items)), width: 230}
	g.invalidate()
}

func (g *gui) openAllGroups(on bool) {
	g.setMu.Lock()
	var keep []string
	for _, k := range g.set.Open {
		if !strings.HasPrefix(k, "g:") {
			keep = append(keep, k)
		}
	}
	if on {
		for _, gr := range g.set.Groups {
			keep = append(keep, "g:"+gr.Name)
		}
	}
	g.set.Open = keep
	g.setMu.Unlock()
	g.saveWorkspace()
}

// serverMenu is the "Server" menu of the menu bar, as in RexDesk.
func (g *gui) serverMenu() []menuItem {
	r, ok := g.selectedRow()
	own := ok && (r.kind == nodeGroup || r.kind == nodeSession || r.kind == nodeServer && (r.mode == "remote" || r.mode == "ssh"))
	return []menuItem{
		{label: "Aggiungi server…", enabled: true, key: sc("N"), do: func(layout.Context) { g.toolAction(0) }},
		{label: "Nuovo gruppo…", enabled: true, do: func(layout.Context) { g.toolAction(1) }},
		{label: "Importa server…", enabled: true, do: func(layout.Context) { g.openImportDialog() }},
		{sep: true},
		{label: "Apri il selezionato", enabled: ok && (r.kind == nodeServer || r.kind == nodeSession), key: "Invio", do: func(layout.Context) { g.openRow(r) }},
		{label: "Modifica il selezionato…", enabled: own, key: "F2", do: func(layout.Context) { g.editRow(r) }},
		{label: "Elimina il selezionato…", enabled: own, key: "Canc", do: func(layout.Context) { g.deleteRow(r) }},
		{sep: true},
		{label: "Aggiorna le sessioni", enabled: true, key: "F5", do: func(layout.Context) { g.refreshTree() }},
		{label: "Apri tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(true) }},
		{label: "Chiudi tutti i gruppi", enabled: true, do: func(layout.Context) { g.openAllGroups(false) }},
	}
}
