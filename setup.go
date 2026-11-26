//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// setup.go: "ptscry setup users" and "ptscry setup keys": who may use ptscry
// (members of access_group and admin_group, creating local accounts if asked)
// and the public keys of the SSH front-end (frontend_authorized_keys_dir).
// Every operation is a direct command; without arguments a text wizard asks.
// Root only for changes. Local groups are changed with gpasswd; a group that
// comes from FreeIPA/SSSD cannot be changed from here and the command says how.

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

const setupUsage = `ptscry setup                         text wizard (users, keys, front-end)
ptscry setup frontend                turn the SSH front-end on: account, host key, key directory,
                                     "frontend = yes" in the configuration, reload of the monitor
ptscry setup users                   wizard for the users
ptscry setup users list              members of access_group and admin_group
ptscry setup users add [-admin] [-create] [-shell PATH] USER...
                                     allow USER to use ptscry (-admin: also admin_group;
                                     -create: create the local account if missing)
ptscry setup users remove [-admin] USER...
                                     withdraw USER from both groups (-admin: only from admin_group)
ptscry setup keys                    wizard for the front-end keys
ptscry setup keys list [USER]        public keys of the SSH front-end
ptscry setup keys add USER FILE|-|-home
                                     import public keys (a file, - = standard input,
                                     -home = the user's ~/.ssh/authorized_keys)
ptscry setup keys remove USER N|SHA256:...
                                     remove a key (number from "list", or its fingerprint)`

func cmdSetup(args []string) error {
	if len(args) == 0 {
		return setupWizard("")
	}
	switch args[0] {
	case "users", "user":
		if len(args) == 1 {
			return setupWizard("users")
		}
		return setupUsers(args[1:])
	case "keys", "key":
		if len(args) == 1 {
			return setupWizard("keys")
		}
		return setupKeys(args[1:])
	case "frontend":
		return setupFrontend(os.Stdout)
	case "-h", "help", "--help":
		fmt.Println(setupUsage)
		return nil
	}
	return fmt.Errorf("unknown setup command %q\n%s", args[0], setupUsage)
}

// setupConfig is the global configuration (groups, key directory).
func setupConfig() (config, error) {
	cf, err := parseConfigFile(configPath())
	if err != nil {
		return config{}, err
	}
	return cf.resolve("", nil), nil
}

func needRoot(what string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("only root can %s (sudo ptscry setup ...)", what)
	}
	return nil
}

// ------------------------------------------------------------------ users

func setupUsers(args []string) error {
	g, err := setupConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		return usersList(os.Stdout, g)
	case "add":
		fs := flag.NewFlagSet("setup users add", flag.ContinueOnError)
		admin := fs.Bool("admin", false, "also admin_group")
		create := fs.Bool("create", false, "create the local account if missing")
		shell := fs.String("shell", "", "login shell of a created account (default /bin/bash)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("usage: ptscry setup users add [-admin] [-create] [-shell PATH] USER...")
		}
		for _, u := range fs.Args() {
			if err := userAdd(os.Stdout, g, u, *admin, *create, *shell); err != nil {
				return err
			}
		}
		return nil
	case "remove", "rm", "del":
		fs := flag.NewFlagSet("setup users remove", flag.ContinueOnError)
		adminOnly := fs.Bool("admin", false, "only from admin_group")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("usage: ptscry setup users remove [-admin] USER...")
		}
		for _, u := range fs.Args() {
			if err := userRemove(os.Stdout, g, u, *adminOnly); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], setupUsage)
}

// groupMembers is the member list of a group as NSS sees it, and whether the
// group is local (/etc/group, so gpasswd can change it).
func groupMembers(name string) (members []string, local bool, err error) {
	if name == "" {
		return nil, false, nil
	}
	line, err := lookupGroup(name)
	if err != nil {
		return nil, false, err
	}
	f := strings.Split(strings.TrimSpace(strings.Split(line, "\n")[0]), ":")
	if len(f) >= 4 && f[3] != "" {
		members = strings.Split(f[3], ",")
	}
	if raw, err := os.ReadFile("/etc/group"); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(l, name+":") {
				local = true
			}
		}
	}
	sort.Strings(members)
	return members, local, nil
}

