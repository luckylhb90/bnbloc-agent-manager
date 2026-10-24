package manager

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Agents added by configuration (docs/prd/2026-10-24-agent-manager.md §2.3):
// the catalog is signed, so what an installed catalog may run is fixed; these
// are the two ways to add an agent without a new build, and they differ in who
// they may make run a command:
//
//   - A member's own (`myAgents`, kept on their computer, never sent anywhere):
//     anything their own computer can run: a command or a path, its version
//     flags, an updater and its arguments, an ACP launch line. They could run
//     it themselves; the page shows an update's exact command before the click.
//   - An organization's (`orgAgents`, set by an admin for every member):
//     recognition only, a bare command name and one version flag from a short
//     list, plus words for the member. No updater, no ACP launch line, and no
//     change to a catalog entry: a setting must never make members' computers run
//     what an admin typed (a build of the manager, signed as a package, is how an
//     organization ships entries that run commands: Catalog.With).
//
// Each agent is a JSON object. The setting's lines are read as one text and cut
// into objects by their braces, so an object may sit on one line or be written
// over several, and two may share a line (splitObjects). An object that does not
// fit, and text outside any object, is named on the page and skipped, never
// dropped silently.
const (
	SettingOrgAgents = "orgAgents"
	SettingMyAgents  = "myAgents"
	// SourceMember and SourceOrg label a configured row.
	SourceMember = "you"
	SourceOrg    = "your organization"

	maxConfigLine = 2048
	maxConfigArgs = 8
	maxConfigArg  = 512
	// configuredKind classifies an install found for a configured agent, so
	// its update (when the member gave one) applies.
	configuredKind = "configured"
)

// orgVersionFlags are the only version arguments an organization's line may
// name: they ask a program to say who it is and do nothing else.
var orgVersionFlags = []string{"--version", "-v", "-V", "version"}

// ConfiguredAgent is one line.
type ConfiguredAgent struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Command       string   `json:"command"`
	VersionArgs   []string `json:"versionArgs,omitempty"`
	VersionParse  string   `json:"versionParse,omitempty"`
	UpdateCommand string   `json:"updateCommand,omitempty"`
	UpdateArgs    []string `json:"updateArgs,omitempty"`
	// InstallCommand and InstallArgs install the agent when it is not on the
	// computer (a member's own; the page shows the command before the click).
	InstallCommand string   `json:"installCommand,omitempty"`
	InstallArgs    []string `json:"installArgs,omitempty"`
	ACPArgs        []string `json:"acpArgs,omitempty"`
	InstallHint    string   `json:"installHint,omitempty"`
	SignIn         string   `json:"signIn,omitempty"`
}

func decodeLine(line string) (ConfiguredAgent, error) {
	var a ConfiguredAgent
	if len(line) > maxConfigLine {
		return a, fmt.Errorf("longer than %d characters", maxConfigLine)
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("not a JSON object of known fields (%v)", err)
	}
	return a, nil
}

func checkArgs(what string, args []string) error {
	if len(args) > maxConfigArgs {
		return fmt.Errorf("%s has more than %d arguments", what, maxConfigArgs)
	}
	for _, a := range args {
		if a == "" || len(a) > maxConfigArg || strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("%s has an argument that is empty, longer than %d or holds a line break", what, maxConfigArg)
		}
	}
	return nil
}

// check validates a line for its scope.
func (a *ConfiguredAgent) check(scope string) error {
	if !idRE.MatchString(a.ID) {
		return fmt.Errorf("id must match %s", idRE)
	}
	member := scope == SourceMember
	switch {
	case a.Command == "":
		return fmt.Errorf("command is required")
	case member && !filepath.IsAbs(a.Command) && !binRE.MatchString(a.Command):
		return fmt.Errorf("command must be a command name or an absolute path")
	case !member && !binRE.MatchString(a.Command):
		return fmt.Errorf("an organization's command must be a bare command name")
	}
	if !member {
		switch {
		case len(a.UpdateArgs) > 0 || a.UpdateCommand != "" || len(a.InstallArgs) > 0 || a.InstallCommand != "":
			return fmt.Errorf("an organization cannot set an install or update command: only a member sets one for their own computer (an organization ships one in a signed build of the manager)")
		case len(a.ACPArgs) > 0:
			return fmt.Errorf("an organization cannot set an ACP launch line: only a member sets one for their own computer")
		case len(a.VersionArgs) > 1 || (len(a.VersionArgs) == 1 && !slices.Contains(orgVersionFlags, a.VersionArgs[0])):
			return fmt.Errorf("an organization's version argument must be one of %s", strings.Join(orgVersionFlags, ", "))
		}
	}
	if err := checkArgs("versionArgs", a.VersionArgs); err != nil {
		return err
	}
	if err := checkArgs("updateArgs", a.UpdateArgs); err != nil {
		return err
	}
	if err := checkArgs("acpArgs", a.ACPArgs); err != nil {
		return err
	}
	if err := checkArgs("installArgs", a.InstallArgs); err != nil {
		return err
	}
	if a.InstallCommand != "" && !binRE.MatchString(a.InstallCommand) {
		return fmt.Errorf("installCommand must be a bare command name")
	}
	if (a.InstallCommand != "") != (len(a.InstallArgs) > 0) && a.InstallCommand != "" {
		return fmt.Errorf("installCommand needs installArgs")
	}
	if a.UpdateCommand != "" && !binRE.MatchString(a.UpdateCommand) {
		return fmt.Errorf("updateCommand must be a bare command name")
	}
	if a.UpdateCommand != "" && len(a.UpdateArgs) == 0 {
		return fmt.Errorf("updateCommand needs updateArgs")
	}
	if a.VersionParse != "" {
		re, err := regexp.Compile(a.VersionParse)
		if err != nil || re.NumSubexp() < 1 {
			return fmt.Errorf("versionParse must be a regular expression with a group for the version")
		}
	}
	return nil
}

