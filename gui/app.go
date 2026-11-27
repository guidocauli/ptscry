// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// app.go: the window: tab bar, dialogs, settings and the saved workspace.

package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	gioapp "gioui.org/app"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"ptscry/internal/rclient"
)

// gui is the application.
type gui struct {
	winTitle string // the title last given to the window
	win      *gioapp.Window
	th       *material.Theme
	shaper   *text.Shaper
	// termShaper draws the terminals when their font comes from a file (nil:
	// they use shaper, with the system lookup by name)
	termShaper *text.Shaper

	setMu sync.Mutex
	set   settings

	hubs   map[string]*hub
	tabMu  sync.Mutex // protects the writes to tabs and the snapshot taken by the poller
	tabs   []*tab
	active int
	nextID int

	wake chan struct{}
	ctl  chan ctlReq // requests of the programs in the panes (OSC 7799)

	lastOSCLayout time.Time // the last layout a program asked for (OSC 7799)

	uiMu    sync.Mutex
	uiQueue []func() // work for the UI goroutine (runOnUI)

	dlgMu    sync.Mutex
	dlg      dialog
	dlgStack []dialog     // dialogs hidden under a question (password, host key)
	pending  []*promptReq // questions from the connection code, waiting for the person

	tabBtn   map[int]*widget.Clickable
	closeBtn map[int]*widget.Clickable
	plusBtn  widget.Clickable
	winState

	dlgList widget.List
	menuState
	lastSaved  time.Time
	noticeMu   sync.Mutex
	notice     string
	noticeInfo bool
	noticeAt   time.Time
	bellAt     time.Time
}

type dialog interface {
	Layout(gtx layout.Context, g *gui) layout.Dimensions
	Cancel()
}

// newTermShaper is the shaper of the terminals when the font was chosen from
// the list: its own file, then Go Mono and the other monospaced fonts of the
// system for the characters it lacks. It does not use the system lookup by
// name, which misses fonts installed after its index was built (and mixing a
// font file with it confuses the match). nil = the interface's shaper.
func newTermShaper(fontFile string) *text.Shaper {
	if fontFile == "" {
		return nil
	}
	faces, err := loadFontFile(fontFile)
	if err != nil {
		return nil
	}
	coll := append(append([]font.FontFace{}, faces...), gofont.Collection()...)
	// the fonts already listed (no scan here: it can take seconds)
	monoMu.Lock()
	others := monoCache
	monoMu.Unlock()
	n := 0
	for _, f := range others {
		if f.Path == "" || f.Path == fontFile || n >= 6 {
			continue
		}
		if ff, err := loadFontFile(f.Path); err == nil {
			coll = append(coll, ff...)
			n++
		}
	}
	return text.NewShaper(text.NoSystemFonts(), text.WithCollection(coll))
}

func newGUI(set settings) *gui {
	shaper := text.NewShaper(text.WithCollection(gofont.Collection()))
	th := material.NewTheme()
	th.Shaper = shaper
	th.Palette = material.Palette{Bg: theme.chrome, Fg: theme.chromeText, ContrastBg: theme.accent, ContrastFg: theme.bg}
	return &gui{set: set, th: th, shaper: shaper, termShaper: newTermShaper(set.FontFile), hubs: map[string]*hub{}, wake: make(chan struct{}, 1), ctl: make(chan ctlReq, 8),
		tabBtn: map[int]*widget.Clickable{}, closeBtn: map[int]*widget.Clickable{}}
}

// invalidate asks for a new frame, at most about 80 times a second.
func (g *gui) invalidate() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (g *gui) pump() {
	for range g.wake {
		if g.win != nil {
			g.win.Invalidate()
		}
		time.Sleep(12 * time.Millisecond)
	}
}

func (g *gui) bell() {
	g.noticeMu.Lock()
	g.bellAt = time.Now()
	g.noticeMu.Unlock()
	g.invalidate()
}

func (g *gui) flashInfo(format string, a ...any) { g.setNotice(true, format, a...) }

func (g *gui) flash(format string, a ...any) { g.setNotice(false, format, a...) }

// setNotice may be called from any goroutine.
func (g *gui) setNotice(info bool, format string, a ...any) {
	g.noticeMu.Lock()
	g.notice, g.noticeAt, g.noticeInfo = fmt.Sprintf(format, a...), time.Now(), info
	g.noticeMu.Unlock()
	g.invalidate()
}

func (g *gui) getNotice() (string, time.Time, bool) {
	g.noticeMu.Lock()
	defer g.noticeMu.Unlock()
	return g.notice, g.noticeAt, g.noticeInfo
}

// ---- profiles and tabs ----

func (g *gui) hubFor(name string) *hub {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	if h, ok := g.hubs[name]; ok {
		return h
	}
	p := g.set.profile(name)
	if p == nil {
		p = g.set.profile(g.set.Default)
	}
	if p == nil {
		d := defaultSettings().Profiles[0]
		p = &d
	}
	h := newHub(*p, g.promptUser)
	g.hubs[name] = h
	return h
}

