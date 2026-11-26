//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// chainaudit.go: the monitor's audit log. One JSON object per line, each
// carrying a sequence number and the SHA-256 of the previous line, so that
// deleting, reordering or editing records is detectable (`ptscry audit-verify`).
// Records are also sent to syslog (authpriv) for off-host retention, which is
// what makes the log resistant against someone who later gains root here.

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/syslog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

type chainAudit struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	sl       *syslog.Writer
	seq      uint64
	prev     string
	hostname string
	failed   atomic.Bool
	pending  []pendingRec // records not yet durably written (kept while the disk is failing)
	dropped  int          // records lost because the pending queue overflowed
	grouped  bool         // the group was applied (so that withdrawing it can be undone)
	group    string       // audit_group: may read the log (root:group 0640); "" = root only
}

// pendingRec is a record waiting to be written. Its sequence number and chain
// hash are assigned only when it is actually written, so a failed write never
// leaves a gap in the chain.
type pendingRec struct {
	event string
	f     map[string]any
	ts    string
}

const (
	maxRecordBytes = 16 * 1024 // one audit line never exceeds this
	maxPending     = 10000     // records held in memory while the disk is failing
)

func openChainAudit(path string, useSyslog bool, group ...string) (*chainAudit, error) {
	a := &chainAudit{path: path, prev: genesisHash}
	if len(group) > 0 {
		a.group = group[0]
	}
	a.hostname, _ = os.Hostname()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := a.openFile(); err != nil {
		return nil, err
	}
	if useSyslog {
		sl, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_INFO, progName)
		if err != nil {
			a.f.Close()
			return nil, fmt.Errorf("syslog: %w", err)
		}
		a.sl = sl
	}
	return a, nil
}

// openFile opens the log and, if it already has records, resumes the chain
// from the last one.
func (a *chainAudit) openFile() error {
	f, err := os.OpenFile(a.path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if line, err := lastLine(f); err == nil && len(line) > 0 {
		var r struct {
			Seq uint64 `json:"seq"`
		}
		if json.Unmarshal(line, &r) != nil || r.Seq == 0 {
			f.Close()
			return errors.New("existing audit log has an unreadable last record; refusing to append")
		}
		sum := sha256.Sum256(line)
		a.seq, a.prev = r.Seq, hex.EncodeToString(sum[:])
	}
	a.applyGroup(f)
	a.f = f
	return nil
}

// setGroup changes the reading group; it takes effect at the next (re)open.
func (a *chainAudit) setGroup(g string) {
	a.mu.Lock()
	a.group = g
	a.mu.Unlock()
}

// applyGroup gives the reading group access to the log file (and to the
// directory that holds it) through the open descriptor. Any failure leaves the
// log root-only, which is the safe side; it is reported on stderr (the audit
// lock is held, so no record can be written from here).
func (a *chainAudit) applyGroup(f *os.File) {
	if a.group == "" {
		if a.grouped { // the group was withdrawn by a reload: back to root only
			a.grouped = false
			_ = f.Chown(0, 0)
			_ = f.Chmod(0o600)
			if d := filepath.Dir(a.path); os.Chown(d, 0, 0) == nil {
				_ = os.Chmod(d, 0o750)
			}
		}
		return
	}
	fail := func(err error) {
		_ = f.Chown(0, 0)
		_ = f.Chmod(0o600)
		a.grouped = false
		fmt.Fprintf(os.Stderr, "ptscry: audit_group %q not applied, the log stays root-only: %v\n", a.group, err)
	}
	gid, err := lookupGID(a.group)
	if err != nil {
		fail(err)
		return
	}
	if err := f.Chown(0, gid); err != nil {
		fail(err)
		return
	}
	if err := f.Chmod(0o640); err != nil {
		fail(err)
		return
	}
	a.grouped = true
	dir := filepath.Dir(a.path)
	if err := os.Chown(dir, 0, gid); err == nil {
		_ = os.Chmod(dir, 0o750)
	}
}

// lastLine returns the last non-empty line of f (looking at its final 1 MiB; records are capped far below that).
func lastLine(f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil, err
	}
	n := int64(1024 * 1024)
	if st.Size() < n {
		n = st.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil && err != io.EOF {
		return nil, err
	}
	buf = bytes.TrimRight(buf, "\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return buf, nil
}

// log queues one record and writes everything pending. Core keys cannot be
// overridden by fields. If the disk is failing the record stays queued (bounded)
// and is written, in order, as soon as writing works again: nothing is lost
// silently and the chain never has a gap.
func (a *chainAudit) log(event string, f fields) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := make(map[string]any, len(f))
	for k, v := range f {
		rec[k] = v
	}
	a.pending = append(a.pending, pendingRec{event, rec, time.Now().UTC().Format(time.RFC3339Nano)})
	if len(a.pending) > maxPending {
		over := len(a.pending) - maxPending
		a.pending = append([]pendingRec(nil), a.pending[over:]...)
		a.dropped += over
	}
	a.flushLocked()
}

// flushLocked writes the pending records. Caller holds a.mu.
func (a *chainAudit) flushLocked() {
	for len(a.pending) > 0 {
		if a.dropped > 0 {
			if !a.writeOne("audit_events_lost", map[string]any{"count": a.dropped},
				time.Now().UTC().Format(time.RFC3339Nano)) {
				return
			}
			a.dropped = 0
		}
		p := a.pending[0]
		if !a.writeOne(p.event, p.f, p.ts) {
			return
		}
		a.pending = a.pending[1:]
	}
	a.pending = nil
	a.failed.Store(false)
}

// writeOne appends one record (sequence number, previous hash) and syncs it.
// On failure nothing changes: the file is cut back to its previous size.
func (a *chainAudit) writeOne(event string, f map[string]any, ts string) bool {
	fail := func(what string, err error) bool {
		a.failed.Store(true)
		fmt.Fprintf(os.Stderr, "ptscry: AUDIT %s FAILED: %v\n", what, err)
		return false
	}
	if a.f == nil {
		return fail("WRITE", errors.New("log file not open"))
	}
	rec := make(map[string]any, len(f)+5)
	for k, v := range f {
		rec[k] = v
	}
	rec["seq"], rec["prev"], rec["ts"], rec["event"], rec["host"] = a.seq+1, a.prev, ts, event, a.hostname
	b, err := json.Marshal(rec)
	if err == nil && len(b) > maxRecordBytes {
		b, err = json.Marshal(shrinkRecord(rec))
	}
	if err != nil {
		return fail("ENCODE", err)
	}
	var size int64 = -1
	if st, err := a.f.Stat(); err == nil {
		size = st.Size()
	}
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		if size >= 0 {
			_ = a.f.Truncate(size)
		}
		return fail("WRITE", err)
	}
	if err := a.f.Sync(); err != nil {
		if size >= 0 {
			_ = a.f.Truncate(size)
		}
		return fail("SYNC", err)
	}
	sum := sha256.Sum256(b)
	a.seq, a.prev = a.seq+1, hex.EncodeToString(sum[:])
	if a.sl != nil {
		_ = a.sl.Info(string(b))
	}
	return true
}

