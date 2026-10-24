package manager

import (
	"regexp"
	"strconv"
	"strings"
)

// A version this component names a directory or an install command with is
// three plain numbers and, optionally, a pre-release part of dot-separated
// letters, digits and hyphens (1.2.3, 0.2.1-rc.2): never a path, never build
// metadata. Everything a registry or an agent prints is checked against this
// before it is used.
const versionPattern = `\d{1,9}\.\d{1,9}\.\d{1,9}(?:-[0-9A-Za-z][0-9A-Za-z-]*(?:\.[0-9A-Za-z][0-9A-Za-z-]*){0,5})?`

var versionRE = regexp.MustCompile(`^` + versionPattern + `$`)

// anyVersionRE finds the first version in a program's output.
var anyVersionRE = regexp.MustCompile(versionPattern)

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

// Compare orders two valid versions as semver does: the numbers first, then a
// release above any pre-release of it, pre-release parts one by one (numbers
// below words, numbers by value). -1, 0, 1. An invalid one sorts first.
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
	ra, rb := prerelease(a), prerelease(b)
	switch {
	case ra == rb:
		return 0
	case ra == "":
		return 1
	case rb == "":
		return -1
	}
	ia, ib := strings.Split(ra, "."), strings.Split(rb, ".")
	for i := 0; i < len(ia) && i < len(ib); i++ {
		if c := compareIdent(ia[i], ib[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(ia) < len(ib):
		return -1
	case len(ia) > len(ib):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
		return 0
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// prerelease is the part after the hyphen ("" for a release).
func prerelease(v string) string {
	_, pre, _ := strings.Cut(v, "-")
	return pre
}

func parts(v string) [3]int {
	var out [3]int
	if !ValidVersion(v) {
		return out
	}
	core, _, _ := strings.Cut(v, "-")
	for i, p := range strings.Split(core, ".") {
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