func (g *gui) openTab(profileName, session string) { g.openTabAt(profileName, session, "") }

// tabSpec describes a tab to open.
type tabSpec struct {
	profile, session, cwd, label string
	shell                        string // "terminale locale": the shell command (empty = the profile's)
	ro                           bool
	env                          []envVar
	panes                        *paneState // the other panes of the tab, if split
}

func (g *gui) openTabAt(profileName, session, cwd string) {
	g.openSpec(tabSpec{profile: profileName, session: session, cwd: cwd})
}

func (g *gui) openSpec(sp tabSpec) {
	profileName, session := sp.profile, sp.session
	if profileName == "" {
		profileName = g.set.Default
	}
	if session == "" {
		session = g.freeSessionName()
	}
	if _, _, _, err := rclient.Cmdline([]string{"new", "-A", "-n", session}); err != nil || strings.HasPrefix(session, "-") {
		g.flash("nome della sessione non valido: solo lettere, cifre e _ . : = @ + , - (non all'inizio)")
		return
	}
	sp.profile, sp.session = profileName, session
	t := g.newTab(g.hubFor(profileName), sp, g.set.Scrollback)
	g.tabMu.Lock()
	g.tabs = append(g.tabs, t)
	g.tabMu.Unlock()
	g.active = len(g.tabs) - 1
	if sp.panes != nil {
		g.openPanes(t, sp.panes)
	}
	g.saveWorkspace()
	g.invalidate()
}

func (g *gui) freeSessionName() string {
	used := map[string]bool{}
	for _, t := range g.allTabs() {
		used[t.session] = true
	}
	if !used["main"] {
		return "main"
	}
	for i := 2; ; i++ {
		n := "tab" + strconv.Itoa(i)
		if !used[n] {
			return n
		}
	}
}

func (g *gui) closeTab(t *tab) {
	t = leaderOf(t)
	for i, x := range g.tabs {
		if x != t {
			continue
		}
		for _, p := range t.panes() {
			p.Close() // every pane: only a detach, the sessions go on
		}
		g.tabMu.Lock()
		g.tabs = append(g.tabs[:i:i], g.tabs[i+1:]...)
		g.tabMu.Unlock()
		delete(g.tabBtn, t.id)
		delete(g.closeBtn, t.id)
		if g.active >= len(g.tabs) {
			g.active = len(g.tabs) - 1
		}
		g.saveWorkspace()
		break
	}
	if len(g.tabs) == 0 {
		g.openTab("", "")
	}
	g.invalidate()
}

func (g *gui) cycle(d int) {
	if n := len(g.tabs); n > 1 {
		g.active = (g.active + d + n) % n
		g.saveWorkspace()
	}
}

func (g *gui) zoom(d int) {
	g.setMu.Lock()
	if d == 0 {
		g.set.FontSize = defaultSettings().FontSize
	} else {
		g.set.FontSize = min(max(g.set.FontSize+float32(d), 6), 48)
	}
	size := g.set.FontSize
	g.setMu.Unlock()
	for _, t := range g.allTabs() {
		t.view.fontSet = false
		t.view.fontPx = size
	}
	g.saveWorkspace()
	g.invalidate()
}

// saveWorkspace writes the settings and the list of tabs; it is cheap, and a
// failure is only reported once.
func (g *gui) saveWorkspace() {
	g.setMu.Lock()
	s := g.set
	g.setMu.Unlock()
	s.Tabs = nil
	g.tabMu.Lock()
	tabs := append([]*tab(nil), g.tabs...)
	g.tabMu.Unlock()
	for _, t := range tabs {
		s.Tabs = append(s.Tabs, t.state())
	}
	s.Active = g.active
	if err := s.save(); err != nil && time.Since(g.lastSaved) > time.Minute {
		g.flash("impostazioni non salvate: %v", err)
		g.lastSaved = time.Now()
	}
}

// ---- questions from the connection code (passphrase, TOTP, host key) ----

type promptAnswer struct {
	text string
	err  error
}

type promptReq struct {
	q      string
	secret bool
	reply  chan promptAnswer
}

var promptMu sync.Mutex // one question at a time

func (g *gui) promptUser(q string, secret bool) (string, error) {
	promptMu.Lock()
	defer promptMu.Unlock()
	r := &promptReq{q: q, secret: secret, reply: make(chan promptAnswer, 1)}
	g.dlgMu.Lock()
	g.pending = append(g.pending, r)
	g.dlgMu.Unlock()
	g.invalidate()
	select {
	case a := <-r.reply:
		return a.text, a.err
	case <-time.After(3 * time.Minute):
		return "", errors.New("nessuna risposta")
	}
}

// ---- the window ----

