package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What the ACP core's live sessions run from. A session continues, after an idle
// stop, with the program it was created with: a version directory removed while a
// session is idle loses its conversation (docs/prd/2026-10-24-agent-manager.md
// §2.4a, reproduced against the real core). The core publishes the paths its
// sessions hold in <acp home>/in-use.json (its own format, owned by bnbloc-acp);
// this component reads it before it removes a file.
//
// Three states, because only one of them licenses removal:
//
//   - current: the core wrote it recently and has not stopped: its sessions are
//     alive and hold those paths. Nothing they name is removed.
//   - gone: the core stopped cleanly, or was killed and stopped renewing it: its
//     sessions are gone with it. Removal is safe.
//   - unknown: no file. Either no core has run here, or the core predates the file
//     and its sessions cannot be seen. Nothing is removed.
type inUseState int

const (
	inUseUnknown inUseState = iota
	inUseCurrent
	inUseGone
)

// inUseFresh is how long a file counts as the core's current word: the core
// renews it every minute.
const inUseFresh = 5 * time.Minute

type inUseFile struct {
	UpdatedAtMs int64 `json:"updatedAtMs"`
	Stopped     bool  `json:"stopped"`
	Sessions    []struct {
		Agent string   `json:"agent"`
		Refs  []string `json:"refs"`
	} `json:"sessions"`
}

// readInUse reads the core's file: its state and every path-like string its
// sessions hold.
func (s *Service) readInUse() (inUseState, []string) {
	if s.ACPHome == "" {
		return inUseUnknown, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.ACPHome, "in-use.json"))
	if err != nil {
		return inUseUnknown, nil
	}
	var f inUseFile
	if json.Unmarshal(raw, &f) != nil {
		return inUseUnknown, nil // a file that cannot be read is no word at all
	}
	if f.Stopped || s.now().Sub(time.UnixMilli(f.UpdatedAtMs)) > inUseFresh {
		return inUseGone, nil
	}
	var refs []string
	for _, sess := range f.Sessions {
		refs = append(refs, sess.Refs...)
	}
	return inUseCurrent, refs
}

// heldBy reports whether any ref names a file inside dir.
func heldBy(dir string, refs []string) bool {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	for _, r := range refs {
		if strings.HasPrefix(filepath.Clean(r), prefix) {
			return true
		}
	}
	return false
}

// HeldVersions are the versions of an agent the live sessions still run from
// (empty unless the core's file is current).
func (s *Service) heldVersions(id string) []string {
	state, refs := s.readInUse()
	if state != inUseCurrent {
		return nil
	}
	var out []string
	entries, _ := os.ReadDir(s.Store.agentDir(id))
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), partialSuffix) && heldBy(filepath.Join(s.Store.agentDir(id), e.Name()), refs) {
			out = append(out, e.Name())
		}
	}
	return out
}

// gc removes the versions of an agent that nothing needs: not the current one,
// not the one a rollback goes to, and none a live session runs from. With no
// word from the core (unknown) nothing is removed. A directory that cannot be
// removed is left for the next pass.
func (s *Service) gc(id string) {
	state, _ := s.readInUse()
	if state == inUseUnknown {
		return
	}
	cur, prev := s.managedVersion(id)
	keep := append([]string{cur, prev}, s.heldVersions(id)...)
	s.Store.Prune(id, keep...)
}

// gcAll runs gc for every agent the manager installed.
func (s *Service) gcAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.state.Agents))
	for id, a := range s.state.Agents {
		if a.Version != "" {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.gc(id)
	}
}
