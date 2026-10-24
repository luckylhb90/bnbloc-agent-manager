package manager

import (
	"regexp"
	"strconv"
	"strings"
)

// A version this component names a directory or an install command with is
// three plain numbers: never a pre-release, never a path. Everything a registry
// or an agent prints is checked against this before it is used.
var versionRE = regexp.MustCompile(`^\d{1,9}\.\d{1,9}\.\d{1,9}$`)

// anyVersionRE finds the first version in a program's output.
var anyVersionRE = regexp.MustCompile(`\d{1,9}\.\d{1,9}\.\d{1,9}`)

// ValidVersion reports whether v is a version this component accepts.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// ParseVersion reads the version out of a program's output: the first
// capture group of re when it has one (an entry's own rule), else the first
// x.y.z. "" means none was found.
func ParseVersion(out string, re *regexp.Regexp) string {
	if re != nil {
		if m := re.FindStringSubmatch(out); m != nil {
			v := m[len(m)-1]
			if ValidVersion(v) {
				return v
			}
		}
		return ""
	}
	if v := anyVersionRE.FindString(out); ValidVersion(v) {
		return v
	}
	return ""
}

// Compare orders two valid versions: -1, 0, 1. An invalid one sorts first.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parts(v string) [3]int {
	var out [3]int
	if !ValidVersion(v) {
		return out
	}
	for i, p := range strings.Split(v, ".") {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// AtLeast reports whether have >= min, comparing only the parts min names
// ("22" is any 22.x.y or newer, "22.19" any 22.19.x or newer).
func AtLeast(have, min string) bool {
	h := strings.Split(strings.TrimPrefix(have, "v"), ".")
	for i, m := range strings.Split(min, ".") {
		mi, err := strconv.Atoi(m)
		if err != nil {
			return false
		}
		hi := 0
		if i < len(h) {
			hi, _ = strconv.Atoi(h[i])
		}
		if hi != mi {
			return hi > mi
		}
	}
	return true
}