func tilingWM() bool {
	for _, k := range []string{"HYPRLAND_INSTANCE_SIGNATURE", "NIRI_SOCKET", "SWAYSOCK", "I3SOCK", "RIVER_SOCKET"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	d := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP") + ":" + os.Getenv("XDG_SESSION_DESKTOP") + ":" + os.Getenv("DESKTOP_SESSION"))
	for _, w := range []string{"niri", "hyprland", "sway", "river", "i3", "bspwm", "dwm", "awesome", "qtile", "herbstluftwm", "xmonad", "leftwm", "wayfire"} {
		if strings.Contains(d, w) {
			return true
		}
	}
	return false
}

func (g *gui) windowOptions() []gioapp.Option {
	w, h := g.set.WinW, g.set.WinH
	if w == 0 {
		w, h = 980, 620
	}
	opts := []gioapp.Option{gioapp.Title("ptscry"), gioapp.Size(unit.Dp(w), unit.Dp(h)), gioapp.MinSize(unit.Dp(160), unit.Dp(80))}
	frameless, mode := g.decorationsWanted()
	g.frameless = frameless
	gioapp.InitialDecorationMode = mode
	return append(opts, gioapp.Decorated(!frameless))
}

// decorationsWanted: whether the window has no title bar (frameless: then
// the tab bar has the window buttons and moves the window) and what to ask a
// Wayland compositor.
//   - "off":  no bar, and the compositor is asked not to draw one.
//   - "on":   always a bar. On Wayland the program draws it (client-side):
//     some compositors (Niri with prefer-no-csd) accept to decorate and then
//     draw nothing, so asking them is not enough. On X11 the window manager.
//   - "auto": none with a tiling compositor (it manages the frame), otherwise
//     the compositor's choice.
func (g *gui) decorationsWanted() (frameless bool, mode int) {
	g.setMu.Lock()
	d := g.set.Decorations
	g.setMu.Unlock()
	if !tilingPossible {
		return false, gioapp.DecorationAuto
	}
	switch d {
	case "off":
		return true, gioapp.DecorationClient
	case "on":
		return false, gioapp.DecorationClient
	}
	if tilingWM() {
		return true, gioapp.DecorationClient
	}
	return false, gioapp.DecorationAuto
}

// applyDecorations puts the title bar setting into effect now.
func (g *gui) applyDecorations() {
	frameless, mode := g.decorationsWanted()
	g.frameless = frameless
	g.win.SetDecorationMode(mode)
	g.win.Option(gioapp.Decorated(!frameless))
	g.invalidate()
}

func (g *gui) run(win *gioapp.Window) error {
	g.win = win
	go g.pump()
	go watchdog()
	go func() { // where the sessions are: saved with the workspace
		for range time.Tick(20 * time.Second) {
			g.pollCwds()
		}
	}()
	go func() { // cursor blink and the age of notices
		for { // on the edges of the blink phases (UnixMilli()/530), so that it blinks evenly
			ms := time.Now().UnixMilli()
			time.Sleep(time.Duration(530-ms%530+3) * time.Millisecond)
			g.runOnUI(g.checkActivity)
		}
	}()
	var ops op.Ops
	for {
		switch e := win.Event().(type) {
		case gioapp.DestroyEvent:
			g.pollCwds()
			g.saveWorkspace()
			for _, t := range g.allTabs() {
				t.Close() // detaches: the sessions go on
			}
			return e.Err
		case gioapp.ConfigEvent:
			g.mode = e.Config.Mode
			win.Invalidate()
		case gioapp.FrameEvent:
			gtx := gioapp.NewContext(&ops, e)
			if e.Size.X > 0 {
				g.setMu.Lock()
				g.set.WinW, g.set.WinH = int(float32(e.Size.X)/gtx.Metric.PxPerDp), int(float32(e.Size.Y)/gtx.Metric.PxPerDp)
				g.setMu.Unlock()
			}
			g.layout(gtx)
			e.Frame(gtx.Ops)
			frameDone()
		}
	}
}

func (g *gui) layout(gtx layout.Context) {
	paint.Fill(gtx.Ops, theme.chrome)
	g.dlgMu.Lock()
	if _, isPrompt := g.dlg.(*promptDialog); !isPrompt && len(g.pending) > 0 {
		if g.dlg != nil {
			g.dlgStack = append(g.dlgStack, g.dlg) // the question comes first, then back to what was open
		}
		g.dlg = newPromptDialog(g.pending[0])
		g.pending = g.pending[1:]
	}
	dlg := g.dlg
	g.dlgMu.Unlock()
	g.runQueued()
	g.takeControls()
	if len(g.tabs) == 0 {
		return
	}
	g.active = min(max(g.active, 0), len(g.tabs)-1)
	lead := g.tabs[g.active]
	cur := lead.focused()
	// only when it changes: on Windows every Option reconfigures the window
	// (SetWindowPos), which asks for a new frame, which set the title again: an
	// endless loop that froze and then crashed the program
	if title := cur.title(); title != "" && title != g.winTitle {
		g.winTitle = title
		g.win.Option(gioapp.Title(title + " - ptscry"))
	}
	// The interface (menus, tabs, dialogs) is scaled by the setting on top of the
	// DPI of the screen; the terminal keeps its own size (font size in dp).
	ugtx := gtx
	f := g.uiFactor()
	ugtx.Metric.PxPerDp *= f
	ugtx.Metric.PxPerSp *= f
	W, H := gtx.Constraints.Max.X, gtx.Constraints.Max.Y
	top := 0
	if g.menuBarOn() {
		mh := ugtx.Dp(26)
		g.menubarH = mh
		st := op.Offset(image.Pt(0, 0)).Push(ugtx.Ops)
		mg := ugtx
		mg.Constraints = layout.Exact(image.Pt(W, mh))
		g.menuBar(mg)
		st.Pop()
		top = mh
	} else {
		g.menubarH = 0
	}
	bar := ugtx.Dp(32)
	{
		st := op.Offset(image.Pt(0, top)).Push(ugtx.Ops)
		bg := ugtx
		bg.Constraints = layout.Exact(image.Pt(W, bar))
		g.tabBar(bg, bar)
		st.Pop()
	}
	top += bar
	if g.menuBarOn() {
		g.menuHot = image.Rect(0, 0, W, g.menubarH)
	} else {
		g.menuHot = image.Rect(0, 0, ugtx.Dp(34), bar)
	}
	sb := 0
	if g.statusBarOn() {
		sb = ugtx.Dp(22)
	}
	{
		st := op.Offset(image.Pt(0, top)).Push(gtx.Ops)
		tg := gtx
		tg.Constraints = layout.Exact(image.Pt(W, max(H-top-sb, 1)))
		g.layoutPanes(tg, lead, dlg == nil && !g.menu.open)
		st.Pop()
	}
	if sb > 0 {
		st := op.Offset(image.Pt(0, H-sb)).Push(ugtx.Ops)
		sg := ugtx
		sg.Constraints = layout.Exact(image.Pt(W, sb))
		g.statusBar(sg, lead, cur)
		st.Pop()
	}
	if dlg != nil {
		g.drawDialog(ugtx, dlg)
	}
	g.statusLine(ugtx)
	g.layoutMenu(ugtx, top, dlg != nil)
}

// uiFactor is the interface scale (1 = what the screen DPI gives).
func (g *gui) uiFactor() float32 {
	g.setMu.Lock()
	p := g.set.UIScale
	g.setMu.Unlock()
	if p <= 0 {
		p = 100
	}
	return float32(p) / 100
}

func (g *gui) menuBarOn() bool {
	g.setMu.Lock()
	v := g.set.MenuBar
	g.setMu.Unlock()
	return v != "off"
}

func clickable(m map[int]*widget.Clickable, id int) *widget.Clickable {
	c, ok := m[id]
	if !ok {
		c = new(widget.Clickable)
		m[id] = c
	}
	return c
}

func (g *gui) tabBar(gtx layout.Context, h int) layout.Dimensions {
	gtx.Constraints.Max.Y, gtx.Constraints.Min.Y = h, h
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, h)}.Op())
	var children []layout.FlexChild
	if !g.menuBarOn() {
		children = append(children, layout.Rigid(g.menuButton))
	}
	// width of a tab: up to 220dp, less when the bar is crowded (menu, "+", window buttons and a bit of free space to drag)
	reserved := gtx.Dp(34 + 40)
	if !g.menuBarOn() {
		reserved += gtx.Dp(34)
	}
	if g.showWindowButtons() {
		reserved += gtx.Dp(120)
	}
	tabW := max(min(gtx.Dp(220), (gtx.Constraints.Max.X-reserved)/max(len(g.tabs), 1)), gtx.Dp(60))
	for i, t := range g.tabs {
		i, t := i, t
		btn, cls := clickable(g.tabBtn, t.id), clickable(g.closeBtn, t.id)
		if btn.Clicked(gtx) {
			g.active = i
			g.saveWorkspace()
		}
		if cls.Clicked(gtx) {
			g.closeTab(t)
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, h)}
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = tabW
			return g.oneTab(gtx, t, i == g.active, btn, cls)
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if g.plusBtn.Clicked(gtx) {
				g.openNewTabDialog()
			}
			return g.iconButton(gtx, &g.plusBtn, "+")
		}),
		layout.Flexed(0.0001, g.dragArea),
	)
	if g.showWindowButtons() {
		children = append(children, layout.Rigid(g.windowButtons))
	}
	d := layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
	d.Size.Y = h
	return d
}

