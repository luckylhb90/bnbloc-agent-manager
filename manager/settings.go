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
)

// Settings are the declared settings' values, checked.
type Settings struct {
	Allowed    []string
	Pinned     map[string]string
	HoldDays   int
	AutoUpdate bool
	// Notes name every line that was ignored and why, shown on the page: a
	// setting is never dropped silently.
	Notes []string
}

// ReadSettings reads the settings file. An empty path is no settings; a file
// that is named but unreadable is an error.
func ReadSettings(path string, c *Catalog) (Settings, error) {
	s := Settings{Pinned: map[string]string{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("component settings file: %w", err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return s, errors.New("component settings file: not a JSON object")
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
	if h := str(SettingHoldDays); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 || n > 365 {
			s.Notes = append(s.Notes, fmt.Sprintf("holdDays: %q is not a number of days from 0 to 365", h))
		} else {
			s.HoldDays = n
		}
	}
	s.AutoUpdate = str(SettingAutoUpdate) == "on"
	return s, nil
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
