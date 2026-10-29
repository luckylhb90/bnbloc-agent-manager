package manager

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

//go:embed catalog.json
var embeddedCatalog []byte

// Catalog is the data that decides what this component may install, detect
// and update on a member's computer. It ships inside the signed package, so
// only a published component version changes it; no setting adds an entry
// (docs/prd/2026-10-24-agent-manager.md §2.3). An entry is typed, never a
// shell string: every method is one the code below knows how to verify.
type Catalog struct {
	// Version is the catalog's own revision, shown on the page.
	Version string  `json:"version"`
	Agents  []Entry `json:"agents"`
}

// Entry is one agent.
type Entry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// InstallRun is a member's own installer for an agent not on their
	// computer: a command and its arguments, run on their click after the page
	// shows it. Only configuration sets it (a catalog file cannot: json:"-").
	InstallRun *Run `json:"-"`
	// Source says who added a configured entry ("" for the catalog's own):
	// shown on its row. Never read from a catalog file.
	Source string `json:"-"`
	// For names the agent this entry serves: the ACP adapter an agent needs
	// for rooms is its own entry (its own install, version and registration),
	// shown under the agent the member knows.
	For string `json:"for,omitempty"`
	// Platforms are the GOOS values the agent runs on (empty: all).
	Platforms []string `json:"platforms,omitempty"`
	// Install is how the manager installs it into its own store. Empty: the
	// manager does not install it and InstallHint says how the member does.
	Install     *Install `json:"install,omitempty"`
	InstallHint string   `json:"installHint,omitempty"`
	// Sandbox makes the agent one that lives only in the member's sandbox:
	// nothing is installed or run on this computer (sandbox.go). Such an entry
	// has no Install and needs no Detect.
	Sandbox *SandboxInstall `json:"sandbox,omitempty"`
	// Latest says where the newest version is read from.
	Latest *Latest `json:"latest,omitempty"`
	// Detect finds an install the manager did not make.
	Detect Detect `json:"detect"`
	// Found classifies such an install by where it sits.
	Found []Found `json:"found,omitempty"`
	// Update is how an install found as Kind updates itself.
	Update []Update `json:"update,omitempty"`
	// Register is how the ACP core launches it. Nil: listed and kept current,
	// not registered (the agent has no ACP launch line).
	Register *Register `json:"register,omitempty"`
	// SignIn is shown after an install: each agent keeps its own sign-in.
	SignIn string `json:"signIn,omitempty"`
	// SandboxOf is, on the sandbox line of an agent that also runs on this computer, the id of
	// the agent it belongs to. Made when the catalog is read (addSandboxTwins), never written in
	// a catalog file: the two lines have their own state, version, store folder and registration.
	SandboxOf string `json:"-"`
}

// sandboxTwinSuffix ends the id and the registered name of an agent's sandbox line.
const sandboxTwinSuffix = "-sandbox"

// hasHostSide reports whether the agent can be on this computer at all: an agent with none
// (OpenCode 2) lives only in the sandbox and keeps one line.
func (e *Entry) hasHostSide() bool {
	return e.Install != nil || e.InstallRun != nil || len(e.Found) > 0 || e.Detect.Command != ""
}

// isSandboxLine reports whether the entry is where an agent lives in the sandbox: the twin of
// an agent that also runs on this computer, or an agent that has no other side.
func (e *Entry) isSandboxLine() bool {
	return e.SandboxOf != "" || (e.Sandbox != nil && !e.hasHostSide())
}

// baseName is the agent's own name: what a sentence about it says, whichever line it is on.
func (e *Entry) baseName() string {
	if e.SandboxOf != "" {
		return strings.TrimSuffix(e.Name, " (sandbox)")
	}
	return e.Name
}

// settingsID is the id the organization's settings name the agent by: both lines of an agent
// are one agent to them (offered, pinned, sandbox-only).
func (e *Entry) settingsID() string {
	if e.SandboxOf != "" {
		return e.SandboxOf
	}
	return e.ID
}

// sandboxTwin is the sandbox line of an agent that also runs on this computer: the same package
// and version source, none of the detection of an install on this computer, and its own
// registered name. Its Install stays (it says how the package is built: the pip and npm
// methods read it) but is never run on this computer for this line.
func (e *Entry) sandboxTwin() Entry {
	t := *e
	t.ID = e.ID + sandboxTwinSuffix
	t.Name = e.Name + " (sandbox)"
	t.SandboxOf = e.ID
	t.Detect, t.Found, t.Update, t.InstallRun, t.InstallHint = Detect{}, nil, nil, nil, ""
	t.Register = nil
	sb := *e.Sandbox
	if r := e.sandboxRegister(); r != nil {
		rc := *r
		rc.Name += sandboxTwinSuffix
		t.Register = &rc
	}
	sb.Register = nil
	t.Sandbox = &sb
	return t
}