func (g *gui) iconButton(gtx layout.Context, b *widget.Clickable, label string) layout.Dimensions {
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := image.Pt(gtx.Dp(34), gtx.Constraints.Max.Y)
		gtx.Constraints.Min, gtx.Constraints.Max = sz, sz
		l := material.Label(g.th, 16, label)
		l.Color = theme.chromeText
		return layout.Center.Layout(gtx, l.Layout)
	})
}

func (g *gui) oneTab(gtx layout.Context, t *tab, active bool, btn, cls *widget.Clickable) layout.Dimensions {
	h := gtx.Constraints.Max.Y
	bg := theme.chrome
	if active {
		bg = theme.tabActive
	}
	paint.FillShape(gtx.Ops, bg, clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, h)}.Op())
	// the colour of the kind, along the base of the tab (brighter when active)
	kc := kindColor(t.kind())
	bh := gtx.Dp(2)
	if !active {
		kc.A = 0x90
	} else {
		bh = gtx.Dp(3)
	}
	paint.FillShape(gtx.Ops, kc, clip.Rect{Min: image.Pt(0, h-bh), Max: image.Pt(gtx.Constraints.Max.X, h)}.Op())
	return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		f := t.focused()
		title := g.activityMark(t) + f.title()
		if n := len(t.panes()); n > 1 {
			title = fmt.Sprintf("%s [%d]", title, n)
		}
		if f.ro {
			title = "[ro] " + title
		}
		st, _ := t.State()
		mark := ""
		switch st {
		case "connecting":
			mark = "… "
		case "lost", "error":
			mark = "! "
		case "ended":
			mark = "× "
		}
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				l := material.Label(g.th, 13, mark+title)
				l.MaxLines = 1
				l.Color = theme.chromeText
				if active {
					l.Color = theme.fg
				}
				if st == "lost" || st == "error" {
					l.Color = theme.warn
				}
				return layout.Inset{Left: 10, Right: 4}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return cls.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					sz := image.Pt(gtx.Dp(24), h)
					gtx.Constraints.Min, gtx.Constraints.Max = sz, sz
					l := material.Label(g.th, 14, "×")
					l.Color = theme.chromeText
					return layout.Center.Layout(gtx, l.Layout)
				})
			}),
		)
	})
}

