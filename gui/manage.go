// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// manage.go: "Gestione ptscry": everything ptscry does, from the window: sessions
// and jobs, the host, the configuration and the checks, the administration of
// the monitor and the service. What a profile cannot do is greyed out.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"gioui.org/layout"
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

type mgAction struct {
	group   string
	label   string
	cap     int
	confirm bool
	args    func(d *manageDialog) ([]string, error)
	special string // "host-start", "service:<verb>"
}

func needName(d *manageDialog) (string, error) {
	n := strings.TrimSpace(d.name.Text())
	if !nameRe.MatchString(n) {
		return "", errors.New("scrivi il nome della sessione (lettere, cifre, _ . -)")
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

var mgActions = []mgAction{
	{group: "Sessioni e job", label: "Elenca", cap: capAll, args: fixedArgs("list")},
	{group: "Sessioni e job", label: "Stato", cap: capAll, args: simpleOp("status")},
	{group: "Sessioni e job", label: "Nuova, staccata", cap: capAll, args: func(d *manageDialog) ([]string, error) {
		n, err := needName(d)
		if err != nil {
			return nil, err
		}
		a := []string{"new", "-d", "-n", n}
		return append(a, strings.Fields(d.cmd.Text())...), nil
	}},
	{group: "Sessioni e job", label: "Esegui job", cap: capAll, args: func(d *manageDialog) ([]string, error) {
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
	{group: "Sessioni e job", label: "Risultato del job", cap: capAll, args: simpleOp("wait")},
	{group: "Sessioni e job", label: "Congela", cap: capAll, args: simpleOp("freeze")},
	{group: "Sessioni e job", label: "Scongela", cap: capAll, args: simpleOp("thaw")},
	{group: "Sessioni e job", label: "Termina / scarta", cap: capAll, confirm: true, args: simpleOp("kill")},

	{group: "Host", label: "Avvia l'host", cap: capStandby, special: "host-start"},
	{group: "Host", label: "Configurazione", cap: capLocal, args: fixedArgs("config")},
	{group: "Host", label: "Verifica (check)", cap: capLocal, args: fixedArgs("check")},
	{group: "Host", label: "Ferma l'host", cap: capAll, confirm: true, args: fixedArgs("stop")},

	{group: "Amministrazione (root o admin_group)", label: "Sessioni di tutti", cap: capAdmin, args: fixedArgs("admin", "list")},
	{group: "Amministrazione (root o admin_group)", label: "Termina sessione di utente", cap: capAdmin, confirm: true, args: func(d *manageDialog) ([]string, error) {
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
	{group: "Amministrazione (root o admin_group)", label: "Ferma host di utente", cap: capAdmin, confirm: true, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"admin", "stop", u}, nil
	}},
	{group: "Amministrazione (root o admin_group)", label: "Verifica audit", cap: capAdmin, args: fixedArgs("audit-verify")},
	{group: "Amministrazione (root o admin_group)", label: "Verifica di un utente", cap: capAdmin, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"check", "-u", u}, nil
	}},
	{group: "Amministrazione (root o admin_group)", label: "Registra il TOTP", cap: capAdmin, confirm: true, args: func(d *manageDialog) ([]string, error) {
		u, err := needUser(d)
		if err != nil {
			return nil, err
		}
		return []string{"totp-init", u}, nil
	}},
	{group: "Amministrazione (root o admin_group)", label: "Sostituisci il monitor (reexec)", cap: capAdmin, confirm: true, args: fixedArgs("admin", "reexec")},

	{group: "Servizio del monitor (systemd)", label: "Stato", cap: capService, special: "service:status"},
	{group: "Servizio del monitor (systemd)", label: "Avvia", cap: capService, special: "service:start"},
	{group: "Servizio del monitor (systemd)", label: "Ferma", cap: capService, confirm: true, special: "service:stop"},
	{group: "Servizio del monitor (systemd)", label: "Riavvia", cap: capService, confirm: true, special: "service:restart"},
	{group: "Servizio del monitor (systemd)", label: "Rileggi la config", cap: capService, special: "service:reload"},
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

// ---- the dialog ----

type manageDialog struct {
	g       *gui
	profile widget.Enum
	names   []string
	modes   map[string]string
	name    widget.Editor
	cmd     widget.Editor
	user    widget.Editor
	out     widget.Editor
	btn     []widget.Clickable
	close   widget.Clickable
	clear   widget.Clickable
	confirm int

	mu      sync.Mutex
	text    string
	changed bool
	busy    bool
}

func (g *gui) openManageDialog() {
	d := &manageDialog{g: g, confirm: -1, modes: map[string]string{}, btn: make([]widget.Clickable, len(mgActions))}
	d.name.SingleLine, d.cmd.SingleLine, d.user.SingleLine = true, true, true
	d.out.ReadOnly = true
	g.setMu.Lock()
	for _, p := range g.set.Profiles {
		d.names = append(d.names, p.Name)
		d.modes[p.Name] = p.Mode
	}
	cur := g.set.Default
	g.setMu.Unlock()
	if g.active >= 0 && g.active < len(g.tabs) {
		cur = g.curTab().profile
		d.name.SetText(g.curTab().session)
	}
	d.profile.Value = cur
	g.setDialog(d)
}

func (d *manageDialog) Cancel() {}

func (d *manageDialog) show(s string) {
	d.mu.Lock()
	d.text, d.changed = s, true
	d.mu.Unlock()
	d.g.invalidate()
}

// run executes the action in the background and shows what it printed.
func (d *manageDialog) run(a mgAction) {
	prof := d.profile.Value
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
	d.show("in corso...")
	go func() {
		defer func() {
			d.mu.Lock()
			d.busy = false
			d.mu.Unlock()
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
	d.mu.Lock()
	if d.changed {
		d.out.SetText(d.text)
		d.changed = false
	}
	busy := d.busy
	d.mu.Unlock()
	mode := d.modes[d.profile.Value]

	kids := []layout.FlexChild{g.row(gtx, g.label("Gestione di ptscry"))}
	for _, n := range d.names {
		n := n
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.RadioButton(g.th, &d.profile, n, n).Layout(gtx)
		}))
	}
	kids = append(kids,
		layout.Rigid(layout.Spacer{Height: 6}.Layout),
		g.row(gtx, g.label("Sessione / job")), g.row(gtx, g.field(&d.name, "nome della sessione")),
		g.row(gtx, g.label("Comando (per Nuova e Esegui job; separato da spazi, senza virgolette)")), g.row(gtx, g.field(&d.cmd, "es. make -j8 all")),
	)
	if mode == "local" {
		kids = append(kids, g.row(gtx, g.label("Utente (per l'amministrazione)")), g.row(gtx, g.field(&d.user, "nome dell'utente")))
	}
	// the buttons, by group, four to a row
	group := ""
	var row []layout.FlexChild
	flush := func() {
		if len(row) > 0 {
			r := row
			kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, r...)
			}))
			row = nil
		}
	}
	for i := range mgActions {
		i, a := i, mgActions[i]
		if !a.avail(mode, false) {
			continue
		}
		if a.group != group {
			flush()
			group = a.group
			kids = append(kids, layout.Rigid(layout.Spacer{Height: 6}.Layout), g.row(gtx, g.label(a.group)))
		}
		if d.btn[i].Clicked(gtx) && !busy {
			if a.confirm && d.confirm != i {
				d.confirm = i
			} else {
				d.confirm = -1
				d.run(a)
			}
		}
		label, bg := a.label, theme.bg
		if d.confirm == i {
			label, bg = "Sicuro?", theme.warn
		}
		if len(row) >= 7 { // a label and a gap each
			flush()
		}
		row = append(row, layout.Rigid(g.smallButton(&d.btn[i], label, bg)), layout.Rigid(layout.Spacer{Width: 6}.Layout))
	}
	flush()
	kids = append(kids,
		layout.Rigid(layout.Spacer{Height: 8}.Layout),
		g.row(gtx, g.label("Risultato")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.Y = gtx.Dp(180)
			gtx.Constraints.Max.Y = gtx.Dp(180)
			return g.fieldFont(&d.out, "", true)(gtx)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.clear, "Pulisci", theme.bg)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.bg)),
			)
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
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
