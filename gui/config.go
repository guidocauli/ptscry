// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// config.go: settings and the saved workspace (gui.json in the same directory
// as client.conf). Nothing secret is stored: no passwords, no TOTP codes.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"ptscry/internal/rclient"
)

// profile is where a tab connects: the ptscry front-end of this computer
// ("local", 127.0.0.1) or of a server ("remote", host:port).
type profile struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`   // "local" or "remote"
	Target   string `json:"target"` // [user@]host[:port]; for local only the port matters
	Identity string `json:"identity,omitempty"`
	Cert     string `json:"cert,omitempty"`
	Pinned   string `json:"fingerprint,omitempty"` // SHA256:... instead of known_hosts
	// plain SSH ("ssh" mode) only
	Auth    string `json:"auth,omitempty"`    // "agent" (default), "key" (Identity) or "password"
	Jump    string `json:"jump,omitempty"`    // [user@]host[:port] to go through, as ssh -J
	Command string `json:"command,omitempty"` // run instead of the login shell
}

const (
	defaultLocalTarget = "127.0.0.1:" + rclient.DefaultPort
	localName          = "locale"
	standaloneName     = "senza server"
	shellName          = "terminale locale"
)

// tabState is what is saved of a tab: enough to reopen it and reattach to its
// session, which lives on the server.
type tabState struct {
	Profile string     `json:"profile"`
	Session string     `json:"session"`
	Cwd     string     `json:"cwd,omitempty"`
	Label   string     `json:"label,omitempty"`     // name given to the tab (the session keeps its own)
	RO      bool       `json:"read_only,omitempty"` // attached read-only
	Env     []envVar   `json:"env,omitempty"`       // variables of this session (empty = the standard ones)
	Shell   string     `json:"shell,omitempty"`     // "terminale locale": the shell of this tab (empty = the profile's)
	Panes   *paneState `json:"panes,omitempty"`     // the tab is split: its panes (this tab is the one marked "lead")
}

// envVar is an environment variable set in a session.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// workspace is a named set of tabs, saved to be reopened later.
type workspace struct {
	Name   string     `json:"name"`
	Tabs   []tabState `json:"tabs"`
	Active int        `json:"active,omitempty"`
}

type settings struct {
	FontSize    float32     `json:"font_size"`
	FontFace    string      `json:"font,omitempty"`
	FontFile    string      `json:"font_file,omitempty"`  // the file of FontFace, loaded directly
	Scrollback  int         `json:"scrollback"`           // lines; 0 = default; scrollUnlimited = "unlimited"
	Decorations string      `json:"decorations"`          // "auto", "on", "off"
	UIScale     int         `json:"ui_scale"`             // size of the interface (menus, tabs, dialogs) in % of what the screen DPI gives; 0 = 100
	MenuBar     string      `json:"menubar"`              // "on" (default) or "off": the drop-down menus along the top
	WinButtons  string      `json:"win_buttons"`          // min/max/close in the tab bar: "auto" (no system bar), "on", "off"
	StatusBar   string      `json:"status_bar,omitempty"` // the bar at the bottom: "on" (default) or "off"
	Cursor      string      `json:"cursor,omitempty"`     // "" = as the program asks (DECSCUSR), "blink", "steady"
	Default     string      `json:"default_profile"`
	Profiles    []profile   `json:"profiles"`
	Workspaces  []workspace `json:"workspaces,omitempty"`
	Startup     string      `json:"startup,omitempty"` // typed (line by line) into every session created anew
	Restore     bool        `json:"restore_workspace"`
	Tabs        []tabState  `json:"tabs,omitempty"`
	Active      int         `json:"active,omitempty"`
	WinW        int         `json:"win_w,omitempty"`
	WinH        int         `json:"win_h,omitempty"`
}

func defaultSettings() settings {
	def := localName
	if runtime.GOOS != "windows" {
		def = standaloneName // works on a fresh computer, with nothing to set up
	} else {
		def = shellName // no host on Windows: a local shell works with nothing to set up
	}
	return settings{
		FontSize: 14, Scrollback: scrollDefault, Decorations: "auto", MenuBar: "on", UIScale: 100, Default: def, Restore: true,
		Profiles: defaultProfiles(),
	}
}

var cfgMu sync.Mutex

func settingsPath() string {
	d, err := os.UserConfigDir()
	if err != nil || d == "" {
		return ""
	}
	return filepath.Join(d, "ptscry", "gui.json")
}

// loadSettings reads gui.json; a missing or broken file gives the defaults
// (a broken one is reported, not overwritten until the next save).
func loadSettings() (settings, error) {
	s := defaultSettings()
	p := settingsPath()
	if p == "" {
		return s, errors.New("no configuration directory: settings will not be saved")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if len(b) > 1<<20 {
		return s, errors.New(p + ": too big")
	}
	var f settings
	f.FontSize, f.Scrollback = 0, -1
	if err := json.Unmarshal(b, &f); err != nil {
		return s, fmt.Errorf("%s: %v", p, err)
	}
	return f.sanitized(), nil
}

// sanitized brings every value into a range that cannot hurt, whatever the
// file says.
func (s settings) sanitized() settings {
	d := defaultSettings()
	if s.FontSize < 6 || s.FontSize > 48 {
		s.FontSize = d.FontSize
	}
	if s.Scrollback < 0 {
		s.Scrollback = d.Scrollback
	}
	if s.Scrollback > scrollUnlimited {
		s.Scrollback = scrollUnlimited
	}
	switch s.Cursor {
	case "", "blink", "steady":
	default:
		s.Cursor = ""
	}
	switch s.Decorations {
	case "auto", "on", "off":
	default:
		s.Decorations = "auto"
	}
	switch s.MenuBar {
	case "on", "off":
	default:
		s.MenuBar = "on"
	}
	if s.UIScale == 0 {
		s.UIScale = 100
	}
	s.UIScale = min(max(s.UIScale, 60), 300)
	switch s.WinButtons {
	case "auto", "on", "off":
	default:
		s.WinButtons = "auto"
	}
	if !tilingPossible { // Windows and macOS: always their own title bar and buttons
		s.Decorations, s.WinButtons = "auto", "auto"
	}
	var ps []profile
	seen := map[string]bool{}
	hasLocal := false
	for _, p := range s.Profiles {
		if p.Name == "" || seen[p.Name] || (p.Mode != "local" && p.Mode != "remote" && p.Mode != "standalone" && p.Mode != "shell" && p.Mode != "ssh") {
			continue
		}
		if p.Mode == "standalone" && runtime.GOOS == "windows" {
			continue // no host on Windows: the profile could only fail
		}
		if p.Mode == "shell" && !validShellCmd(p.Target) {
			p.Target = ""
		}
		if p.Mode == "ssh" {
			if strings.ContainsAny(p.Command, "\x00\r\n") || len(p.Command) > 1024 {
				p.Command = ""
			}
			if p.Jump != "" {
				if _, err := parseSSHTarget(p.Jump); err != nil {
					p.Jump = ""
				}
			}
			switch p.Auth {
			case "agent", "key", "password":
			default:
				p.Auth = "agent"
			}
		}
		if p.Mode != "standalone" && p.Mode != "shell" { // its Target is a path, not an address
			parse := rclient.ParseTarget
			if p.Mode == "ssh" {
				parse = parseSSHTarget
			}
			if _, err := parse(orLocalUser(p)); err != nil {
				continue
			}
		}
		seen[p.Name] = true
		if p.Mode == "local" {
			hasLocal = true
		}
		ps = append(ps, p)
	}
	if !hasLocal {
		ps = append([]profile{d.Profiles[0]}, ps...)
	}
	if !seen[standaloneName] && runtime.GOOS != "windows" {
		ps = append(ps, profile{Name: standaloneName, Mode: "standalone"})
	}
	if !seen[shellName] {
		ps = append(ps, profile{Name: shellName, Mode: "shell"})
		if runtime.GOOS == "windows" && s.Default == localName {
			s.Default = shellName // saved before this profile existed: "locale" never worked there without a server
		}
	}
	s.Profiles = ps
	if s.profile(s.Default) == nil {
		s.Default = d.Default
	}
	tabs := s.cleanTabs(s.Tabs)
	s.Tabs = tabs
	var ws []workspace
	seenW := map[string]bool{}
	for _, w := range s.Workspaces {
		if w.Name == "" || len(w.Name) > 64 || seenW[w.Name] || len(ws) >= 100 {
			continue
		}
		seenW[w.Name] = true
		w.Tabs = s.cleanTabs(w.Tabs)
		if w.Active < 0 || w.Active >= len(w.Tabs) {
			w.Active = 0
		}
		ws = append(ws, w)
	}
	s.Workspaces = ws
	if s.Active < 0 || s.Active >= len(tabs) {
		s.Active = 0
	}
	if s.WinW < 300 || s.WinH < 200 || s.WinW > 10000 || s.WinH > 10000 {
		s.WinW, s.WinH = 0, 0
	}
	return s
}

func orLocalUser(p profile) string {
	if p.Mode == "local" {
		return localTarget(p)
	}
	return p.Target
}

// localTarget is the front-end of this computer: the port of the profile (or
// the default one) on 127.0.0.1, as the current user.
func localTarget(p profile) string {
	port := rclient.DefaultPort
	if p.Target != "" {
		if t, err := rclient.ParseTarget("x@" + hostPortOnly(p.Target)); err == nil {
			port = t.Port
		}
	}
	return "127.0.0.1:" + port
}

func hostPortOnly(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			return s[i+1:]
		}
	}
	return s
}

func (s *settings) profile(name string) *profile {
	for i := range s.Profiles {
		if s.Profiles[i].Name == name {
			return &s.Profiles[i]
		}
	}
	return nil
}

// save writes gui.json atomically, readable only by the user.
func (s settings) save() error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	p := settingsPath()
	if p == "" {
		return errors.New("no configuration directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".gui.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// cleanTabs keeps the saved tabs that still make sense: known profile, valid
// session name, sane label.
func (s settings) cleanTabs(in []tabState) []tabState {
	var tabs []tabState
	for _, t := range in {
		if s.profile(t.Profile) == nil || t.Session == "" || len(t.Session) > 64 || strings.HasPrefix(t.Session, "-") || len(tabs) >= 200 {
			continue
		}
		if _, _, _, err := rclient.Cmdline([]string{"new", "-A", "-n", t.Session}); err != nil {
			continue
		}
		t.Env = cleanEnv(t.Env)
		if !validShellCmd(t.Shell) {
			t.Shell = ""
		}
		if len(t.Label) > 80 || strings.ContainsAny(t.Label, "\x00\r\n") {
			t.Label = ""
		}
		tabs = append(tabs, t)
	}
	return tabs
}

// validEnvName tells whether s is a name a shell accepts for a variable.
func validEnvName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// cleanEnv keeps the usable variables: valid name, one-line value, no repeats
// (the last one wins), at most 100.
func cleanEnv(in []envVar) []envVar {
	var out []envVar
	pos := map[string]int{}
	for _, v := range in {
		if !validEnvName(v.Name) || len(v.Value) > 4096 || strings.ContainsAny(v.Value, "\x00\r\n") {
			continue
		}
		if i, ok := pos[v.Name]; ok {
			out[i] = v
			continue
		}
		if len(out) >= 100 {
			break
		}
		pos[v.Name] = len(out)
		out = append(out, v)
	}
	return out
}

// defaultProfiles are the ones every installation has; "senza server" needs the
// per-user host, which does not exist on Windows.
func defaultProfiles() []profile {
	ps := []profile{{Name: localName, Mode: "local", Target: defaultLocalTarget}}
	if runtime.GOOS != "windows" {
		ps = append(ps, profile{Name: standaloneName, Mode: "standalone"})
	}
	return append(ps, profile{Name: shellName, Mode: "shell"})
}

// parseSSHTarget reads [user@]host[:port] for a plain SSH server: port 22 by
// default (ptscry's front-end uses 2222).
func parseSSHTarget(s string) (rclient.Target, error) {
	s = strings.TrimSpace(s)
	host := s
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	if !strings.Contains(host, ":") || strings.HasPrefix(host, "[") && !strings.Contains(host, "]:") {
		s += ":22"
	}
	return rclient.ParseTarget(s)
}

// persistent tells whether the tabs of the profile are ptscry sessions (they
// survive the window); "shell" and "ssh" are plain terminals.
func (p profile) persistent() bool { return p.Mode != "shell" && p.Mode != "ssh" }