// statusLine shows a short message at the bottom right for a few seconds, and
// the state of the connection when it is not a normal one.
func (g *gui) statusLine(gtx layout.Context) {
	msg := ""
	notice, noticeAt, noticeInfo := g.getNotice()
	if time.Since(noticeAt) < 6*time.Second {
		msg = notice
	}
	if len(g.tabs) > 0 {
		if st, d := g.curTab().State(); (st == "lost" || st == "error") && msg == "" {
			msg = d
		}
	}
	if msg == "" {
		return
	}
	sb := 0
	if g.statusBarOn() {
		sb = gtx.Dp(22)
	}
	defer op.Offset(image.Pt(0, gtx.Constraints.Max.Y-gtx.Dp(26)-sb)).Push(gtx.Ops).Pop()
	paint.FillShape(gtx.Ops, color.NRGBA{R: 0x1d, G: 0x20, B: 0x26, A: 0xe8}, clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, gtx.Dp(26))}.Op())
	l := material.Label(g.th, 12, msg)
	l.Color = theme.warn
	if noticeInfo && time.Since(noticeAt) < 6*time.Second {
		l.Color = theme.chromeText
	}
	l.MaxLines = 1
	layout.Inset{Left: 10, Top: 5}.Layout(gtx, l.Layout)
}

// ---- dialogs ----

func (g *gui) setDialog(d dialog) {
	g.dlgMu.Lock()
	g.dlg = d
	g.dlgList.Position = layout.Position{}
	g.dlgMu.Unlock()
	g.invalidate()
}

func (g *gui) closeDialog() {
	g.dlgMu.Lock()
	var next dialog
	if n := len(g.dlgStack); n > 0 {
		next, g.dlgStack = g.dlgStack[n-1], g.dlgStack[:n-1]
	}
	g.dlg = next
	g.dlgList.Position = layout.Position{}
	g.dlgMu.Unlock()
	g.invalidate()
}

func (g *gui) drawDialog(gtx layout.Context, d dialog) {
	paint.FillShape(gtx.Ops, color.NRGBA{A: 0x99}, clip.Rect{Max: gtx.Constraints.Max}.Op())
	// Escape closes it
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			d.Cancel()
			g.closeDialog()
			return
		}
	}
	outer := gtx.Constraints.Max
	width := min(gtx.Dp(520), outer.X-gtx.Dp(24))
	gtx.Constraints.Min = image.Pt(width, 0)
	gtx.Constraints.Max.X = width
	gtx.Constraints.Max.Y = max(outer.Y-gtx.Dp(16), gtx.Dp(60))
	g.dlgList.Axis = layout.Vertical
	macro := op.Record(gtx.Ops)
	dims := layout.UniformInset(18).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		// taller than the window: the dialog scrolls
		return material.List(g.th, &g.dlgList).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions { return d.Layout(gtx, g) })
	})
	call := macro.Stop()
	x, y := (outer.X-dims.Size.X)/2, max((outer.Y-dims.Size.Y)/3, gtx.Dp(8))
	defer op.Offset(image.Pt(x, y)).Push(gtx.Ops).Pop()
	paint.FillShape(gtx.Ops, theme.chrome, clip.UniformRRect(image.Rectangle{Max: dims.Size}, gtx.Dp(8)).Op(gtx.Ops))
	call.Add(gtx.Ops)
}

