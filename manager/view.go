package manager

import (
	"os"
	"strings"
	"time"
)

// Snapshot is what the page shows: every agent of the catalog, with what the
// member can do with each.
type Snapshot struct {
	Component      string      `json:"component"`
	CatalogVersion string      `json:"catalogVersion"`
	Detecting      bool        `json:"detecting"`
	Notes          []string    `json:"notes,omitempty"`
	Settings       SettingView `json:"settings"`
	Agents         []AgentView `json:"agents"`
}

// SettingView is the policy in force, shown so a member sees why a row is as
// it is.
type SettingView struct {
	HoldDays   int  `json:"holdDays"`
	AutoUpdate bool `json:"autoUpdate"`
}

// Managed is an install the manager made.
type Managed struct {
	Version     string    `json:"version"`
	Previous    string    `json:"previous,omitempty"`
	InstalledAt time.Time `json:"installedAt,omitzero"`
	SizeBytes   int64     `json:"sizeBytes"`
}

// Can lists what the page may offer for the row.
type Can struct {
	Install    bool `json:"install"`
	Update     bool `json:"update"`
	Rollback   bool `json:"rollback"`
	Remove     bool `json:"remove"`
	Register   bool `json:"register"`
	Unregister bool `json:"unregister"`
}

// AgentView is one row.
type AgentView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// For is the agent this row is the ACP adapter of (shown under it);
	// Adapter is, on an agent, the id of its adapter row.
	For string `json:"for,omitempty"`
	// Source says who added the row ("you", "your organization"); empty for
	// the catalog's own.
	Source  string `json:"source,omitempty"`
	Adapter string `json:"adapter,omitempty"`
	// Status: "unsupported" (not for this OS), "blocked" (the organization
	// does not offer it), "absent", "installed".
	Status   string    `json:"status"`
	Managed  *Managed  `json:"managed,omitempty"`
	External *Detected `json:"external,omitempty"`
	// Latest is the newest version offered (the pin when pinned); Target is
	// what an update moves to.
	Latest          string    `json:"latest,omitempty"`
	Target          string    `json:"target,omitempty"`
	CheckedAt       time.Time `json:"checkedAt,omitzero"`
	CheckError      string    `json:"checkError,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	// UpdateUnknown: the agent is found and has an updater of its own, but no
	// newer version is known: the page offers to run it without claiming one.
	UpdateUnknown bool   `json:"updateUnknown,omitempty"`
	Pinned        string `json:"pinned,omitempty"`
	// InstallCommand is shown before a member's own installer runs.
	InstallCommand string `json:"installCommand,omitempty"`
	// UpdateCommand is shown before an update of an install the manager did
	// not make.
	UpdateCommand string `json:"updateCommand,omitempty"`
	// Reason says why an action is unavailable, in words for the member.
	Reason      string  `json:"reason,omitempty"`
	Can         Can     `json:"can"`
	Registered  bool    `json:"registered"`
	HasACP      bool    `json:"hasACP"`
	InstallHint string  `json:"installHint,omitempty"`
	SignIn      string  `json:"signIn,omitempty"`
	Op          *OpView `json:"op,omitempty"`
}

// Snapshot builds the page's picture from what was last read; it runs no
// command and makes no network call.
func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	snap := Snapshot{
		Component:      s.Version,
		CatalogVersion: s.cat().Version,
		Detecting:      s.detectedAt.IsZero(),
		Settings:       SettingView{HoldDays: s.Settings.HoldDays, AutoUpdate: s.Settings.AutoUpdate},
		Notes:          append([]string(nil), s.Settings.Notes...),
	}
	if s.settingsErr != "" {
		snap.Notes = append(snap.Notes, "settings: "+s.settingsErr)
	}
	s.mu.Unlock()
	for i := range s.cat().Agents {
		snap.Agents = append(snap.Agents, s.agentView(&s.cat().Agents[i]))
	}
	return snap
}

func (s *Service) agentView(e *Entry) AgentView {
	s.mu.Lock()
	a := AgentState{}
	if rec := s.state.Agents[e.ID]; rec != nil {
		a = *rec
	}
	d := s.det[e.ID]
	allowed := s.Settings.Allows(e.ID)
	pin := s.Settings.Pinned[e.ID]
	op := s.ops[e.ID]
	detecting := s.detectedAt.IsZero()
	s.mu.Unlock()

	v := AgentView{
		ID: e.ID, Name: e.Name, For: e.For, Source: e.Source, Adapter: s.adapterOf(e.ID), Pinned: pin, HasACP: e.Register != nil,
		InstallHint: e.InstallHint, SignIn: e.SignIn,
		Latest: a.Latest, CheckedAt: a.CheckedAt, CheckError: a.CheckErr,
		Registered: a.Registered && e.Register != nil,
	}
	if op != nil {
		v.Op = op.view()
	}
	busy := v.Op != nil && v.Op.Running

	switch {
	case !e.RunsHere():
		v.Status, v.Reason = "unsupported", e.Name+" does not run on this operating system."
		return v
	case !allowed:
		v.Status, v.Reason = "blocked", "Your organization does not offer "+e.Name+"."
	}

	managed := a.Version != ""
	if managed {
		v.Managed = &Managed{Version: a.Version, Previous: a.Previous, InstalledAt: a.InstalledAt, SizeBytes: s.size(e.ID, a.Version, a.Previous)}
		if a.Previous != "" {
			if _, err := os.Stat(s.Store.versionDir(e.ID, a.Previous)); err != nil {
				v.Managed.Previous = ""
			}
		}
	}
	if d.Command != "" && !detecting {
		ext := d
		v.External = &ext
	}
	if v.Status == "" {
		v.Status = "absent"
		if managed || v.External != nil {
			v.Status = "installed"
		}
	}
	if v.Status == "blocked" {
		v.Can.Remove = managed && !busy
		return v
	}

	installed := ""
	switch {
	case managed:
		installed = a.Version
	case v.External != nil:
		installed = v.External.Version
	}
	v.Target = a.Latest
	if pin != "" {
		v.Latest, v.Target = pin, pin
	}
	if installed != "" && v.Target != "" {
		if pin != "" {
			v.UpdateAvailable = installed != pin
		} else {
			v.UpdateAvailable = Compare(v.Target, installed) > 0
		}
	}

	v.Can.Install = (e.Install != nil || e.InstallRun != nil) && !managed && v.External == nil && !busy
	if e.InstallRun != nil && v.Can.Install {
		v.InstallCommand = e.InstallRun.Command + " " + strings.Join(e.InstallRun.Args, " ")
	}
	if !managed && e.Install == nil && e.InstallHint != "" && v.External == nil {
		v.Reason = e.InstallHint
	}
	v.Can.Rollback = managed && v.Managed.Previous != "" && !busy
	v.Can.Remove = managed && !busy
	if e.Register != nil && !busy && (managed || v.External != nil) {
		v.Can.Register, v.Can.Unregister = !v.Registered, v.Registered
	}

	switch {
	case busy:
	case managed:
		v.Can.Update = v.UpdateAvailable && e.Install != nil
	case v.External != nil:
		s.externalUpdate(e, &v, pin)
	}
	return v
}

// externalUpdate decides whether Update is offered for an install the manager
// did not make, and the command a click would run.
func (s *Service) externalUpdate(e *Entry, v *AgentView, pin string) {
	d := v.External
	if d.Kind == KindNPMGlobal && e.Install != nil {
		v.Can.Update = v.UpdateAvailable
		if v.Target != "" {
			v.UpdateCommand = "npm install -g " + e.Install.Package + "@" + v.Target
		}
		return
	}
	for _, u := range e.Update {
		if u.Kind != d.Kind {
			continue
		}
		args, ok := selfArgs(u, pin)
		if !ok {
			v.Reason = "Your organization pinned " + e.Name + " to " + pin + ", and this install cannot be set to a chosen version."
			return
		}
		program := d.Command
		if u.Command != "" {
			program = u.Command
		}
		v.UpdateCommand = program + " " + strings.Join(args, " ")
		if v.Target == "" {
			v.UpdateUnknown = true
			v.Can.Update = true
		} else {
			v.Can.Update = v.UpdateAvailable
		}
		return
	}
	if d.Owner != "" {
		v.Reason = "Updated by " + d.Owner + "."
	} else if d.Kind == KindUnknown {
		v.Reason = "Installed by you outside the manager; update it the way you installed it."
	}
}

// size is the bytes an agent's kept versions hold, measured once per
// (versions kept) so a page polling every second does not walk the disk.
func (s *Service) size(id string, kept ...string) int64 {
	key := id + "\x00" + strings.Join(kept, "\x00")
	s.mu.Lock()
	if c, ok := s.sizes[id]; ok && c.key == key {
		s.mu.Unlock()
		return c.n
	}
	s.mu.Unlock()
	n := dirSize(s.Store.agentDir(id))
	s.mu.Lock()
	if s.sizes == nil {
		s.sizes = map[string]sizeEntry{}
	}
	s.sizes[id] = sizeEntry{key: key, n: n}
	s.mu.Unlock()
	return n
}

type sizeEntry struct {
	key string
	n   int64
}

// adapterOf is the id of the entry that is the adapter of agent id ("" none).
func (s *Service) adapterOf(id string) string {
	for i := range s.cat().Agents {
		if s.cat().Agents[i].For == id {
			return s.cat().Agents[i].ID
		}
	}
	return ""
}
