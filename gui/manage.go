// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// manage.go: "Gestione ptscry": everything ptscry does, from the window: sessions
// and jobs, the host, the configuration and the checks, the administration of
// the monitor and the service, on pages; what a profile cannot do is not there.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"ptscry/internal/rclient"
)

// what a profile can do
const (
	capAll     = iota // every profile: through the SSH front-end or the local host
	capLocal          // this computer (no server, or the server of this computer)
	capAdmin          // this computer with the monitor: root or admin_group
	capService        // this computer, Linux: the systemd service of the monitor
	capStandby        // only the profile without server: start the host
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]{0,63}$`) // not "-...": an option on a command line

// localOnly are the commands that exist only on this computer (not over SSH).
var localOnly = map[string]bool{"config": true, "check": true, "doctor": true, "admin": true, "audit-verify": true, "totp-init": true, "host": true}

// exec runs a ptscry command for the profile and returns what it printed.
func (h *hub) execCmd(args []string, d time.Duration) (string, error) {
	if len(args) == 0 {
		return "", errors.New("comando vuoto")
	}
	local := h.p.Mode == "standalone" || localOnly[args[0]]
	if local {
		if h.p.Mode == "remote" {
			return "", errors.New("questo comando vale solo per questo computer: scegli il profilo locale")
		}
		pp := h.p
		if pp.Mode != "standalone" {
			pp = profile{} // the ptscry installed here, not an address
		}
		bin, err := ptscryBinary(pp)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = cmdEnv()
		if h.p.Mode == "standalone" {
			conf, _, err := localConf()
			if err != nil {
				return "", err
			}
			cmd.Env = append(cmd.Env, "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err = cmd.Run()
		if ctx.Err() != nil {
			return out.String(), errors.New("tempo scaduto")
		}
		return out.String(), err
	}
	line, _, _, err := rclient.Cmdline(args)
	if err != nil {
		return "", err
	}
	cl, err := h.client()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	res, err := rclient.Run(cl, line, &out, &out)
	if err != nil {
		return out.String(), err
	}
	if res.Code != 0 {
		return out.String(), fmt.Errorf("uscita %d", res.Code)
	}
	return out.String(), nil
}

// ---- the actions ----

// mgAction is a button of a page; danger asks first and stands apart, in red.
type mgAction struct {
	page    string // sess, host, admin, svc
	section string
	label   string
	about   string
	cap     int
	danger  bool
	args    func(d *manageDialog) ([]string, error)
	special string // "host-start", "service:<verb>"
	reload  bool   // the list of the sessions changes
}

func needName(d *manageDialog) (string, error) {
	n := strings.TrimSpace(d.name.Text())
	if !nameRe.MatchString(n) {
		return "", errors.New("scegli una sessione dall'elenco o scrivine il nome (lettere, cifre, _ . -)")
	}
	return n, nil
}

func needUser(d *manageDialog) (string, error) {
	u := strings.TrimSpace(d.user.Text())
	if !nameRe.MatchString(u) {
		return "", errors.New("scrivi il nome dell'utente")
	}
	return u, nil
}

func cmdFields(d *manageDialog) ([]string, error) {
	f := strings.Fields(d.cmd.Text())
	if len(f) == 0 {
		return nil, errors.New("scrivi il comando da eseguire")
	}
	return f, nil
}

func simpleOp(op string) func(d *manageDialog) ([]string, error) {
	return func(d *manageDialog) ([]string, error) {
		n, err := needName(d)
		if err != nil {
			return nil, err
		}
		return []string{op, n}, nil
	}
}

func fixedArgs(args ...string) func(d *manageDialog) ([]string, error) {
	return func(*manageDialog) ([]string, error) { return args, nil }
}

// the pages of the dialog, as the settings
var mgPages = []struct{ key, label, about string }{
	{"sess", "Sessioni e job", "Le sessioni del server scelto: scegline una dall'elenco per agire su di lei, oppure creane una nuova o lancia un job."},
	{"host", "Host", "Il processo che tiene le tue sessioni su questo computer: avvio, configurazione, verifiche, arresto."},
	{"admin", "Amministrazione", "Solo con il monitor di questo computer, come root o membro di admin_group: tutti gli utenti, audit, TOTP."},
	{"svc", "Servizio", "Il servizio systemd ptscry-monitor di questo computer (senza root la conferma la chiede pkexec)."},
}

var mgActions = []mgAction{
	{page: "sess", section: "Sessione scelta", label: "Stato", about: "processi, dimensioni, client collegati", cap: capAll, args: simpleOp("status")},
	{page: "sess", section: "Sessione scelta", label: "Risultato del job", about: "aspetta la fine del job e ne mostra il codice", cap: capAll, args: simpleOp("wait")},
	{page: "sess", section: "Sessione scelta", label: "Congela", about: "ferma tutti i suoi processi (SIGSTOP)", cap: capAll, args: simpleOp("freeze"), reload: true},
	{page: "sess", section: "Sessione scelta", label: "Scongela", about: "li fa ripartire", cap: capAll, args: simpleOp("thaw"), reload: true},
	{page: "sess", section: "Sessione scelta", label: "Termina / scarta", about: "chiude la sessione: i programmi ricevono l'hangup", cap: capAll, danger: true, args: simpleOp("kill"), reload: true},
	{page: "sess", section: "Crea", label: "Nuova sessione staccata", about: "con il nome scritto sopra; il comando e' facoltativo", cap: capAll, reload: true, args: func(d *manageDialog) ([]string, error) {
		n, err := needName(d)
		if err != nil {
			return nil, err
		}
		a := []string{"new", "-d", "-n", n}
		return append(a, strings.Fields(d.cmd.Text())...), nil
	}},
	{page: "sess", section: "Crea", label: "Esegui un job", about: "il comando in una sessione staccata; il risultato resta", cap: capAll, reload: true, args: func(d *manageDialog) ([]string, error) {
		n, err := needName(d)
		if err != nil {
			return nil, err
		}
		c, err := cmdFields(d)
		if err != nil {
			return nil, err
		}
		return append([]string{"run", "-d", "-n", n}, c...), nil
	}},

	{page: "host", section: "Avvio", label: "Avvia l'host", about: "per le sessioni senza server; resta acceso chiudendo la finestra", cap: capStandby, special: "host-start"},
	{page: "host", section: "Controlli", label: "Configurazione", about: "la configurazione in uso (ptscry config)", cap: capLocal, args: fixedArgs("config")},
	{page: "host", section: "Controlli", label: "Verifica", about: "installazione, permessi, configurazione (ptscry check)", cap: capLocal, args: fixedArgs("check")},
	{page: "host", section: "Controlli", label: "Elenco delle sessioni", about: "come ptscry list", cap: capAll, args: fixedArgs("list")},
	{page: "host", section: "Arresto", label: "Ferma l'host", about: "tutte le tue sessioni su questo server terminano", cap: capAll, danger: true, args: fixedArgs("stop"), reload: true},

	{page: "admin", section: "Controlli", label: "Sessioni di tutti", about: "le sessioni di ogni utente (admin list)", cap: capAdmin, args: fixedArgs("admin", "list")},
	{page: "admin", section: "Controlli", label: "Verifica l'audit", about: "la catena firmata dei registri (audit-verify)", cap: capAdmin, args: fixedArgs("audit-verify")},
	{page: "admin", section: "Controlli", label: "Verifica l'utente", about: "i controlli del monitor per l'utente scritto sopra", cap: capAdmin, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"check", "-u", u}, nil
	}},
	{page: "admin", section: "Interventi sull'utente", label: "Registra il TOTP", about: "nuovo segreto TOTP per l'utente (sostituisce il precedente)", cap: capAdmin, danger: true, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"totp-init", u}, nil
	}},
	{page: "admin", section: "Interventi sull'utente", label: "Termina una sua sessione", about: "la sessione scritta sopra, dell'utente scritto sopra", cap: capAdmin, danger: true, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		n, err := needName(d)
		if err != nil {
			return nil, err
		}
		return []string{"admin", "kill", u, n}, nil
	}},
	{page: "admin", section: "Interventi sull'utente", label: "Ferma il suo host", about: "tutte le sessioni dell'utente terminano", cap: capAdmin, danger: true, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"admin", "stop", u}, nil
	}},
	{page: "admin", section: "Monitor", label: "Sostituisci il monitor", about: "riesegue il binario aggiornato senza chiudere le sessioni (reexec)", cap: capAdmin, danger: true, args: fixedArgs("admin", "reexec")},

	{page: "svc", section: "Stato", label: "Stato del servizio", about: "systemctl status ptscry-monitor", cap: capService, special: "service:status"},
	{page: "svc", section: "Comandi", label: "Avvia", about: "avvia il monitor", cap: capService, special: "service:start"},
	{page: "svc", section: "Comandi", label: "Rileggi la config", about: "applica le modifiche a /etc/ptscry senza fermare nulla", cap: capService, special: "service:reload"},
	{page: "svc", section: "Comandi", label: "Riavvia", about: "ferma e riavvia il monitor", cap: capService, danger: true, special: "service:restart"},
	{page: "svc", section: "Comandi", label: "Ferma", about: "nessun nuovo collegamento finche' non riparte", cap: capService, danger: true, special: "service:stop"},
}

// avail tells whether an action makes sense for the profile.
func (a mgAction) avail(mode string, admin bool) bool {
	switch a.cap {
	case capAll:
		return true
	case capLocal:
		return mode == "standalone" || mode == "local"
	case capAdmin:
		return mode == "local"
	case capService:
		return mode == "local" && runtime.GOOS == "linux"
	case capStandby:
		return mode == "standalone" && runtime.GOOS != "windows"
	}
	return false
}

// pageAvail: a page with at least one action for the profile.
func pageAvail(page, mode string) bool {
	for _, a := range mgActions {
		if a.page == page && a.avail(mode, false) {
			return true
		}
	}
	return false
}

// ---- the dialog ----

type manageDialog struct {
	g       *gui
	where   *combo
	modes   map[string]string
	prof    string // the profile of the page shown
	page    string
	pageBtn []widget.Clickable
	name    widget.Editor
	cmd     widget.Editor
	user    widget.Editor
	out     widget.Editor
	btn     []widget.Clickable
	close   widget.Clickable
	clear   widget.Clickable
	copyOut widget.Clickable
	refresh widget.Clickable
	sessL   widget.List
	sessBtn []widget.Clickable

	mu       sync.Mutex
	text     string
	changed  bool
	busy     bool
	sess     []sessInfo
	sessErr  string
	loading  bool
	loadedAt time.Time
}

func (g *gui) openManageDialog() {
	d := &manageDialog{g: g, modes: map[string]string{}, btn: make([]widget.Clickable, len(mgActions)),
		pageBtn: make([]widget.Clickable, len(mgPages)), page: "sess",
		where: newCombo("server o computer su cui agire", "nessun profilo ptscry")}
	d.name.SingleLine, d.cmd.SingleLine, d.user.SingleLine = true, true, true
	d.out.ReadOnly = true
	var items []comboItem
	g.setMu.Lock()
	for _, p := range g.set.Profiles {
		d.modes[p.Name] = p.Mode
		switch p.Mode {
		case "standalone", "local":
			l, det := builtinLabel(p)
			items = append(items, comboItem{Label: l, Detail: "questo computer · " + det, Value: p.Name})
		case "remote":
			det := p.Target
			if p.Group != "" {
				det = p.Group + " · " + det
			}
			items = append(items, comboItem{Label: p.Name, Detail: det, Value: p.Name})
		}
	}
	cur := g.set.Default
	g.setMu.Unlock()
	d.where.SetItems(items)
	if t := g.curTab(); t != nil && !g.home {
		cur = t.profile
		if d.modes[cur] != "shell" && d.modes[cur] != "ssh" {
			d.name.SetText(t.sessionName())
		}
	}
	if r, ok := g.selectedRow(); ok && (g.kbdSide || g.home) && (r.kind == nodeServer || r.kind == nodeSession) {
		cur = r.profile // working in the side bar: what is selected there
		if r.kind == nodeSession {
			d.name.SetText(r.session)
		}
	}
	if !d.where.Choose(cur) && len(items) > 0 {
		d.where.Choose(items[0].Value)
	}
	g.setDialog(d)
}

func (d *manageDialog) Cancel() {}

// dialogWidth: wider than the others, for the two columns.
func (d *manageDialog) dialogWidth() int { return 760 }

func (d *manageDialog) show(s string) {
	d.mu.Lock()
	d.text, d.changed = s, true
	d.mu.Unlock()
	d.g.invalidate()
}

// profile is the chosen server ("" when the field holds something else).
func (d *manageDialog) profile() string {
	if it, ok := d.where.Chosen(); ok {
		return it.Value
	}
	return ""
}

// loadSessions reads the sessions of the chosen server, in the background.
func (d *manageDialog) loadSessions(prof string) {
	d.mu.Lock()
	if d.loading {
		d.mu.Unlock()
		return
	}
	d.loading = true
	d.mu.Unlock()
	go func() {
		list, err := d.g.hubFor(prof).sessions(true)
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		d.mu.Lock()
		d.loading, d.loadedAt = false, time.Now()
		if d.prof == prof { // still the one shown
			d.sess, d.sessErr = list, ""
			if err != nil {
				d.sess, d.sessErr = nil, err.Error()
			}
		}
		d.mu.Unlock()
		d.g.invalidate()
	}()
}

// run executes the action in the background and shows what it printed.
func (d *manageDialog) run(a mgAction) {
	prof := d.profile()
	if prof == "" {
		d.show("scegli il server o il computer su cui agire")
		return
	}
	h := d.g.hubFor(prof)
	var args []string
	if a.special == "" {
		var err error
		if args, err = a.args(d); err != nil {
			d.show(err.Error())
			return
		}
	}
	d.mu.Lock()
	d.busy = true
	d.mu.Unlock()
	d.show(a.label + ": in corso...")
	go func() {
		defer func() {
			d.mu.Lock()
			d.busy = false
			d.mu.Unlock()
			if a.reload {
				d.loadSessions(prof)
				d.g.refreshTree()
			}
			d.g.invalidate()
		}()
		var out string
		var err error
		head := "$ ptscry " + strings.Join(args, " ")
		switch {
		case a.special == "host-start":
			head, out, err = "$ ptscry host", "", startHost(h)
			if err == nil {
				out = "host avviato"
			}
		case strings.HasPrefix(a.special, "service:"):
			verb := strings.TrimPrefix(a.special, "service:")
			head = "$ systemctl " + verb + " ptscry-monitor"
			out, err = runService(verb)
		default:
			out, err = h.execCmd(args, 90*time.Second)
		}
		res := head + "\n" + strings.TrimRight(cleanOutput(out), "\n")
		if err != nil {
			res += "\n[" + err.Error() + "]"
			d.g.logAct(true, "Gestione ptscry, %s: %s: %v", prof, a.label, err)
		} else if a.danger || a.reload {
			d.g.logAct(false, "Gestione ptscry, %s: %s", prof, a.label)
		}
		d.show(res)
	}()
}

// startHost starts the host of the profile without server and leaves it running.
func startHost(h *hub) error {
	bin, err := ptscryBinary(h.p)
	if err != nil {
		return err
	}
	conf, _, err := localConf()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "host")
	cmd.Env = append(cmdEnv(), "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	detachProc(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done: // it ended at once: already running, or an error
		if err != nil {
			return errors.New("l'host non e' partito (forse c'e' gia')")
		}
	case <-time.After(1200 * time.Millisecond):
	}
	return nil
}

// runService drives the systemd unit of the monitor; without root it asks pkexec.
func runService(verb string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("solo su Linux")
	}
	args := []string{"systemctl", verb, "ptscry-monitor"}
	if verb == "status" {
		args = []string{"systemctl", "status", "--no-pager", "ptscry-monitor"}
	} else if os.Geteuid() != 0 {
		if _, err := exec.LookPath("pkexec"); err != nil {
			return "", errors.New("servono i privilegi di root: avvia la GUI con sudo oppure installa polkit (pkexec)")
		}
		args = append([]string{"pkexec"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if verb == "status" && err != nil && out.Len() > 0 {
		err = nil // "inactive" exits non-zero: the text says it
	}
	return out.String(), err
}

func (d *manageDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.close.Clicked(gtx) {
		g.closeDialog()
	}
	if d.clear.Clicked(gtx) {
		d.show("")
	}
	if d.copyOut.Clicked(gtx) {
		d.mu.Lock()
		t := d.text
		d.mu.Unlock()
		setClipboard(gtx, t)
		g.flashQuiet("risultato copiato negli appunti")
	}
	prof := d.profile()
	mode := d.modes[prof]
	if prof != d.prof {
		// another server: its pages, its sessions
		d.mu.Lock()
		d.prof, d.sess, d.sessErr = prof, nil, ""
		d.mu.Unlock()
		if prof != "" {
			d.loadSessions(prof)
		}
		if !pageAvail(d.page, mode) {
			d.page = "sess"
		}
	}
	if d.refresh.Clicked(gtx) && prof != "" {
		d.loadSessions(prof)
	}
	for i, pg := range mgPages {
		if d.pageBtn[i].Clicked(gtx) && pageAvail(pg.key, mode) {
			d.page = pg.key
		}
	}
	d.mu.Lock()
	if d.changed {
		d.out.SetText(d.text)
		d.changed = false
	}
	busy := d.busy
	d.mu.Unlock()

	// the clicks of the buttons: the dangerous ones ask first
	for i := range mgActions {
		a := mgActions[i]
		if d.btn[i].Clicked(gtx) && !busy && prof != "" && a.avail(mode, false) {
			if a.danger {
				what := a.label + " (" + a.about + ")"
				if n := strings.TrimSpace(d.name.Text()); a.page == "sess" && n != "" {
					what = a.label + " la sessione " + n + " (" + a.about + ")"
				}
				where := prof
				if it, ok := d.where.Chosen(); ok {
					where = it.Label
				}
				g.stackDialog(&confirmDialog{head: "Confermi?", text: what + ". Dove: " + where + ".", okLabel: a.label, danger: true, do: func() { d.run(a) }})
			} else {
				d.run(a)
			}
		}
	}

	kids := []layout.FlexChild{
		g.row(gtx, g.title("Gestione ptscry")),
		g.row(gtx, labeled(g, "Dove", func(gtx layout.Context) layout.Dimensions { return d.where.Layout(gtx, g) })),
		// the pages
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			var ch []layout.FlexChild
			for i, pg := range mgPages {
				i, pg := i, pg
				ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					// not gtx.Disabled(): a Clickable laid out disabled loses
					// the first click once enabled again; a dim one ignores it
					return g.segButton(gtx, &d.pageBtn[i], pg.label, d.page == pg.key, pageAvail(pg.key, mode))
				}))
				if i < len(mgPages)-1 {
					ch = append(ch, layout.Rigid(layout.Spacer{Width: 4}.Layout))
				}
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, ch...)
		}),
	}
	about, off := "", []string{}
	for _, pg := range mgPages {
		if pg.key == d.page {
			about = pg.about
		}
		if prof != "" && !pageAvail(pg.key, mode) {
			off = append(off, pg.label)
		}
	}
	if len(off) > 0 {
		why := "per i server remoti"
		if mode == "standalone" {
			why = "senza server (servono il monitor e il servizio di questo computer)"
		}
		about += " (" + strings.Join(off, " e ") + ": non disponibil" + map[bool]string{true: "e", false: "i"}[len(off) == 1] + " " + why + ")"
	}
	kids = append(kids, g.row(gtx, small(g, about)))
	if prof == "" {
		kids = append(kids, g.row(gtx, small(g, "Scegli in \"Dove\" un server ptscry o questo computer.")))
	} else {
		switch d.page {
		case "sess":
			kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(0.42, func(gtx layout.Context) layout.Dimensions { return d.sessionList(gtx, g) }),
					layout.Rigid(layout.Spacer{Width: 14}.Layout),
					layout.Flexed(0.58, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							g.row(gtx, labeled(g, "Sessione (scelta nell'elenco o scritta qui)", g.field(&d.name, "nome della sessione"))),
							g.row(gtx, d.actionGrid(g, mode, "sess", "Sessione scelta")),
							g.row(gtx, g.heading("Crea")),
							g.row(gtx, labeled(g, "Comando (separato da spazi, senza virgolette)", g.field(&d.cmd, "es. make -j8 all"))),
							g.row(gtx, d.actionGrid(g, mode, "sess", "Crea")),
						)
					}),
				)
			}))
		case "admin":
			kids = append(kids,
				g.row(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Flexed(0.5, labeled(g, "Utente", g.field(&d.user, "nome dell'utente"))),
						layout.Rigid(layout.Spacer{Width: 10}.Layout),
						layout.Flexed(0.5, labeled(g, "Sessione (per Termina)", g.field(&d.name, "nome della sessione"))),
					)
				}))
			kids = append(kids, d.actionRows(gtx, g, mode, "admin")...)
		default:
			kids = append(kids, d.actionRows(gtx, g, mode, d.page)...)
		}
	}
	// the result, always at the bottom
	resLabel := "Risultato"
	if busy {
		resLabel = "Risultato · in corso..."
	}
	kids = append(kids,
		g.row(gtx, g.heading(resLabel)),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.Y = gtx.Dp(170)
			gtx.Constraints.Max.Y = gtx.Dp(170)
			return g.fieldFont(&d.out, "qui compare quello che il comando scrive", true)(gtx)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.copyOut, "Copia", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.clear, "Pulisci", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.accent)),
			)
		}),
	)
	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	if d.profile() != prof {
		// chosen in this frame: draw again at once with its pages (else the
		// buttons stay as they were, disabled, until something else redraws)
		gtx.Execute(op.InvalidateCmd{})
	}
	return dims
}

// sessionList: the sessions of the server; a click chooses one.
func (d *manageDialog) sessionList(gtx layout.Context, g *gui) layout.Dimensions {
	d.mu.Lock()
	list := append([]sessInfo(nil), d.sess...)
	errText, loading := d.sessErr, d.loading
	d.mu.Unlock()
	if len(d.sessBtn) < len(list) {
		d.sessBtn = append(d.sessBtn, make([]widget.Clickable, len(list)-len(d.sessBtn))...)
	}
	for i := range list {
		if d.sessBtn[i].Clicked(gtx) {
			d.name.SetText(list[i].Name)
		}
	}
	cur := strings.TrimSpace(d.name.Text())
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, g.heading(fmt.Sprintf("Sessioni (%d)", len(list)))),
				layout.Rigid(g.flatButton(&d.refresh, "Aggiorna")),
			)
		}),
		layout.Rigid(layout.Spacer{Height: 4}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			h := gtx.Dp(236)
			gtx.Constraints = layout.Exact(image.Pt(gtx.Constraints.Max.X, h))
			paint.FillShape(gtx.Ops, theme.input, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Max}, gtx.Dp(4)).Op(gtx.Ops))
			switch {
			case len(list) == 0 && loading:
				return layout.UniformInset(10).Layout(gtx, small(g, "lettura delle sessioni..."))
			case len(list) == 0 && errText != "":
				return layout.UniformInset(10).Layout(gtx, small(g, "non lette: "+errText))
			case len(list) == 0:
				return layout.UniformInset(10).Layout(gtx, small(g, "nessuna sessione: creane una a destra"))
			}
			d.sessL.Axis = layout.Vertical
			return layout.UniformInset(3).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.List(g.th, &d.sessL).Layout(gtx, len(list), func(gtx layout.Context, i int) layout.Dimensions {
					si := list[i]
					c := &d.sessBtn[i]
					return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Background{}.Layout(gtx,
							func(gtx layout.Context) layout.Dimensions {
								switch {
								case si.Name == cur:
									paint.FillShape(gtx.Ops, theme.selection, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(3)).Op(gtx.Ops))
								case c.Hovered():
									paint.FillShape(gtx.Ops, theme.hover, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(3)).Op(gtx.Ops))
								}
								return layout.Dimensions{Size: gtx.Constraints.Min}
							},
							func(gtx layout.Context) layout.Dimensions {
								det := ""
								switch {
								case si.Frozen:
									det = "congelata"
								case si.Attached > 0:
									det = fmt.Sprintf("%d collegat%s", si.Attached, map[bool]string{true: "o", false: "i"}[si.Attached == 1])
								default:
									det = "staccata"
								}
								if si.Cwd != "" {
									det += " · " + si.Cwd
								}
								return layout.Inset{Left: 8, Right: 6, Top: 4, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											l := material.Label(g.th, 13, si.Name)
											l.Color = theme.fg
											l.Font.Weight = font.SemiBold
											l.MaxLines = 1
											return l.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											l := material.Label(g.th, 11, det)
											l.Color = theme.disabled
											l.MaxLines = 1
											return l.Layout(gtx)
										}),
									)
								})
							})
					})
				})
			})
		}),
	)
}

// actionGrid: the buttons of a section, in a row (the dangerous one last, red).
func (d *manageDialog) actionGrid(g *gui, mode, page, section string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		var ch []layout.FlexChild
		for i := range mgActions {
			i, a := i, mgActions[i]
			if a.page != page || a.section != section || !a.avail(mode, false) {
				continue
			}
			bg := theme.input
			if a.danger {
				bg = theme.warn
				ch = append(ch, layout.Flexed(1, layout.Spacer{}.Layout)) // apart, on the right
			}
			ch = append(ch, layout.Rigid(g.smallButton(&d.btn[i], a.label, bg)), layout.Rigid(layout.Spacer{Width: 6}.Layout))
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, ch...)
	}
}

// actionRows: one row per action, the button and what it does, by section;
// the dangerous ones in red.
func (d *manageDialog) actionRows(gtx layout.Context, g *gui, mode, page string) []layout.FlexChild {
	var kids []layout.FlexChild
	section := ""
	for i := range mgActions {
		i, a := i, mgActions[i]
		if a.page != page || !a.avail(mode, false) {
			continue
		}
		if a.section != section {
			section = a.section
			kids = append(kids, g.row(gtx, g.heading(section)))
		}
		bg := theme.input
		if a.danger {
			bg = theme.warn
		}
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Dp(210)
						gtx.Constraints.Max.X = gtx.Dp(210)
						return g.smallButton(&d.btn[i], a.label, bg)(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: 12}.Layout),
					layout.Flexed(1, small(g, a.about)),
				)
			})
		}))
	}
	if len(kids) == 0 {
		kids = append(kids, g.row(gtx, small(g, "Niente da fare qui per questo profilo.")))
	}
	return kids
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

// cleanOutput drops the colour codes and the control characters of a command's output.
func cleanOutput(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' && r != 0x7f {
			return r
		}
		return -1
	}, s)
}