// addSandboxTwins gives every agent that runs on this computer and can be put in the sandbox a
// second line, right after it. An id or registered name a twin would take twice is refused.
func (c *Catalog) addSandboxTwins() error {
	out := make([]Entry, 0, len(c.Agents)*2)
	taken := map[string]bool{}
	for _, e := range c.Agents {
		taken[e.ID] = true
		if e.Register != nil {
			taken["name:"+e.Register.Name] = true
		}
	}
	for i := range c.Agents {
		e := &c.Agents[i]
		out = append(out, *e)
		if e.Sandbox == nil || !e.hasHostSide() {
			continue
		}
		t := e.sandboxTwin()
		if taken[t.ID] || (t.Register != nil && taken["name:"+t.Register.Name]) {
			return fmt.Errorf("catalog: agent %q: its sandbox line %q (or its registered name) is already taken", e.ID, t.ID)
		}
		taken[t.ID] = true
		if t.Register != nil {
			taken["name:"+t.Register.Name] = true
		}
		out = append(out, t)
	}
	c.Agents = out
	return nil
}

// Run is a command with its arguments.
type Run struct {
	Command string
	Args    []string
}

// Install is how a package is put into <store>/<id>/<version>/: with npm (the
// default) or with pip (a Python agent, see pip.go).
type Install struct {
	Method  string `json:"method"`
	Package string `json:"package"`
	Bin     string `json:"bin"`
	// Extras are the package's own optional dependency groups the install
	// takes (pip only: hermes-agent[acp]).
	Extras []string `json:"extras,omitempty"`
	// Python is the range of interpreters the package runs on (pip only; a
	// version is major.minor): the install looks for one on this computer.
	Python *PythonRange `json:"python,omitempty"`
	// IgnoreScripts runs npm with --ignore-scripts (the default when absent).
	IgnoreScripts *bool `json:"ignoreScripts,omitempty"`
	// Requires are tools that must be present, in the login-shell environment.
	Requires []Require `json:"requires,omitempty"`
}

// PythonRange is the interpreters a Python package runs on, inclusive.
type PythonRange struct {
	Min string `json:"min"`
	Max string `json:"max,omitempty"`
}

// Require is a tool prerequisite.
type Require struct {
	Tool string `json:"tool"`
	Min  string `json:"min,omitempty"`
	Hint string `json:"hint,omitempty"`
}

// Latest is where the newest version comes from: "npm" (the npm registry) or
// "pypi" (the Python Package Index).
type Latest struct {
	From    string `json:"from"`
	Package string `json:"package,omitempty"`
}

// Detect names the command and how its version is read. Args empty: the
// command is not run and a found install has no known version.
type Detect struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Parse is a regexp whose last capture group is the version; empty reads
	// the first x.y.z.
	Parse string `json:"parse,omitempty"`

	parse *regexp.Regexp
}

// Found is a rule that names the kind of an install by its resolved path:
// Contains is a lower-cased, slash-separated fragment of it, Dir a directory
// (with ~ and ${VAR}) that exists beside it. Either matching is enough.
type Found struct {
	Kind     string `json:"kind"`
	Contains string `json:"contains,omitempty"`
	Dir      string `json:"dir,omitempty"`
	// Owner is who updates this kind when the manager cannot, shown to the
	// member.
	Owner string `json:"owner,omitempty"`
}

// Update is the updater for an install found as Kind. Args run the command
// Detect names (or Command's, a package manager); PinArgs, with {version}, install a chosen version.
// No PinArgs: the agent cannot install a chosen version, so an organization's
// pin disables its update instead of being ignored.
type Update struct {
	Kind string `json:"kind"`
	// Command is the bare name of the program that runs Args when it is not
	// the agent's own (a package manager: `brew upgrade <formula>`); empty
	// runs the agent's command.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args"`
	PinArgs []string `json:"pinArgs,omitempty"`
}