func (g *gui) row(gtx layout.Context, w layout.Widget) layout.FlexChild {
	return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: 10}.Layout(gtx, w)
	})
}

func (g *gui) label(s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Label(g.th, 14, s)
		l.Color = theme.chromeText
		return l.Layout(gtx)
	}
}

func (g *gui) field(ed *widget.Editor, hint string) layout.Widget {
	return g.fieldFont(ed, hint, false)
}

// fieldFont is field, optionally in the monospaced font (command output).
func (g *gui) fieldFont(ed *widget.Editor, hint string, mono bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return g.trackHover(gtx, ed, func(gtx layout.Context) layout.Dimensions {
			e := material.Editor(g.th, ed, hint)
			if mono {
				e.Font.Typeface = "Go Mono"
				e.TextSize = 12
			}
			e.Color = theme.fg
			e.HintColor = theme.chromeText
			return layout.Background{}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					sz := gtx.Constraints.Min
					paint.FillShape(gtx.Ops, theme.bg, clip.UniformRRect(image.Rectangle{Max: sz}, gtx.Dp(4)).Op(gtx.Ops))
					return layout.Dimensions{Size: sz}
				},
				func(gtx layout.Context) layout.Dimensions { return layout.UniformInset(8).Layout(gtx, e.Layout) })
		})
	}
}

func (g *gui) buttons(ok, cancel *widget.Clickable, okLabel string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				b := material.Button(g.th, cancel, "Annulla")
				b.Background, b.Color = theme.bg, theme.chromeText
				return b.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: 10}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				b := material.Button(g.th, ok, okLabel)
				b.Background, b.Color = theme.accent, theme.bg
				return b.Layout(gtx)
			}),
		)
	}
}

// -- the question dialog

type promptDialog struct {
	req       *promptReq
	ed        widget.Editor
	ok, can   widget.Clickable
	focusDone bool
	answered  bool
}

func newPromptDialog(r *promptReq) *promptDialog {
	d := &promptDialog{req: r}
	d.ed.SingleLine, d.ed.Submit = true, true
	if r.secret {
		d.ed.Mask = '•'
	}
	return d
}

func (d *promptDialog) answer(s string, err error) {
	if !d.answered {
		d.answered = true
		d.req.reply <- promptAnswer{s, err}
	}
}

func (d *promptDialog) Cancel() { d.answer("", errors.New("annullato")) }

func (d *promptDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if !d.focusDone {
		gtx.Execute(key.FocusCmd{Tag: &d.ed})
		d.focusDone = true
	}
	for {
		ev, ok := d.ed.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			d.answer(d.ed.Text(), nil)
			g.closeDialog()
		}
	}
	if d.ok.Clicked(gtx) {
		d.answer(d.ed.Text(), nil)
		g.closeDialog()
	}
	if d.can.Clicked(gtx) {
		d.Cancel()
		g.closeDialog()
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.label(d.req.q)),
		g.row(gtx, g.field(&d.ed, "")),
		g.row(gtx, g.buttons(&d.ok, &d.can, "OK")),
	)
}

// -- new tab: where to connect

// -- settings

type settingsDialog struct {
	font     widget.Editor
	face     widget.Editor
	scroll   widget.Enum
	deco     widget.Enum
	cursor   widget.Enum
	wbtn     widget.Enum
	mbar     widget.Enum
	uiscale  widget.Editor
	def      *combo
	startup  widget.Editor
	restore  widget.Bool
	ok, can  widget.Clickable
	choose   widget.Clickable
	faceFile string // file of the font chosen from the list
	shell    *combo
}

func (g *gui) openSettingsDialog() {
	d := &settingsDialog{}
	g.setMu.Lock()
	s := g.set
	g.setMu.Unlock()
	d.font.SingleLine, d.face.SingleLine, d.uiscale.SingleLine = true, true, true
	d.uiscale.SetText(strconv.Itoa(s.UIScale))
	d.mbar.Value = s.MenuBar
	d.font.SetText(strconv.FormatFloat(float64(s.FontSize), 'f', -1, 32))
	d.face.SetText(s.FontFace)
	d.faceFile = s.FontFile
	switch {
	case s.Scrollback >= scrollUnlimited:
		d.scroll.Value = "unlimited"
	case s.Scrollback >= 100000:
		d.scroll.Value = "100000"
	case s.Scrollback >= 10000:
		d.scroll.Value = "10000"
	default:
		d.scroll.Value = "1000"
	}
	d.startup.SetText(s.Startup)
	d.deco.Value = s.Decorations
	d.cursor.Value = s.Cursor
	if d.cursor.Value == "" {
		d.cursor.Value = "app"
	}
	d.wbtn.Value = s.WinButtons
	d.def = newCombo("cerca la connessione…", "nessuna connessione")
	var pitems []comboItem
	for _, p := range s.Profiles {
		pitems = append(pitems, comboItem{Label: p.Name, Detail: profileDesc(p), Value: p.Name})
	}
	d.def.SetItems(pitems)
	d.def.Choose(s.Default)
	d.restore.Value = s.Restore
	if p := s.profile(shellName); p != nil {
		d.shell = newCombo(shellHint, "nessuna shell trovata")
		items := []comboItem{{Label: "automatica", Detail: autoShellName(), Value: ""}}
		for _, s := range availableShells() {
			items = append(items, comboItem{Label: s.Name, Detail: s.Cmd, Value: s.Cmd})
		}
		d.shell.SetItems(items)
		if !d.shell.Choose(p.Target) {
			d.shell.ed.SetText(p.Target)
		}
	}
	g.setDialog(d)
}