func usersList(w io.Writer, g config) error {
	if g.AccessGroup == "" {
		fmt.Fprintln(w, "access_group is empty: every user may use ptscry")
	}
	acc, accLocal, err := groupMembers(g.AccessGroup)
	if err != nil && g.AccessGroup != "" {
		return fmt.Errorf("access_group %q: %v (create it: groupadd -r %s)", g.AccessGroup, err, g.AccessGroup)
	}
	adm, _, _ := groupMembers(g.AdminGroup)
	isAdm := map[string]bool{}
	for _, a := range adm {
		isAdm[a] = true
	}
	all := map[string]bool{}
	for _, u := range append(append([]string{}, acc...), adm...) {
		all[u] = true
	}
	names := make([]string, 0, len(all))
	for u := range all {
		names = append(names, u)
	}
	sort.Strings(names)
	src := "local"
	if !accLocal {
		src = "directory (FreeIPA/SSSD)"
	}
	if g.AccessGroup != "" {
		fmt.Fprintf(w, "access_group %s (%s), admin_group %s; root allowed: %v\n\n", g.AccessGroup, src, g.AdminGroup, g.AllowRoot)
	}
	fmt.Fprintf(w, "%-20s %-7s %-6s %-5s %s\n", "USER", "UID", "ADMIN", "KEYS", "NOTE")
	for _, u := range names {
		uid, note := "-", ""
		if id, err := lookupUserName(u); err == nil {
			uid = id
		} else {
			note = "account not found"
		}
		if !contains(acc, u) {
			note = strings.TrimSpace(note + " not in access_group (admin only)")
		}
		keys := "-"
		if g.FrontendKeysDir != "none" {
			if lines, err := readKeyFile(g, u); err == nil && len(lines) > 0 {
				keys = strconv.Itoa(len(lines))
			}
		}
		fmt.Fprintf(w, "%-20s %-7s %-6s %-5s %s\n", u, uid, yesno(isAdm[u]), keys, note)
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "(nobody yet: ptscry setup users add USER)")
	}
	return nil
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func userAdd(w io.Writer, g config, name string, admin, create bool, shell string) error {
	if err := needRoot("change the users"); err != nil {
		return err
	}
	if !feUserRe.MatchString(name) || name == "root" {
		return fmt.Errorf("%q: not a valid user name (root needs no group: allow_root)", name)
	}
	if _, err := lookupUserName(name); err != nil {
		if !create {
			return fmt.Errorf("user %s does not exist (add -create to make a local account, or create it in FreeIPA)", name)
		}
		if shell == "" {
			shell = "/bin/bash"
		}
		if _, err := os.Stat(shell); err != nil {
			return fmt.Errorf("shell %s: %v", shell, err)
		}
		if out, err := exec.Command("useradd", "-m", "-s", shell, name).CombinedOutput(); err != nil {
			return fmt.Errorf("useradd %s: %v %s", name, err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "account %s: created (set a password with: passwd %s, or give it a key: ptscry setup keys add %s ...)\n", name, name, name)
	}
	groups := []string{g.AccessGroup}
	if admin {
		groups = append(groups, g.AdminGroup)
	}
	for _, grp := range groups {
		if grp == "" {
			continue
		}
		if err := groupChange(w, grp, name, true); err != nil {
			return err
		}
	}
	return nil
}

func userRemove(w io.Writer, g config, name string, adminOnly bool) error {
	if err := needRoot("change the users"); err != nil {
		return err
	}
	groups := []string{g.AdminGroup}
	if !adminOnly {
		groups = append(groups, g.AccessGroup)
	}
	for _, grp := range groups {
		if grp == "" {
			continue
		}
		if err := groupChange(w, grp, name, false); err != nil {
			return err
		}
	}
	if !adminOnly {
		fmt.Fprintf(w, "%s can no longer use ptscry; the monitor closes their sessions at the next check (the account itself is kept)\n", name)
	}
	return nil
}

// groupChange adds (or removes) a user to a local group with gpasswd.
func groupChange(w io.Writer, grp, name string, add bool) error {
	members, local, err := groupMembers(grp)
	if err != nil {
		return fmt.Errorf("group %s: %v (create it: groupadd -r %s)", grp, err, grp)
	}
	in := contains(members, name)
	if add == in {
		if add {
			fmt.Fprintf(w, "%s: already a member of %s\n", name, grp)
		} else {
			fmt.Fprintf(w, "%s: not a member of %s, nothing to do\n", name, grp)
		}
		return nil
	}
	if !local {
		verb := map[bool]string{true: "group-add-member", false: "group-remove-member"}[add]
		return fmt.Errorf("group %s is not a local group (FreeIPA/SSSD): change it there, for example: ipa %s %s --users=%s (then sss_cache -E)", grp, verb, grp, name)
	}
	flag := map[bool]string{true: "-a", false: "-d"}[add]
	if out, err := exec.Command("gpasswd", flag, name, grp).CombinedOutput(); err != nil {
		return fmt.Errorf("gpasswd %s %s %s: %v %s", flag, name, grp, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "%s: %s %s\n", name, map[bool]string{true: "added to", false: "removed from"}[add], grp)
	return nil
}

// ------------------------------------------------------------------ keys

func setupKeys(args []string) error {
	g, err := setupConfig()
	if err != nil {
		return err
	}
	if g.FrontendKeysDir == "none" {
		return errors.New("frontend_authorized_keys_dir is none: the front-end takes no plain keys (certificates only)")
	}
	switch args[0] {
	case "list", "ls":
		if len(args) > 2 {
			return errors.New("usage: ptscry setup keys list [USER]")
		}
		if len(args) == 2 {
			return keysList(os.Stdout, g, args[1])
		}
		return keysListAll(os.Stdout, g)
	case "add", "import":
		if len(args) != 3 {
			return errors.New("usage: ptscry setup keys add USER FILE|-|-home")
		}
		var src io.Reader
		switch args[2] {
		case "-":
			src = os.Stdin
		case "-home":
			r, err := homeAuthorizedKeys(args[1])
			if err != nil {
				return err
			}
			src = r
		default:
			f, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer f.Close()
			src = f
		}
		n, err := keysAdd(os.Stdout, g, args[1], src)
		if err == nil && n == 0 {
			fmt.Println("nothing new to add")
		}
		return err
	case "remove", "rm", "del":
		if len(args) != 3 {
			return errors.New("usage: ptscry setup keys remove USER N|SHA256:...")
		}
		return keysRemove(os.Stdout, g, args[1], args[2])
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], setupUsage)
}

func keyFile(g config, name string) (string, error) {
	if !feUserRe.MatchString(name) {
		return "", fmt.Errorf("%q: not a valid user name", name)
	}
	return filepath.Join(g.FrontendKeysDir, name), nil
}

// readKeyFile returns the lines of the user's key file (missing = none).
func readKeyFile(g config, name string) ([]string, error) {
	p, err := keyFile(g, name)
	if err != nil {
		return nil, err
	}
	raw, err := readRootFile(p, feKeysMaxBytes, false)
	if err != nil {
		if os.IsNotExist(err) || os.IsNotExist(errors.Unwrap(err)) {
			return nil, nil
		}
		if _, serr := os.Stat(p); os.IsNotExist(serr) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

func fingerprint(k ssh.PublicKey) string {
	h := sha256.Sum256(k.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// describeKey: type, fingerprint, comment, options of an authorized_keys line.
func describeKey(line string) (fp, desc string, err error) {
	k, comment, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", "", err
	}
	fp = fingerprint(k)
	desc = k.Type() + "  " + fp
	if comment != "" {
		desc += "  " + comment
	}
	if len(opts) > 0 {
		desc += "  [" + strings.Join(opts, ",") + "]"
	}
	if _, err := parseAuthLine(line); err != nil {
		desc += "  (NOT USABLE: " + err.Error() + ")"
	}
	return fp, desc, nil
}

func keysList(w io.Writer, g config, name string) error {
	lines, err := readKeyFile(g, name)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintf(w, "%s: no keys (ptscry setup keys add %s FILE)\n", name, name)
		return nil
	}
	fmt.Fprintf(w, "%s:\n", name)
	for i, l := range lines {
		_, d, err := describeKey(l)
		if err != nil {
			d = "unreadable line: " + err.Error()
		}
		fmt.Fprintf(w, "  %2d  %s\n", i+1, d)
	}
	return nil
}

func keysListAll(w io.Writer, g config) error {
	ents, err := os.ReadDir(g.FrontendKeysDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "no keys yet (ptscry setup keys add USER FILE)")
			return nil
		}
		return err
	}
	n := 0
	for _, e := range ents {
		if e.Type().IsRegular() && feUserRe.MatchString(e.Name()) {
			if err := keysList(w, g, e.Name()); err != nil {
				fmt.Fprintf(w, "%s: %v\n", e.Name(), err)
			}
			n++
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "no keys yet (ptscry setup keys add USER FILE)")
	}
	return nil
}

// homeAuthorizedKeys opens the user's own ~/.ssh/authorized_keys.
func homeAuthorizedKeys(name string) (io.Reader, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(u.HomeDir, ".ssh", "authorized_keys")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(string(raw)), nil
}

// keysAdd imports the valid public keys of src for the user; it returns how
// many were new. Lines the front-end could not honour are refused.
func keysAdd(w io.Writer, g config, name string, src io.Reader) (int, error) {
	if err := needRoot("change the keys"); err != nil {
		return 0, err
	}
	if _, err := lookupUserName(name); err != nil {
		return 0, fmt.Errorf("user %s does not exist", name)
	}
	p, err := keyFile(g, name)
	if err != nil {
		return 0, err
	}
	old, err := readKeyFile(g, name)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, l := range old {
		if fp, _, err := describeKey(l); err == nil {
			have[fp] = true
		}
	}
	raw, err := io.ReadAll(io.LimitReader(src, feKeysMaxBytes))
	if err != nil {
		return 0, err
	}
	var add []string
	for i, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Contains(l, "PRIVATE KEY") {
			return 0, errors.New("this is a private key: give the public one (.pub)")
		}
		fp, d, err := describeKey(l)
		if err != nil {
			return 0, fmt.Errorf("line %d: not a public key: %v", i+1, err)
		}
		if _, err := parseAuthLine(l); err != nil {
			return 0, fmt.Errorf("line %d: %v (the front-end would ignore it)", i+1, err)
		}
		if have[fp] {
			fmt.Fprintf(w, "already present: %s\n", d)
			continue
		}
		have[fp] = true
		add = append(add, l)
		fmt.Fprintf(w, "added: %s\n", d)
	}
	if len(add) == 0 {
		return 0, nil
	}
	if len(old)+len(add) > feKeysMaxLines {
		return 0, fmt.Errorf("too many keys for %s (at most %d)", name, feKeysMaxLines)
	}
	return len(add), writeKeyFile(g, p, append(old, add...))
}

