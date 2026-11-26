//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// host_jobs.go: results of detached jobs (`ptscry run -d`).
//
// A job started with run -d leaves its exit status and the tail of its output
// (replay_bytes) in the host for job_retention, so that a controller can
// submit, disconnect and come back later with `ptscry wait NAME` or
// `ptscry status NAME`. Results live in the host's memory only: they are
// bounded in number and in size, they belong to the user's own host (nobody
// else can reach them) and they do not count as sessions. A host in
// exit_on_last_shell mode stays up until its results have expired.

package main

import (
	"encoding/binary"
	"sort"
	"time"
)

// maxJobResults bounds the results one host keeps (oldest evicted first).
const maxJobResults = 64

type jobResult struct {
	info   jobInfo
	output []byte
}

// keepResult stores the result of an ended job session. Called before the
// session leaves the table, so the host cannot exit in between.
func (h *host) keepResult(s *session, code int, reason string) {
	if !s.job || h.cfg.JobRetention <= 0 {
		return
	}
	s.mu.Lock()
	var out []byte
	if s.ring != nil {
		snap, trunc := s.ring.Snapshot()
		out = append([]byte(nil), cleanReplay(snap, trunc)...)
	}
	s.mu.Unlock()
	now := time.Now()
	r := &jobResult{
		info: jobInfo{Name: s.Name(), Cmd: s.cmdline, Created: s.created, Ended: now,
			ExitCode: code, Reason: reason, Expires: now.Add(h.cfg.JobRetention)},
		output: out,
	}
	h.mu.Lock()
	if h.results == nil {
		h.results = map[string]*jobResult{}
	}
	h.results[s.Name()] = r
	for len(h.results) > maxJobResults {
		oldest := ""
		for n, x := range h.results {
			if oldest == "" || x.info.Ended.Before(h.results[oldest].info.Ended) {
				oldest = n
			}
		}
		delete(h.results, oldest)
	}
	h.mu.Unlock()
}

// expireResults drops results past their retention. The caller holds h.mu.
func (h *host) expireResults() {
	if len(h.results) == 0 {
		return
	}
	now := time.Now()
	for n, r := range h.results {
		if now.After(r.info.Expires) {
			delete(h.results, n)
		}
	}
	if len(h.results) == 0 {
		h.emptySince = now
	}
}

func (h *host) getResult(name string) *jobResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireResults()
	return h.results[name]
}

func (h *host) listJobs() []jobInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireResults()
	out := make([]jobInfo, 0, len(h.results))
	for _, r := range h.results {
		out = append(out, r.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ended.Before(out[j].Ended) })
	return out
}

// dropResult discards a result; it reports whether there was one.
func (h *host) dropResult(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.results[name]
	delete(h.results, name)
	if ok && len(h.results) == 0 && len(h.sessions) == 0 {
		h.emptySince = time.Now()
	}
	return ok
}

// serveResult answers an attach to a finished job: the output it left, then
// its exit status, exactly as if the client had been attached when it ended.
func (h *host) serveResult(fc *framed, r *jobResult, hl *hello, peer peerInfo) {
	si := sessionInfo{Name: r.info.Name, Created: r.info.Created, Last: r.info.Ended, Cmd: r.info.Cmd}
	if err := replyOK(fc, okReply{Session: &si}); err != nil {
		return
	}
	for off := 0; off < len(r.output); off += 32 * 1024 {
		end := off + 32*1024
		if end > len(r.output) {
			end = len(r.output)
		}
		if fc.write(msgData, r.output[off:end]) != nil {
			return
		}
	}
	var st [4]byte
	binary.BigEndian.PutUint32(st[:], uint32(int32(r.info.ExitCode)))
	if fc.write(msgExit, st[:]) != nil {
		return
	}
	h.audit.log("job_collect", peer.fields(fields{"session": r.info.Name,
		"exit_code": r.info.ExitCode, "discard": hl.Remove}))
	if hl.Remove {
		h.dropResult(r.info.Name)
	}
}
