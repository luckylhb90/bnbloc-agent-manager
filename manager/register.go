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
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Sandbox "guest": the agent exists only in the member's sandbox and Command
	// is a command line of the guest (the ACP core's own field).
	Sandbox     string          `json:"sandbox,omitempty"`
	Profile     json.RawMessage `json:"profile,omitempty"`
	Permissions json.RawMessage `json:"permissions,omitempty"`
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
		if e.Sandbox != nil {
			if a.Version != "" && s.SandboxReady() {
				out.Agents = append(out.Agents, dropInEntry{Name: e.Register.Name, Command: guestCommand(e), Args: e.Register.Args, Sandbox: "guest", Profile: e.Register.Profile, Permissions: e.Register.Permissions})
			}
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
		env := r.Env
		if r.CommandEnv != "" {
			if served := s.servedCommand(e.For); served != "" {
				env = map[string]string{}
				for k, v := range r.Env {
					env[k] = v
				}
				env[r.CommandEnv] = served
			}
		}
		out.Agents = append(out.Agents, dropInEntry{Name: r.Name, Command: command, Args: r.Args, Env: env, Profile: r.Profile, Permissions: r.Permissions})
	}
	return out
}

// servedCommand is the command of the agent an adapter serves: its managed
// copy, else the one found on the computer ("" when neither). Caller holds the
// lock.
func (s *Service) servedCommand(id string) string {
	e, ok := s.cat().Entry(id)
	if !ok {
		return ""
	}
	if a := s.state.Agents[id]; a != nil && a.Version != "" && e.Install != nil {
		return binPath(s.Store.versionDir(id, a.Version), e.Install.Bin)
	}
	return s.det[id].Command
}

// writeRegistration brings the drop-in file up to date: written atomically,
// only when its content changed (the core reloads on a change), removed when
// no agent is registered. With no ACP home known it does nothing.
func (s *Service) writeRegistration() error {
	if err := s.writeMounts(); err != nil {
		return err
	}
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

// OwnConfigFile is the member's own agent list in the ACP core's home; it wins
// over this component's drop-in on a name.
const OwnConfigFile = "agents.json"

// ownNames are the agent names the member's own file defines. Only the names
// are read (the schema is the ACP core's); a file that is missing or does not
// parse names none.
func (s *Service) ownNames() map[string]bool {
	out := map[string]bool{}
	if s.ACPHome == "" {
		return out
	}
	raw, err := os.ReadFile(filepath.Join(s.ACPHome, OwnConfigFile))
	if err != nil {
		return out
	}
	var doc struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, a := range doc.Agents {
		out[a.Name] = true
	}
	return out
}
