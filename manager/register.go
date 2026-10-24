package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// DropIn is the file this component owns in the ACP core's agents.d
// directory. The core's own agents.json always wins over it on a name; the
// manager writes nothing else there (docs/prd/2026-10-24-agent-manager.md
// §2.5). The entry's shape is the ACP core's agents.json entry, which the
// core owns: this component writes the documented fields and imports none of
// its code.
const (
	DropInDir  = "agents.d"
	DropInFile = "agent-manager.json"
)

type dropInEntry struct {
	Name        string            `json:"name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Profile     json.RawMessage   `json:"profile,omitempty"`
	Permissions json.RawMessage   `json:"permissions,omitempty"`
}

type dropIn struct {
	Agents []dropInEntry `json:"agents"`
}

// registration builds the drop-in from the agents the member gave to the ACP
// core: a managed one by its installed command, one found on the computer by
// the command found. Caller holds the lock.
func (s *Service) registration() dropIn {
	out := dropIn{Agents: []dropInEntry{}}
	set := s.Settings
	for i := range s.cat().Agents {
		e := &s.cat().Agents[i]
		a := s.state.Agents[e.ID]
		if e.Register == nil || a == nil || !a.Registered || !e.RunsHere() || !set.Allows(e.ID) {
			continue
		}
		command := ""
		if a.Version != "" && e.Install != nil {
			command = binPath(s.Store.versionDir(e.ID, a.Version), e.Install.Bin)
		} else if d := s.det[e.ID]; d.Command != "" {
			command = d.Command
		}
		if command == "" {
			continue
		}
		r := e.Register
		out.Agents = append(out.Agents, dropInEntry{Name: r.Name, Command: command, Args: r.Args, Env: r.Env, Profile: r.Profile, Permissions: r.Permissions})
	}
	return out
}

// writeRegistration brings the drop-in file up to date: written atomically,
// only when its content changed (the core reloads on a change), removed when
// no agent is registered. With no ACP home known it does nothing.
func (s *Service) writeRegistration() error {
	if s.ACPHome == "" {
		return nil
	}
	s.mu.Lock()
	doc := s.registration()
	s.mu.Unlock()
	path := filepath.Join(s.ACPHome, DropInDir, DropInFile)
	if len(doc.Agents) == 0 {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, raw) {
		return nil
	}
	return writeFileAtomic(path, raw, 0o600)
}
