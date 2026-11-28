//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runSetupForAdmin: what an administrator may ask the root monitor.
func TestRunSetupForAdmin(t *testing.T) {
	d := t.TempDir()
	conf := filepath.Join(d, "ptscry.conf")
	if err := os.WriteFile(conf, []byte("[global]\naccess_group = nosuchgroup_t\nfrontend_authorized_keys_dir = "+filepath.Join(d, "k")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// no path is read by root for the caller: only text (-) or -home
	adm := setupCaller{name: "someadmin"}
	if _, err := runSetupForAdmin(conf, []string{"keys", "add", "nobody", "/etc/shadow"}, "", setupCaller{root: true}); err == nil || !strings.Contains(err.Error(), "read by the client") {
		t.Errorf("a file path through the monitor: %v", err)
	}
	// system accounts: root only
	for _, args := range [][]string{{"users", "add", "-create", "x1"}, {"users", "add", "-shell", "/bin/sh", "x1"}} {
		if _, err := runSetupForAdmin(conf, args, "", adm); err == nil || !strings.Contains(err.Error(), "needs root") && !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%v by an administrator: %v", args, err)
		}
	}
	if _, err := runSetupForAdmin(conf, []string{"frontend", "x"}, "", setupCaller{root: true}); err == nil {
		t.Error("only users and keys")
	}
	if _, err := runSetupForAdmin(conf, []string{"users"}, "", setupCaller{root: true}); err == nil {
		t.Error("too few arguments")
	}
	// system accounts and root: not by an administrator
	for _, args := range [][]string{{"users", "add", "daemon"}, {"keys", "add", "daemon", "-"}, {"users", "add", "root"}, {"keys", "add", "root", "-"}} {
		if _, err := runSetupForAdmin(conf, args, "", adm); err == nil {
			t.Errorf("%v by an administrator was accepted", args)
		}
	}
	// the membership of admin_group: root or admin_root_groups only
	for _, args := range [][]string{{"users", "add", "-admin", "nobody"}, {"users", "add", "--admin=true", "nobody"}, {"users", "remove", "-admin", "nobody"}} {
		if _, err := runSetupForAdmin(conf, args, "", adm); err == nil || !strings.Contains(err.Error(), "admin_root_groups") {
			t.Errorf("%v by an administrator: %v", args, err)
		}
	}
	// admin_setup = users: no keys
	if err := os.WriteFile(conf, []byte("[global]\nadmin_setup = users\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupForAdmin(conf, []string{"keys", "list"}, "", adm); err == nil || !strings.Contains(err.Error(), "admin_setup") {
		t.Errorf("keys with admin_setup = users: %v", err)
	}
	if err := os.WriteFile(conf, []byte("[global]\nadmin_setup = maybe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupForAdmin(conf, []string{"users", "list"}, "", adm); err == nil {
		t.Error("an invalid admin_setup was accepted")
	}
}

func TestAdminRootGroupsConfig(t *testing.T) {
	d := t.TempDir()
	conf := filepath.Join(d, "ptscry.conf")
	if err := os.WriteFile(conf, []byte("[global]\nadmin_root_groups = ldap-sysadmins, ipa-admins root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := setupConfigAt(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.AdminRootGroups, " ") != "ldap-sysadmins ipa-admins" {
		t.Errorf("admin_root_groups = %q", g.AdminRootGroups)
	}
	if err := os.WriteFile(conf, []byte("[global]\n[user:x]\nadmin_root_groups = g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setupConfigAt(conf); err == nil {
		t.Error("admin_root_groups accepted outside [global]")
	}
}