func (d *settingsDialog) Cancel() {}

func (d *settingsDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	if d.choose.Clicked(gtx) {
		g.openFontDialog(strings.TrimSpace(d.face.Text()), func(f fontPick) { d.face.SetText(f.Name); d.faceFile = f.Path })
	}
	if d.ok.Clicked(gtx) {
		g.setMu.Lock()
		if f, err := strconv.ParseFloat(strings.TrimSpace(d.font.Text()), 32); err == nil {
			g.set.FontSize = float32(min(max(f, 6), 48))
		}
		newFace := strings.TrimSpace(d.face.Text())
		newFile := ""
		if newFace != "" && d.faceFile != "" && newFace == faceOfFile(d.faceFile) {
			newFile = d.faceFile
		}
		fontChanged := newFace != g.set.FontFace || newFile != g.set.FontFile
		switch d.scroll.Value {
		case "unlimited":
			g.set.Scrollback = scrollUnlimited
		default:
			g.set.Scrollback, _ = strconv.Atoi(d.scroll.Value)
		}
		g.set.Decorations = d.deco.Value
		g.set.Cursor = d.cursor.Value
		if g.set.Cursor == "app" {
			g.set.Cursor = ""
		}
		g.set.WinButtons = d.wbtn.Value
		g.set.MenuBar = d.mbar.Value
		if v, err := strconv.Atoi(strings.TrimSpace(d.uiscale.Text())); err == nil {
			g.set.UIScale = min(max(v, 60), 300)
		}
		if it, ok := d.def.Chosen(); ok {
			g.set.Default = it.Value
		}
		g.set.Restore = d.restore.Value
		if d.shell != nil {
			if v := d.shell.Value(); validShellCmd(v) {
				if p := g.set.profile(shellName); p != nil {
					p.Target = v
				}
			}
		}
		g.set.Startup = strings.TrimRight(d.startup.Text(), "\n ")
		size, sb := g.set.FontSize, g.set.Scrollback
		g.setMu.Unlock()
		if fontChanged {
			g.setTermFont(newFace, newFile)
		}
		for _, t := range g.allTabs() {
			t.view.fontSet, t.view.fontPx = false, size
			t.model.SetScrollback(sb)
		}
		g.saveWorkspace()
		g.closeDialog()
		g.applyDecorations()
		g.flashInfo("impostazioni salvate")
	}
	kids := []layout.FlexChild{
		g.row(gtx, g.label("Dimensione del carattere")), g.row(gtx, g.field(&d.font, "14")),
		g.row(gtx, g.label("Carattere (nome di un font a spaziatura fissa; vuoto = Go Mono)")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, g.field(&d.face, "")),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(g.smallButton(&d.choose, "Scegli dall'elenco...", theme.bg)),
			)
		}),
		g.row(gtx, g.label("Righe di scrollback")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.scroll, "1000", "1000").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.scroll, "10000", "10 000").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.scroll, "100000", "100 000").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.scroll, "unlimited", "illimitato").Layout),
			)
		}),
		g.row(gtx, g.label("Dimensione dell'interfaccia: menu, tab, finestre di dialogo (% sopra i DPI dello schermo, 60-300; 100 = normale)")), g.row(gtx, g.field(&d.uiscale, "100")),
		g.row(gtx, g.label("Barra dei menu a tendina")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(material.RadioButton(g.th, &d.mbar, "on", "sempre visibile").Layout),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(material.RadioButton(g.th, &d.mbar, "off", "nascosta (resta il pulsante ☰)").Layout),
			)
		}),
		g.row(gtx, g.label("Connessione predefinita per i nuovi tab")),
	}
	{
		at := len(kids) - 1
		extra := []layout.FlexChild{
			g.row(gtx, g.label("Cursore del terminale")),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(material.RadioButton(g.th, &d.cursor, "app", "come chiede il programma").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.cursor, "blink", "lampeggiante").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.cursor, "steady", "fisso").Layout),
				)
			}),
		}
		kids = append(kids[:at], append(extra, kids[at:]...)...)
	}
	if tilingPossible {
		// only where a tiling window manager may draw the frame (Linux, BSD):
		// Windows and macOS always have their own title bar
		at := len(kids) - 1
		extra := []layout.FlexChild{
			g.row(gtx, g.label("Barra del titolo (con Niri, Hyprland, Sway... \"auto\" la toglie)")),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(material.RadioButton(g.th, &d.deco, "auto", "automatica").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.deco, "on", "sempre").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.deco, "off", "mai").Layout),
				)
			}),
			g.row(gtx, g.label("Pulsanti riduci / ingrandisci / chiudi nella barra dei tab")),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(material.RadioButton(g.th, &d.wbtn, "auto", "solo senza barra del titolo").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.wbtn, "on", "sempre").Layout),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(material.RadioButton(g.th, &d.wbtn, "off", "mai").Layout),
				)
			}),
		}
		kids = append(kids[:at], append(extra, kids[at])...)
	}
	kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.def.Layout(gtx, g) }))
	kids = append(kids,
		layout.Rigid(layout.Spacer{Height: 8}.Layout),
		g.row(gtx, g.label("Comandi da scrivere in ogni sessione creata di nuovo (una riga ciascuno: export, alias...)")),
		g.row(gtx, g.field(&d.startup, "export EDITOR=vim")),
		g.row(gtx, material.CheckBox(g.th, &d.restore, "Riapri i tab dell'ultima volta all'avvio").Layout),
	)
	if d.shell != nil {
		kids = append(kids,
			g.row(gtx, g.label("Shell del \"terminale locale\" (ogni nuovo tab puo' sceglierne un'altra)")),
			g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.shell.Layout(gtx, g) }),
		)
	}
	kids = append(kids,
		g.row(gtx, g.buttons(&d.ok, &d.can, "Salva")),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

func (g *gui) textButton(gtx layout.Context, b *widget.Clickable, label string) layout.Dimensions {
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		l := material.Label(g.th, 13, label)
		l.Color = theme.chromeText
		return layout.Inset{Left: 8, Right: 8, Top: 8}.Layout(gtx, l.Layout)
	})
}