// Register is the entry the ACP core gets (the shape of its agents.json
// entry, which it owns); Command is filled in with the installed path.
type Register struct {
	Name string `json:"name"`
	// CommandEnv names an environment variable of the launched adapter that is
	// given the command of the agent it serves (an entry with `for`): the
	// managed copy, else the one found on the computer. For an adapter that runs
	// its agent by name and would not find a managed copy on the PATH. Nothing
	// is set when the agent is not on the computer.
	CommandEnv  string            `json:"commandEnv,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Profile     json.RawMessage   `json:"profile,omitempty"`
	Permissions json.RawMessage   `json:"permissions,omitempty"`
}

var (
	idRE         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	packageRE    = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	binRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	agentNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	commandEnvRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	// pypiNameRE is a distribution name as PyPI normalises it (letters, digits,
	// dot, dash, underscore).
	pypiNameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	pyVersionRE = regexp.MustCompile(`^3\.[0-9]{1,2}$`)
)

// deniedEnv is a variable that changes how a program loads or runs, which a
// catalog's commandEnv may never name.
func deniedEnv(name string) bool {
	switch name {
	case "PATH", "HOME", "NODE_OPTIONS", "BASH_ENV", "ENV", "SHELL", "IFS", "PYTHONPATH", "PYTHONSTARTUP":
		return true
	}
	return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "BNBLOC_")
}

// DefaultCatalog parses the catalog the binary ships.
func DefaultCatalog() (*Catalog, error) { return ParseCatalog(embeddedCatalog) }

// With returns the catalog with another one's agents added: the way an
// organization's build carries entries of its own beside the public ones (it is
// a build, signed as its own package, never a setting). The result is validated
// as one catalog, so an id or a registered name taken twice, or an adapter of an
// agent that does not exist, is refused.
func (c *Catalog) With(extra []byte) (*Catalog, error) {
	var more Catalog
	dec := json.NewDecoder(strings.NewReader(string(extra)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&more); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	base := slices.DeleteFunc(slices.Clone(c.Agents), func(e Entry) bool { return e.SandboxOf != "" }) // made again by ParseCatalog
	raw, err := json.Marshal(Catalog{Version: c.Version + "+" + more.Version, Agents: append(base, more.Agents...)})
	if err != nil {
		return nil, err
	}
	return ParseCatalog(raw)
}

// ParseCatalog decodes and validates a catalog; an invalid one is an error,
// never a partial list.
func ParseCatalog(raw []byte) (*Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for i := range c.Agents {
		e := &c.Agents[i]
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("catalog: agent %q: %w", e.ID, err)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("catalog: duplicate agent %q", e.ID)
		}
		seen[e.ID] = true
		if e.Register != nil {
			if names[e.Register.Name] {
				return nil, fmt.Errorf("catalog: duplicate registered name %q", e.Register.Name)
			}
			names[e.Register.Name] = true
		}
	}
	for _, e := range c.Agents {
		if e.For == "" {
			continue
		}
		parent, ok := c.Entry(e.For)
		if !ok || e.For == e.ID || parent.For != "" {
			return nil, fmt.Errorf("catalog: agent %q is for %q, which is not an agent of its own", e.ID, e.For)
		}
	}
	if err := c.addSandboxTwins(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (e *Entry) validate() error {
	if !idRE.MatchString(e.ID) {
		return fmt.Errorf("id must match %s", idRE)
	}
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if e.Detect.Command == "" && (e.Sandbox == nil || e.Install != nil || len(e.Found) > 0) {
		return fmt.Errorf("detect.command must be a bare command name")
	}
	if e.Detect.Command != "" && strings.ContainsAny(e.Detect.Command, `/\ `) {
		return fmt.Errorf("detect.command must be a bare command name")
	}
	if e.Sandbox != nil {
		if err := e.Sandbox.validate(e); err != nil {
			return err
		}
	}
	if e.Detect.Parse != "" {
		re, err := regexp.Compile(e.Detect.Parse)
		if err != nil {
			return fmt.Errorf("detect.parse: %w", err)
		}
		if re.NumSubexp() < 1 {
			return fmt.Errorf("detect.parse needs a capture group for the version")
		}
		e.Detect.parse = re
	}
	if in := e.Install; in != nil {
		switch in.Method {
		case "npm":
			if !packageRE.MatchString(in.Package) {
				return fmt.Errorf("install.package %q is not an npm package name", in.Package)
			}
			if len(in.Extras) > 0 || in.Python != nil {
				return fmt.Errorf("install.extras and install.python are for the pip method")
			}
		case "pip":
			if !pypiNameRE.MatchString(in.Package) {
				return fmt.Errorf("install.package %q is not a Python package name", in.Package)
			}
			for _, x := range in.Extras {
				if !pypiNameRE.MatchString(x) {
					return fmt.Errorf("install.extras %q is not a name", x)
				}
			}
			if in.Python == nil || !pyVersionRE.MatchString(in.Python.Min) || (in.Python.Max != "" && !pyVersionRE.MatchString(in.Python.Max)) {
				return fmt.Errorf("install.python needs min (and max) as major.minor")
			}
		default:
			return fmt.Errorf("install.method %q is not a method this component has (npm, pip)", in.Method)
		}
		if !binRE.MatchString(in.Bin) {
			return fmt.Errorf("install.bin %q is not a command name", in.Bin)
		}
		for _, r := range in.Requires {
			if !binRE.MatchString(r.Tool) {
				return fmt.Errorf("requires.tool %q is not a command name", r.Tool)
			}
		}
	}
	if l := e.Latest; l != nil {
		if l.From != "npm" && l.From != "pypi" {
			return fmt.Errorf("latest.from %q is not a source this component has (npm, pypi)", l.From)
		}
		if l.Package == "" && e.Install == nil {
			return fmt.Errorf("latest.from %s needs a package", l.From)
		}
		if l.Package != "" && !packageRE.MatchString(l.Package) && !pypiNameRE.MatchString(l.Package) {
			return fmt.Errorf("latest.package %q is not a package name", l.Package)
		}
		if l.From == "pypi" && e.Install != nil && e.Install.Method != "pip" && l.Package == "" {
			return fmt.Errorf("latest.from pypi on an npm install needs its own package")
		}
	}
	for _, f := range e.Found {
		if f.Kind == "" || (f.Contains == "" && f.Dir == "") {
			return fmt.Errorf("found rules need a kind and a contains or dir")
		}
		if f.Kind == KindManaged || f.Kind == KindNPMGlobal || f.Kind == KindUnknown {
			return fmt.Errorf("found kind %q is the component's own", f.Kind)
		}
	}
	for _, u := range e.Update {
		if len(u.Args) == 0 || !slices.ContainsFunc(e.Found, func(f Found) bool { return f.Kind == u.Kind }) {
			return fmt.Errorf("update for %q needs args and a found rule of that kind", u.Kind)
		}
		if u.Command != "" && !binRE.MatchString(u.Command) {
			return fmt.Errorf("update.command %q must be a bare command name", u.Command)
		}
		if len(u.PinArgs) > 0 && !slices.Contains(u.PinArgs, "{version}") {
			return fmt.Errorf("update.pinArgs for %q must carry {version}", u.Kind)
		}
	}
	if r := e.Register; r != nil {
		if !agentNameRE.MatchString(r.Name) {
			return fmt.Errorf("register.name %q is not an agent name", r.Name)
		}
		if r.CommandEnv != "" && (!commandEnvRE.MatchString(r.CommandEnv) || deniedEnv(r.CommandEnv) || e.For == "") {
			return fmt.Errorf("register.commandEnv %q must be an upper-case variable name that does not change how a process runs, on an entry with `for`", r.CommandEnv)
		}
		for _, raw := range []json.RawMessage{r.Profile, r.Permissions} {
			if len(raw) > 0 && !json.Valid(raw) {
				return fmt.Errorf("register: profile and permissions must be JSON")
			}
		}
	}
	return nil
}

// Entry returns the catalog entry with the id.
func (c *Catalog) Entry(id string) (*Entry, bool) {
	for i := range c.Agents {
		if c.Agents[i].ID == id {
			return &c.Agents[i], true
		}
	}
	return nil, false
}

// RunsHere reports whether the agent runs on this operating system.
func (e *Entry) RunsHere() bool {
	return len(e.Platforms) == 0 || slices.Contains(e.Platforms, runtime.GOOS)
}

// hasVersionSource says whether a look for the newest version can be made: a package in an
// index, or a release page.
func (e *Entry) hasVersionSource(placement string) bool {
	if placement == PlaceSandbox && e.Sandbox != nil && e.Sandbox.Release != nil {
		return true
	}
	return e.packageIn(placement) != ""
}

// latestFrom is the index the newest version of the agent in a placement is read
// from: the sandbox's own package comes from the npm registry unless the entry's
// install is a Python package that the sandbox builds from the same wheels.
func (e *Entry) latestFrom(placement string) string {
	if placement != PlaceSandbox && e.Latest != nil && e.Latest.From == "pypi" {
		return "pypi"
	}
	if placement == PlaceSandbox && e.Sandbox != nil && e.Sandbox.method() == sandboxPip {
		return "pypi"
	}
	if e.Install != nil && e.Install.Method == "pip" && (e.Latest == nil || e.Latest.From == "") {
		return "pypi"
	}
	return "npm"
}

// Package is the package the newest version is read from.
func (e *Entry) Package() string {
	if e.Latest != nil && e.Latest.Package != "" {
		return e.Latest.Package
	}
	if e.Install != nil {
		return e.Install.Package
	}
	return ""
}

// Found kinds the component names itself; an entry's rules name the others.
const (
	KindManaged   = "managed"
	KindNPMGlobal = "npm-global"
	KindUnknown   = "unknown"
)
