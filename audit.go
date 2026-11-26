//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// audit.go: JSON-lines audit log (file and/or syslog).
//
// The host fails closed: if auditing is configured and cannot be opened, the
// host refuses to start (set audit_log = off to disable it explicitly).

package main

import (
	"encoding/json"
	"fmt"
	"log/syslog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fields map[string]any

// auditor receives audit events. In standalone mode it is the local *audit;
// in managed mode events are relayed to the monitor, which writes them.
type auditor interface {
	log(event string, f fields)
	close()
}

type audit struct {
	mu       sync.Mutex
	f        *os.File
	sl       *syslog.Writer
	user     string
	uid      int
	hostname string
}

func openAudit(cfg *config, username string, uid int, home string) (*audit, error) {
	a := &audit{user: username, uid: uid}
	a.hostname, _ = os.Hostname()

	path := cfg.AuditLog
	if path == "" {
		path = defaultAuditPath(uid, home)
	}
	if path != "off" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
		if err != nil {
			return nil, err
		}
		a.f = f
	}
	if cfg.AuditSyslog {
		sl, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_INFO, progName)
		if err != nil {
			if a.f != nil {
				a.f.Close()
			}
			return nil, fmt.Errorf("syslog: %w", err)
		}
		a.sl = sl
	}
	return a, nil
}

func (a *audit) log(event string, f fields) {
	if a == nil || (a.f == nil && a.sl == nil) {
		return
	}
	rec := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"event": event,
		"host":  a.hostname,
		"user":  a.user,
		"uid":   a.uid,
	}
	for k, v := range f {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		_, _ = a.f.Write(append(b, '\n'))
	}
	if a.sl != nil {
		_ = a.sl.Info(string(b))
	}
}

func (a *audit) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
	}
	if a.sl != nil {
		a.sl.Close()
	}
}
