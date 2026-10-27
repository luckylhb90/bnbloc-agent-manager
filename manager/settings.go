package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The component's declared settings (package.json `settings`). The desktop
// writes them to the file BNBLOC_COMPONENT_SETTINGS_FILE names at launch: an
// owner-only JSON object by key, a `lines` setting an array of strings, any
// other a string, an unset one absent.
const (
	SettingAllowed    = "allowed"    // org: agent ids offered (empty: all)
	SettingPinned     = "pinned"     // org: id@version lines
	SettingHoldDays   = "holdDays"   // org: a version younger than this is not offered
	SettingAutoUpdate = "autoUpdate" // member: off | on
	// SettingSandboxOnly: org: agent ids offered only for the sandbox.
	SettingSandboxOnly = "sandboxOnly"
)

// Settings are the declared settings' values, checked.
type Settings struct {
	Allowed    []string
	Pinned     map[string]string
	HoldDays   int
	AutoUpdate bool
	// SandboxOnly are the agents the organization offers only in the member's
	// sandbox. It governs what the manager offers (as Allowed and Pinned do): an
	// agent the member installs some other way is outside it.
	SandboxOnly map[string]bool
	// OrgAgents and MemberAgents are the lines of configured agents
	// (config.go).
	OrgAgents    []string
	MemberAgents []string
	// Notes name every line that was ignored and why, shown on the page: a
	// setting is never dropped silently.
	Notes []string
}

// ReadSettings reads the settings file against the base catalog and answers
// the settings with the effective catalog: the base and the agents the settings
// configure. An empty path is no settings; a file that is named but unreadable
// is an error.
func ReadSettings(path string, base *Catalog) (Settings, *Catalog, error) {
	s := Settings{Pinned: map[string]string{}, SandboxOnly: map[string]bool{}}
	if path == "" {
		return s, base, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, base, fmt.Errorf("component settings file: %w", err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return s, base, errors.New("component settings file: not a JSON object")
	}
	lines := func(key string) []string {
		var out []string
		if v, ok := values[key]; ok {
			_ = json.Unmarshal(v, &out)
		}
		return out
	}
	str := func(key string) string {
		var out string
		if v, ok := values[key]; ok {
			_ = json.Unmarshal(v, &out)
		}
		return strings.TrimSpace(out)
	}
	s.OrgAgents, s.MemberAgents = lines(SettingOrgAgents), lines(SettingMyAgents)
	c, notes := base.WithConfigured(s.OrgAgents, s.MemberAgents)
	s.Notes = append(s.Notes, notes...)
	for _, id := range lines(SettingAllowed) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := c.Entry(id); !ok {
			s.Notes = append(s.Notes, fmt.Sprintf("allowed: %q is not an agent of this catalog", id))
			continue
		}
		s.Allowed = append(s.Allowed, id)
	}
	for _, line := range lines(SettingPinned) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, v, ok := strings.Cut(line, "@")
		id, v = strings.TrimSpace(id), strings.TrimSpace(v)
		if _, known := c.Entry(id); !ok || !known || !ValidVersion(v) {
			s.Notes = append(s.Notes, fmt.Sprintf("pinned: %q is not <agent id>@<x.y.z> of an agent in this catalog", line))
			continue
		}
		s.Pinned[id] = v
	}
	for _, line := range lines(SettingSandboxOnly) {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if e, ok := c.Entry(id); !ok || e.Sandbox == nil {
			s.Notes = append(s.Notes, fmt.Sprintf("sandboxOnly: %q is not an agent of this catalog that can live in the sandbox", id))
			continue
		}
		s.SandboxOnly[id] = true
	}
	if h := str(SettingHoldDays); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 || n > 365 {
			s.Notes = append(s.Notes, fmt.Sprintf("holdDays: %q is not a number of days from 0 to 365", h))
		} else {
			s.HoldDays = n
		}
	}
	s.AutoUpdate = str(SettingAutoUpdate) == "on"
	return s, c, nil
}

// Allows reports whether the organization offers the agent.
func (s Settings) Allows(id string) bool {
	if len(s.Allowed) == 0 {
		return true
	}
	for _, a := range s.Allowed {
		if a == id {
			return true
		}
	}
	return false
}
