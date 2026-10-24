package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// DefaultRegistry is the npm registry; only https registries are accepted.
const DefaultRegistry = "https://registry.npmjs.org"

// maxRegistryDoc bounds a registry answer: a popular package lists every
// version it ever published.
const maxRegistryDoc = 64 << 20

// registryDoc is the part of a registry answer used here.
type registryDoc struct {
	DistTags map[string]string          `json:"dist-tags"`
	Time     map[string]time.Time       `json:"time"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// latestFromRegistry reads the newest stable version of a package. With
// holdDays above 0 it is the newest one published at least that long ago and
// not newer than the registry's own `latest`: a release that just went out is
// not offered yet.
func (s *Service) latestFromRegistry(ctx context.Context, pkg string, holdDays int) (string, error) {
	base := s.RegistryURL
	if base == "" {
		base = DefaultRegistry
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("the registry %q is not an https address", base)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+url.PathEscape(pkg), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the registry did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("the package %s was not found in the registry", pkg)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the registry answered %d", resp.StatusCode)
	}
	var doc registryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRegistryDoc)).Decode(&doc); err != nil {
		return "", fmt.Errorf("the registry's answer for %s could not be read: %w", pkg, err)
	}
	latest := doc.DistTags["latest"]
	if !ValidVersion(latest) {
		return "", fmt.Errorf("the registry names no stable latest version of %s", pkg)
	}
	if holdDays <= 0 {
		return latest, nil
	}
	cutoff := s.now().Add(-time.Duration(holdDays) * 24 * time.Hour)
	best := ""
	for v := range doc.Versions {
		if !ValidVersion(v) || Compare(v, latest) > 0 {
			continue
		}
		// A pre-release is offered only when the registry's own latest is one.
		if prerelease(v) != "" && prerelease(latest) == "" {
			continue
		}
		if t, ok := doc.Time[v]; !ok || t.After(cutoff) {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no version of %s is older than %d days", pkg, holdDays)
	}
	return best, nil
}

// binPath is the command inside an installed version directory.
func binPath(versionDir, bin string) string {
	if runtime.GOOS == "windows" {
		bin += ".cmd"
	}
	return filepath.Join(versionDir, "node_modules", ".bin", bin)
}

// checkRequires verifies the install's tool prerequisites, in the
// environment this process has.
func (s *Service) checkRequires(ctx context.Context, in *Install) error {
	for _, r := range in.Requires {
		path, err := exec.LookPath(r.Tool)
		if err != nil {
			return fmt.Errorf("%s is needed and was not found. %s", r.Tool, r.Hint)
		}
		if r.Min == "" {
			continue
		}
		ctx2, cancel := context.WithTimeout(ctx, versionTimeout)
		out, _ := exec.CommandContext(ctx2, path, "--version").CombinedOutput()
		cancel()
		have := anyVersionRE.FindString(string(out))
		if have == "" || !AtLeast(have, r.Min) {
			if have == "" {
				have = "an unreadable version"
			}
			return fmt.Errorf("%s %s or newer is needed, found %s. %s", r.Tool, r.Min, have, r.Hint)
		}
	}
	return nil
}

// npmInstall installs pkg@version into a new directory of the store and
// verifies it before anything else may point at it: the version directory
// appears only complete, and a failure removes what was made.
func (s *Service) npmInstall(ctx context.Context, op *operation, e *Entry, version string) (string, error) {
	in := e.Install
	if !ValidVersion(version) {
		return "", fmt.Errorf("%q is not a version this component installs", version)
	}
	if err := s.checkRequires(ctx, in); err != nil {
		return "", err
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", fmt.Errorf("npm is needed and was not found. Install Node.js, then try again")
	}
	final := s.Store.versionDir(e.ID, version)
	partial := final + partialSuffix
	_ = os.RemoveAll(partial)
	if err := os.MkdirAll(partial, 0o700); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(partial)
		}
	}()
	args := []string{"install", "--prefix", partial, "--cache", s.Store.cacheDir(),
		"--no-audit", "--no-fund", "--no-update-notifier", "--loglevel=http"}
	if in.IgnoreScripts == nil || *in.IgnoreScripts {
		args = append(args, "--ignore-scripts")
	}
	args = append(args, in.Package+"@"+version)
	if err := s.run(ctx, op, npm, args...); err != nil {
		return "", err
	}
	bin := binPath(partial, in.Bin)
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("npm finished and %s is not in the package: the catalog's command name may be wrong", in.Bin)
	}
	if len(e.Detect.Args) > 0 {
		got := s.readVersion(ctx, e, bin)
		if got != version {
			if got == "" {
				got = "none"
			}
			return "", fmt.Errorf("the installed command reports version %s, not %s; not registered", got, version)
		}
	}
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	ok = true
	return final, nil
}

// npmGlobalUpdate updates an install the member made with npm -g.
func (s *Service) npmGlobalUpdate(ctx context.Context, op *operation, e *Entry, version string) error {
	if err := s.checkRequires(ctx, e.Install); err != nil {
		return err
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm is needed and was not found")
	}
	args := []string{"install", "-g", "--no-audit", "--no-fund", "--no-update-notifier", "--loglevel=http"}
	if e.Install.IgnoreScripts == nil || *e.Install.IgnoreScripts {
		args = append(args, "--ignore-scripts")
	}
	return s.run(ctx, op, npm, append(args, e.Install.Package+"@"+version)...)
}

// selfArgs is the command an Update click runs for an install the manager did
// not make: the entry's own, or its pinned form when the organization pinned a
// version (false: this install cannot be set to a chosen version).
func selfArgs(u Update, pin string) ([]string, bool) {
	if pin == "" {
		return slices.Clone(u.Args), true
	}
	if len(u.PinArgs) == 0 {
		return nil, false
	}
	out := make([]string, len(u.PinArgs))
	for i, a := range u.PinArgs {
		out[i] = strings.ReplaceAll(a, "{version}", pin)
	}
	return out, true
}