func keysRemove(w io.Writer, g config, name, which string) error {
	if err := needRoot("change the keys"); err != nil {
		return err
	}
	p, err := keyFile(g, name)
	if err != nil {
		return err
	}
	lines, err := readKeyFile(g, name)
	if err != nil {
		return err
	}
	idx := -1
	if n, err := strconv.Atoi(which); err == nil {
		idx = n - 1
	} else {
		for i, l := range lines {
			if fp, _, err := describeKey(l); err == nil && fp == which {
				idx = i
			}
		}
	}
	if idx < 0 || idx >= len(lines) {
		return fmt.Errorf("%s: no key %q (see: ptscry setup keys list %s)", name, which, name)
	}
	_, d, _ := describeKey(lines[idx])
	lines = append(lines[:idx], lines[idx+1:]...)
	if err := writeKeyFile(g, p, lines); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed: %s\n", d)
	return nil
}

// writeKeyFile replaces the file atomically: root:root 0644 in a root-only directory.
func writeKeyFile(g config, p string, lines []string) error {
	if err := ensureRootDir(g.FrontendKeysDir, 0o755); err != nil {
		return err
	}
	if len(lines) == 0 {
		err := os.Remove(p)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tmp, err := os.CreateTemp(g.FrontendKeysDir, ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	body := "# ptscry front-end keys of " + filepath.Base(p) + " (ptscry setup keys)\n" + strings.Join(lines, "\n") + "\n"
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ------------------------------------------------------------------ wizard

type wizard struct {
	in  *bufio.Reader
	out io.Writer
	g   config
}

func (wz *wizard) ask(q string) (string, bool) {
	fmt.Fprint(wz.out, q)
	l, err := wz.in.ReadString('\n')
	if err != nil && l == "" {
		fmt.Fprintln(wz.out)
		return "", false
	}
	return strings.TrimSpace(l), true
}

func (wz *wizard) yes(q string) bool {
	a, _ := wz.ask(q + " [s/N] ")
	a = strings.ToLower(a)
	return a == "s" || a == "si" || a == "sì" || a == "y" || a == "yes"
}

func (wz *wizard) menu(title string, items []string) (int, bool) {
	fmt.Fprintf(wz.out, "\n== %s\n", title)
	for i, it := range items {
		fmt.Fprintf(wz.out, "  %d) %s\n", i+1, it)
	}
	fmt.Fprintln(wz.out, "  0) indietro / esci")
	for {
		a, ok := wz.ask("> ")
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(a)
		if err == nil && n >= 0 && n <= len(items) {
			return n, true
		}
		fmt.Fprintln(wz.out, "scegli un numero dell'elenco")
	}
}

func (wz *wizard) report(err error) {
	if err != nil {
		fmt.Fprintf(wz.out, "errore: %v\n", err)
	}
}

func setupWizard(start string) error {
	if !isTerminal(os.Stdin) {
		return errors.New("the wizard needs a terminal; use the direct commands:\n" + setupUsage)
	}
	if err := needRoot("run the setup wizard"); err != nil {
		return err
	}
	g, err := setupConfig()
	if err != nil {
		return err
	}
	wz := &wizard{in: bufio.NewReader(os.Stdin), out: os.Stdout, g: g}
	fmt.Printf("ptscry setup - %s\nconfigurazione: %s\n", progCredits, configPath())
	switch start {
	case "users":
		wz.users()
		return nil
	case "keys":
		wz.keys()
		return nil
	}
	for {
		n, ok := wz.menu("ptscry setup", []string{"Utenti (chi puo' usare ptscry)", "Chiavi del front-end SSH", "Attiva il front-end SSH (connessioni dalla GUI e da altri computer)"})
		if !ok || n == 0 {
			return nil
		}
		switch n {
		case 1:
			wz.users()
		case 2:
			wz.keys()
		case 3:
			if wz.yes("attivare il front-end SSH (porta " + wz.g.FrontendListen + ")?") {
				wz.report(setupFrontend(wz.out))
				if g, err := setupConfig(); err == nil {
					wz.g = g
				}
			}
		}
	}
}

func (wz *wizard) users() {
	for {
		n, ok := wz.menu("Utenti", []string{"Elenco", "Aggiungi un utente", "Togli un utente", "Rendi amministratore", "Togli i diritti di amministratore"})
		if !ok || n == 0 {
			return
		}
		switch n {
		case 1:
			wz.report(usersList(wz.out, wz.g))
		case 2:
			name, _ := wz.ask("utente: ")
			if name == "" {
				continue
			}
			create := false
			if _, err := lookupUserName(name); err != nil {
				if !wz.yes("l'utente " + name + " non esiste: crearlo come account locale?") {
					continue
				}
				create = true
			}
			admin := wz.yes("anche amministratore di ptscry (" + wz.g.AdminGroup + ")?")
			wz.report(userAdd(wz.out, wz.g, name, admin, create, ""))
			if create && wz.yes("impostare adesso la password di "+name+"?") {
				cmd := exec.Command("passwd", name)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
				wz.report(cmd.Run())
			}
			if wz.yes("importare una chiave pubblica per il front-end SSH?") {
				wz.addKey(name)
			}
		case 3:
			name, _ := wz.ask("utente da togliere: ")
			if name != "" && wz.yes("togliere "+name+" da ptscry (l'account resta)?") {
				wz.report(userRemove(wz.out, wz.g, name, false))
			}
		case 4:
			name, _ := wz.ask("utente: ")
			if name != "" {
				wz.report(groupChange(wz.out, wz.g.AdminGroup, name, true))
			}
		case 5:
			name, _ := wz.ask("utente: ")
			if name != "" {
				wz.report(userRemove(wz.out, wz.g, name, true))
			}
		}
	}
}

func (wz *wizard) keys() {
	if wz.g.FrontendKeysDir == "none" {
		fmt.Fprintln(wz.out, "frontend_authorized_keys_dir = none: il front-end accetta solo certificati")
		return
	}
	for {
		n, ok := wz.menu("Chiavi del front-end SSH ("+wz.g.FrontendKeysDir+")", []string{"Elenco di tutte le chiavi", "Chiavi di un utente", "Aggiungi una chiave", "Importa ~/.ssh/authorized_keys di un utente", "Togli una chiave"})
		if !ok || n == 0 {
			return
		}
		switch n {
		case 1:
			wz.report(keysListAll(wz.out, wz.g))
		case 2:
			if name, _ := wz.ask("utente: "); name != "" {
				wz.report(keysList(wz.out, wz.g, name))
			}
		case 3:
			if name, _ := wz.ask("utente: "); name != "" {
				wz.addKey(name)
			}
		case 4:
			if name, _ := wz.ask("utente: "); name != "" {
				r, err := homeAuthorizedKeys(name)
				if err != nil {
					wz.report(err)
					continue
				}
				_, err = keysAdd(wz.out, wz.g, name, r)
				wz.report(err)
			}
		case 5:
			name, _ := wz.ask("utente: ")
			if name == "" {
				continue
			}
			if err := keysList(wz.out, wz.g, name); err != nil {
				wz.report(err)
				continue
			}
			if which, _ := wz.ask("numero (o impronta SHA256:...) della chiave da togliere: "); which != "" {
				wz.report(keysRemove(wz.out, wz.g, name, which))
			}
		}
	}
}

// addKey asks for a public key: a path to a .pub file or the key line itself.
func (wz *wizard) addKey(name string) {
	a, _ := wz.ask("file .pub, oppure incolla la riga della chiave (ssh-ed25519 AAAA...): ")
	if a == "" {
		return
	}
	var src io.Reader = strings.NewReader(a)
	if !strings.HasPrefix(a, "ssh-") && !strings.HasPrefix(a, "ecdsa-") && !strings.HasPrefix(a, "sk-") {
		if strings.HasPrefix(a, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				a = filepath.Join(h, a[2:])
			}
		}
		f, err := os.Open(a)
		if err != nil {
			wz.report(err)
			return
		}
		defer f.Close()
		src = f
	}
	n, err := keysAdd(wz.out, wz.g, name, src)
	wz.report(err)
	if err == nil && n == 0 {
		fmt.Fprintln(wz.out, "nessuna chiave nuova")
	}
}
