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
}

// Run is a command with its arguments.
type Run struct {
	Command string
	Args    []string
}

// Install is the npm method: a package into <store>/<id>/<version>/.
type Install struct {
	Method  string `json:"method"`
	Package string `json:"package"`
	Bin     string `json:"bin"`
	// IgnoreScripts runs npm with --ignore-scripts (the default when absent).
	IgnoreScripts *bool `json:"ignoreScripts,omitempty"`
	// Requires are tools that must be present, in the login-shell environment.
	Requires []Require `json:"requires,omitempty"`
}

// Require is a tool prerequisite.
type Require struct {
	Tool string `json:"tool"`
	Min  string `json:"min,omitempty"`
	Hint string `json:"hint,omitempty"`
}

// Latest is where the newest version comes from. Only "npm" is built.
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
	raw, err := json.Marshal(Catalog{Version: c.Version + "+" + more.Version, Agents: append(append([]Entry(nil), c.Agents...), more.Agents...)})
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
	return &c, nil
}

func (e *Entry) validate() error {
	if !idRE.MatchString(e.ID) {
		return fmt.Errorf("id must match %s", idRE)
	}
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if e.Sandbox != nil {
		// Nothing of a sandbox agent is on this computer to look for.
		if e.Detect.Command != "" {
			return fmt.Errorf("a sandbox agent has no detect: none of it is on this computer")
		}
		if err := e.Sandbox.validate(e); err != nil {
			return err
		}
	} else if e.Detect.Command == "" || strings.ContainsAny(e.Detect.Command, `/\ `) {
		return fmt.Errorf("detect.command must be a bare command name")
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
		if in.Method != "npm" {
			return fmt.Errorf("install.method %q is not a method this component has (npm)", in.Method)
		}
		if !packageRE.MatchString(in.Package) {
			return fmt.Errorf("install.package %q is not an npm package name", in.Package)
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
		if l.From != "npm" {
			return fmt.Errorf("latest.from %q is not a source this component has (npm)", l.From)
		}
		if l.Package == "" && e.Install == nil {
			return fmt.Errorf("latest.from npm needs a package")
		}
		if l.Package != "" && !packageRE.MatchString(l.Package) {
			return fmt.Errorf("latest.package %q is not an npm package name", l.Package)
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

// Package is the npm package the newest version is read from.
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