// state is what is saved of a tab.
func (t *tab) state() tabState {
	t.mu.Lock()
	l := t.label
	env := append([]envVar(nil), t.env...)
	t.mu.Unlock()
	return tabState{Profile: t.profile, Session: t.session, Cwd: t.getCwd(), Label: l, RO: t.ro, Env: env, Shell: t.shell, Panes: t.paneState()}
}

// loadWorkspace replaces the open tabs with those of a saved workspace; the
// sessions of the old tabs are only detached and go on running.
func (g *gui) loadWorkspace(w workspace) {
	g.tabMu.Lock()
	old := g.tabs
	g.tabs = nil
	g.tabMu.Unlock()
	for _, t := range old {
		t.Close()
	}
	for _, ts := range w.Tabs {
		g.openSpec(specOf(ts))
	}
	if len(g.tabs) == 0 {
		g.openTab("", "")
	}
	g.active = min(max(w.Active, 0), len(g.tabs)-1)
	g.saveWorkspace()
	g.invalidate()
}

// tilingPossible: the title bar settings make sense only here.
var tilingPossible = runtime.GOOS != "windows" && runtime.GOOS != "darwin"

// specOf is the tab to reopen from a saved one.
func specOf(ts tabState) tabSpec {
	return tabSpec{profile: ts.Profile, session: ts.Session, cwd: ts.Cwd, label: ts.Label, ro: ts.RO, env: ts.Env, shell: ts.Shell, panes: ts.Panes}
}

// defaultShellLabel describes the shell a "terminale locale" tab gets when
// none is chosen for it.
func defaultShellLabel(g *gui) string {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	if p := g.set.profile(shellName); p != nil && p.Target != "" {
		return "quella delle impostazioni (" + p.Target + ")"
	}
	return "quella delle impostazioni (" + autoShellName() + ")"
}

// profileDesc says in a few words what a connection is.
func profileDesc(p profile) string {
	switch p.Mode {
	case "local":
		return "server ptscry di questo computer (" + localTarget(p) + ")"
	case "standalone":
		return "questo computer, sessioni persistenti senza server"
	case "shell":
		return "una shell di questo computer (non persistente)"
	case "ssh":
		return "SSH " + p.Target
	}
	return "ptscry " + p.Target
}

// faceOfFile is the typeface name a font file gives itself ("" if unreadable).
func faceOfFile(path string) string {
	if faces, err := loadFontFile(path); err == nil && len(faces) > 0 {
		return string(faces[0].Font.Typeface)
	}
	return ""
}

// terminalShaper is the shaper the terminals use.
func (g *gui) terminalShaper() *text.Shaper {
	if g.termShaper != nil {
		return g.termShaper
	}
	return g.shaper
}

// kindColor: green = this computer, azure = ptscry server, red = plain SSH.
func kindColor(kind string) color.NRGBA {
	switch kind {
	case "local":
		return color.NRGBA{R: 0x4c, G: 0xb8, B: 0x5c, A: 0xff}
	case "ssh":
		return color.NRGBA{R: 0xe0, G: 0x5a, B: 0x50, A: 0xff}
	}
	return color.NRGBA{R: 0x4a, G: 0xaa, B: 0xea, A: 0xff}
}
