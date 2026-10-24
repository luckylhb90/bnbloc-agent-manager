package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentState is what the manager recorded about one agent it manages. An
// agent found on the computer that it did not install has no record here
// unless the member chose "use in rooms" for it.
type AgentState struct {
	// Version is the installed version's directory ("" = none installed);
	// Previous is the one kept for a rollback.
	Version  string `json:"version,omitempty"`
	Previous string `json:"previous,omitempty"`
	Method   string `json:"method,omitempty"`
	// InstalledAt is when Version was installed.
	InstalledAt time.Time `json:"installedAt,omitzero"`
	// Registered: the ACP core is given this agent (the member's click).
	Registered bool `json:"registered,omitempty"`
	// Latest, CheckedAt and CheckErr are the last look for a newer version.
	Latest    string    `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	CheckErr  string    `json:"checkError,omitempty"`
}

// State is state.json.
type State struct {
	Agents map[string]*AgentState `json:"agents"`
}

// Store is the manager's data directory: <dir>/state.json, the agents under
// <dir>/agents/<id>/<version>/ and the npm cache under <dir>/npm-cache.
type Store struct{ Dir string }

func (s Store) statePath() string              { return filepath.Join(s.Dir, "state.json") }
func (s Store) agentDir(id string) string      { return filepath.Join(s.Dir, "agents", id) }
func (s Store) versionDir(id, v string) string { return filepath.Join(s.agentDir(id), v) }
func (s Store) cacheDir() string               { return filepath.Join(s.Dir, "npm-cache") }

// partialSuffix marks a version directory that is not finished.
const partialSuffix = ".partial"

// Load reads the state; a missing file is an empty state, an unreadable one
// is an error (never silently reset: it records what was installed).
func (s Store) Load() (State, error) {
	st := State{Agents: map[string]*AgentState{}}
	raw, err := os.ReadFile(s.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("state: %w", err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{Agents: map[string]*AgentState{}}, fmt.Errorf("state: %w", err)
	}
	if st.Agents == nil {
		st.Agents = map[string]*AgentState{}
	}
	return st, nil
}

// Save writes the state atomically, owner-only.
func (s Store) Save(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.statePath(), raw, 0o600)
}

// writeFileAtomic writes path through a temporary file and a rename, so a
// reader sees the old or the new content and never half of it.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// SweepPartials removes version directories an interrupted install left.
func (s Store) SweepPartials() {
	dirs, _ := os.ReadDir(filepath.Join(s.Dir, "agents"))
	for _, d := range dirs {
		inner, _ := os.ReadDir(s.agentDir(d.Name()))
		for _, v := range inner {
			if strings.HasSuffix(v.Name(), partialSuffix) {
				_ = os.RemoveAll(filepath.Join(s.agentDir(d.Name()), v.Name()))
			}
		}
	}
}

// Prune removes every version of the agent but the ones kept, and any
// partial directory. A directory that cannot be removed (a process runs from
// it on Windows) is left for the next prune, never forced.
func (s Store) Prune(id string, keep ...string) {
	entries, _ := os.ReadDir(s.agentDir(id))
	for _, e := range entries {
		keepIt := false
		for _, k := range keep {
			if k != "" && e.Name() == k {
				keepIt = true
			}
		}
		if !keepIt {
			_ = os.RemoveAll(filepath.Join(s.agentDir(id), e.Name()))
		}
	}
}

// size is the bytes a directory holds (0 when absent).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