// overridesOnly reports a line that only says where an existing agent is: the
// fields that may change one.
func (a ConfiguredAgent) overridesOnly() bool {
	return a.Name == "" && a.UpdateCommand == "" && len(a.UpdateArgs) == 0 && a.InstallCommand == "" && len(a.InstallArgs) == 0 && len(a.ACPArgs) == 0 && a.InstallHint == "" && a.SignIn == ""
}

func (a ConfiguredAgent) detect() Detect {
	args := a.VersionArgs
	d := Detect{Command: a.Command, Args: slices.Clone(args), Parse: a.VersionParse}
	if d.Parse != "" {
		d.parse = regexp.MustCompile(d.Parse) // checked by check
	}
	return d
}

// entry is a new agent from a line.
func (a ConfiguredAgent) entry(source string) Entry {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	e := Entry{ID: a.ID, Name: name, Source: source, Detect: a.detect(), InstallHint: a.InstallHint, SignIn: a.SignIn,
		Found: []Found{{Kind: configuredKind, Contains: "/"}}}
	if len(e.Detect.Args) == 0 {
		// A version is read the usual way unless the line says otherwise.
		e.Detect.Args = []string{"--version"}
	}
	if len(a.UpdateArgs) > 0 {
		e.Update = []Update{{Kind: configuredKind, Command: a.UpdateCommand, Args: slices.Clone(a.UpdateArgs)}}
	}
	if len(a.InstallArgs) > 0 {
		// A command name for the installer, else a shell: the member's own
		// install line, as they would type it.
		cmd := a.InstallCommand
		if cmd == "" {
			cmd = "sh"
		}
		e.InstallRun = &Run{Command: cmd, Args: slices.Clone(a.InstallArgs)}
	}
	if len(a.ACPArgs) > 0 && agentNameRE.MatchString(a.ID) {
		e.Register = &Register{Name: a.ID, Args: slices.Clone(a.ACPArgs)}
	}
	return e
}

// With the lines applied: the effective catalog for the lines' agents, and a
// note for every line left out. The base catalog is not changed.
func (c *Catalog) WithConfigured(orgLines, memberLines []string) (*Catalog, []string) {
	out := &Catalog{Version: c.Version, Agents: slices.Clone(c.Agents)}
	var notes []string
	index := func(id string) int {
		return slices.IndexFunc(out.Agents, func(e Entry) bool { return e.ID == id })
	}
	registered := map[string]bool{}
	for _, e := range out.Agents {
		if e.Register != nil {
			registered[e.Register.Name] = true
		}
	}
	apply := func(lines []string, scope, label string) {
		objects, stray := splitObjects(strings.Join(lines, "\n"))
		for _, n := range stray {
			notes = append(notes, label+": "+n)
		}
		for _, line := range objects {
			a, err := decodeLine(line)
			if err == nil {
				err = a.check(scope)
			}
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s: %s: %v", label, shorten(line), err))
				continue
			}
			if i := index(a.ID); i >= 0 {
				// An existing agent: a member may say where it is; nothing else changes.
				if scope != SourceMember || !a.overridesOnly() {
					notes = append(notes, fmt.Sprintf("%s: %q is already an agent: only a member may point it at another command, with just id, command and versionArgs", label, a.ID))
					continue
				}
				e := out.Agents[i]
				e.Detect = a.detect()
				if len(e.Detect.Args) == 0 {
					e.Detect.Args = out.Agents[i].Detect.Args
				}
				e.Source = scope
				out.Agents[i] = e
				continue
			}
			e := a.entry(scope)
			if e.Register != nil && registered[e.Register.Name] {
				notes = append(notes, fmt.Sprintf("%s: %q: another agent is already given to rooms under that name, so this one is listed and not registered", label, a.ID))
				e.Register = nil
			}
			if e.Register != nil {
				registered[e.Register.Name] = true
			}
			out.Agents = append(out.Agents, e)
		}
	}
	apply(orgLines, SourceOrg, SettingOrgAgents)
	apply(memberLines, SourceMember, SettingMyAgents)
	return out, notes
}

// splitObjects cuts text into its top-level {…} objects (braces inside strings
// do not count) and says what it could not use: text between objects, and an
// object that is never closed.
func splitObjects(text string) (objects, notes []string) {
	depth, start := 0, -1
	inString, escaped := false, false
	stray := false
	for i, r := range text {
		if depth > 0 {
			switch {
			case inString && escaped:
				escaped = false
			case inString && r == '\\':
				escaped = true
			case r == '"':
				inString = !inString
			case !inString && r == '{':
				depth++
			case !inString && r == '}':
				depth--
				if depth == 0 {
					objects = append(objects, text[start:i+1])
				}
			}
			continue
		}
		switch {
		case r == '{':
			depth, start, inString, escaped = 1, i, false, false
		case r != ' ' && r != '\t' && r != '\n' && r != '\r' && !stray:
			stray = true
			notes = append(notes, fmt.Sprintf("text outside any {…} was ignored (near %q)", shorten(strings.TrimSpace(text[i:]))))
		}
	}
	if depth > 0 {
		notes = append(notes, fmt.Sprintf("an object is never closed: %s", shorten(strings.TrimSpace(text[start:]))))
	}
	return objects, notes
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
