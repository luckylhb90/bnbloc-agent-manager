package manager

import (
	"os"
	"strconv"
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
	// Held are versions kept (beyond the current and the previous one) because a
	// room's conversation still runs from them.
	Held []string `json:"held,omitempty"`
}

// PlacementView is one place an agent can be installed: this computer or the
// member's sandbox. Every agent row carries both, so the choice is the same for
// every agent; one that cannot be used says why.
type PlacementView struct {
	ID    string `json:"id"` // "host" or "sandbox"
	Label string `json:"label"`
	Can   bool   `json:"can"`
	// Reason says why it cannot be chosen (empty when it can).
	Reason string `json:"reason,omitempty"`
	// Note says what choosing it means (size, what is different).
	Note string `json:"note,omitempty"`
}

// SandboxOfferView is what the page shows beside "Install in my sandbox".
type SandboxOfferView struct {
	ApproxMB int    `json:"approxMB,omitempty"`
	Note     string `json:"note,omitempty"`
	// Only: the organization offers this agent only in the sandbox.
	Only bool `json:"only,omitempty"`
	// NeedsSignIn: it works only after signing in to its provider, which the
	// sandbox cannot do yet: shown disabled, with why.
	NeedsSignIn bool `json:"needsSignIn,omitempty"`
	// HostToo: the agent can also be installed on this computer, so the button
	// must say where.
	HostToo bool `json:"hostToo,omitempty"`
}

// Can lists what the page may offer for the row.
type Can struct {
	Install bool `json:"install"`
	// InstallSandbox: put it in the sandbox instead of on this computer.
	InstallSandbox bool `json:"installSandbox"`
	Update         bool `json:"update"`
	Rollback       bool `json:"rollback"`
	Remove         bool `json:"remove"`
	Register       bool `json:"register"`
	Unregister     bool `json:"unregister"`
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
	Reason     string `json:"reason,omitempty"`
	Can        Can    `json:"can"`
	Registered bool   `json:"registered"`
	HasACP     bool   `json:"hasACP"`
	// Sandbox: the manager's install of it lives in the member's sandbox (nothing
	// of it is on this computer). SandboxOffer is set when it can be put there.
	Sandbox      bool              `json:"sandbox,omitempty"`
	SandboxOffer *SandboxOfferView `json:"sandboxOffer,omitempty"`
	// Placements are the two places to install it, when it is not installed by
	// the manager yet (empty once it is: it stays where it was put).
	Placements []PlacementView `json:"placements,omitempty"`
	// Shadowed: the member's own agents.json defines the name this row is given
	// to rooms under, so their entry is the one rooms run, not this one.
	Shadowed    bool    `json:"shadowed,omitempty"`
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
		Registered: a.Registered && e.Register != nil, Sandbox: placementIn(&a) == PlaceSandbox,
	}
	if v.Sandbox {
		// What the host install tells the member to do (sign in in a terminal) is not
		// available in the sandbox.
		v.SignIn = sandboxSignIn
	}
	sandboxOnly := false
	if e.Sandbox != nil {
		s.mu.Lock()
		sandboxOnly = s.Settings.SandboxOnly[e.ID]
		s.mu.Unlock()
		v.SandboxOffer = &SandboxOfferView{ApproxMB: e.Sandbox.ApproxMB, Note: e.Sandbox.Note, Only: sandboxOnly, HostToo: e.Install != nil || e.InstallRun != nil || len(e.Found) > 0 || e.Detect.Command != ""}
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
		v.Managed = &Managed{Version: a.Version, Previous: a.Previous, InstalledAt: a.InstalledAt, SizeBytes: s.size(e, a.Version, a.Previous)}
		if e.Sandbox == nil {
			v.Managed.Held = held(s.heldVersions(e.ID), a.Version, a.Previous)
		}
		if a.Previous != "" {
			if _, err := os.Stat(s.storeOf(e).versionDir(e.ID, a.Previous)); err != nil {
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

	v.Can.Install = (e.Install != nil || e.InstallRun != nil) && !managed && v.External == nil && !busy && !sandboxOnly
	v.Can.InstallSandbox = e.Sandbox != nil && e.sandboxRefusal() == "" && s.SandboxReady() && !managed && !busy
	if e.InstallRun != nil && v.Can.Install {
		v.InstallCommand = e.InstallRun.Command + " " + strings.Join(e.InstallRun.Args, " ")
	}
	if !managed && !busy && e.For == "" {
		v.Placements = s.placements(e, &v, sandboxOnly)
	}
	if !managed && e.Install == nil && e.InstallHint != "" && v.External == nil {
		v.Reason = e.InstallHint
	}
	v.Can.Rollback = managed && v.Managed.Previous != "" && !busy
	v.Can.Remove = managed && !busy
	if e.Register != nil && s.ownNames()[e.Register.Name] {
		v.Shadowed = true
	}
	if e.Register != nil && !busy && (managed || v.External != nil) {
		v.Can.Register, v.Can.Unregister = !v.Registered, v.Registered
	}

	switch {
	case busy:
	case managed:
		v.Can.Update = v.UpdateAvailable && (v.Sandbox && e.Sandbox != nil || !v.Sandbox && e.Install != nil)
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
func (s *Service) size(e *Entry, kept ...string) int64 {
	id := e.ID
	key := id + "\x00" + strings.Join(kept, "\x00")
	s.mu.Lock()
	if c, ok := s.sizes[id]; ok && c.key == key {
		s.mu.Unlock()
		return c.n
	}
	s.mu.Unlock()
	n := dirSize(s.storeOf(e).agentDir(id))
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

// held are the versions kept only for a room: the held ones that are neither the
// current one nor the one a rollback goes to.
func held(all []string, cur, prev string) []string {
	var out []string
	for _, v := range all {
		if v != cur && v != prev {
			out = append(out, v)
		}
	}
	return out
}

// placements builds the two choices of an agent that is not installed by the
// manager: the same two for every agent, each enabled or disabled with its reason.
func (s *Service) placements(e *Entry, v *AgentView, sandboxOnly bool) []PlacementView {
	host := PlacementView{ID: PlaceHost, Label: "This computer", Can: v.Can.Install}
	if !host.Can {
		switch {
		case sandboxOnly:
			host.Reason = "Your organization installs " + e.Name + " only in the sandbox."
		case v.External != nil:
			host.Reason = "Already on this computer, installed by you (" + v.External.Path + ")."
		case e.InstallHint != "":
			host.Reason = e.InstallHint
		default:
			host.Reason = "The manager does not install " + e.Name + " on this computer."
		}
	}
	sb := PlacementView{ID: PlaceSandbox, Label: "My sandbox", Can: v.Can.InstallSandbox}
	switch {
	case sb.Can:
		sb.Note = "Nothing of it is installed or run on this computer, and every room shares the one copy"
		if e.Sandbox.ApproxMB > 0 {
			sb.Note += " (about " + strconv.Itoa(e.Sandbox.ApproxMB) + " MB)"
		}
		sb.Note += "."
		if e.Sandbox.Note != "" {
			sb.Note += " " + e.Sandbox.Note
		}
		if e.Sandbox.SignIn == "key" {
			sb.Note += " " + sandboxKeyNote
		}
		if why := s.copyOnThisComputer(e); why != "" {
			sb.Note += " " + why
		}
	case e.sandboxRefusal() != "":
		sb.Reason = e.sandboxRefusal()
	case !s.SandboxReady():
		sb.Reason = "Needs the desktop app with the sandbox on."
	}
	return []PlacementView{host, sb}
}