// shrinkRecord keeps a record under maxRecordBytes: long strings are cut, and
// if that is not enough the non-core fields are dropped.
func shrinkRecord(rec map[string]any) map[string]any {
	core := map[string]bool{"seq": true, "prev": true, "ts": true, "event": true, "host": true}
	out := make(map[string]any, len(rec)+1)
	for k, v := range rec {
		if s, ok := v.(string); ok && len(s) > 256 && !core[k] {
			v = s[:256]
		}
		out[k] = v
	}
	out["truncated"] = true
	if b, err := json.Marshal(out); err == nil && len(b) <= maxRecordBytes {
		return out
	}
	for k := range out {
		if !core[k] && k != "truncated" && k != "uid" && k != "user" && k != "req" && k != "session" {
			delete(out, k)
		}
	}
	return out
}

// healthy reports whether the last write succeeded. The monitor refuses new
// activity while it is false (fail closed).
func (a *chainAudit) healthy() bool { return !a.failed.Load() }

// probe tries to leave the failed state (the disk may have been cleaned up)
// and reports whether auditing works now. Without it a transient write error
// would keep the monitor refusing requests forever, since nothing else
// writes while it refuses.
func (a *chainAudit) probe() bool {
	if a.healthy() {
		return true
	}
	a.log("audit_probe", nil)
	return a.healthy()
}

// reopen switches to a fresh file at the same path (after logrotate moved the
// old one), carrying the chain across so the files verify as one sequence.
func (a *chainAudit) reopen() error {
	a.log("audit_reopen", nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		a.failed.Store(true)
		return err
	}
	a.applyGroup(f)
	if a.f != nil {
		a.f.Close()
	}
	a.f = f
	a.flushLocked() // anything queued while the old file was failing
	return nil
}

// suspend ends this process's use of the log without a closing record, for a
// monitor re-exec: the new image reopens the file and continues the chain.
// It refuses while records are waiting (the disk is failing).
func (a *chainAudit) suspend() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) > 0 || a.failed.Load() {
		return errors.New("the audit log is not healthy")
	}
	if a.f != nil {
		if err := a.f.Sync(); err != nil {
			return err
		}
		a.f.Close()
		a.f = nil
	}
	if a.sl != nil {
		a.sl.Close()
	}
	return nil
}

func (a *chainAudit) close() {
	a.log("audit_close", nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
	}
	if a.sl != nil {
		a.sl.Close()
	}
}

// verifyChain checks the given log files, in order, as one chain.
func verifyChain(paths []string) (records int, err error) {
	var lastHash string
	var lastSeq uint64
	first := true
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return records, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var r struct {
				Seq  uint64 `json:"seq"`
				Prev string `json:"prev"`
			}
			if err := json.Unmarshal(line, &r); err != nil || r.Seq == 0 || len(r.Prev) != 64 {
				f.Close()
				return records, fmt.Errorf("%s:%d: not a valid audit record", p, ln)
			}
			if !first {
				if r.Seq != lastSeq+1 {
					f.Close()
					return records, fmt.Errorf("%s:%d: sequence gap (expected %d, found %d): records missing", p, ln, lastSeq+1, r.Seq)
				}
				if !strings.EqualFold(r.Prev, lastHash) {
					f.Close()
					return records, fmt.Errorf("%s:%d: hash chain broken: a previous record was altered or removed", p, ln)
				}
			}
			first = false
			sum := sha256.Sum256(line)
			lastHash, lastSeq = hex.EncodeToString(sum[:]), r.Seq
			records++
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return records, err
		}
	}
	return records, nil
}
