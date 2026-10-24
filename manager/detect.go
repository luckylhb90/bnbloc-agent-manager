package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Detected is what a look at the computer found for one agent.
type Detected struct {
	// Command is the command as the PATH resolves it, the path the ACP core
	// is given (not Path: a resolved link may not be directly executable).
	Command string `json:"-"`
	// Path is the command, symlinks resolved ("" = not found).
	Path string `json:"path,omitempty"`
	// Version is what the agent reported ("" = it reported none we can read).
	Version string `json:"version,omitempty"`
	// Kind is KindManaged, KindNPMGlobal, an entry's own kind or KindUnknown.
	Kind string `json:"kind,omitempty"`
	// Owner is who updates this kind when the manager cannot.
	Owner string `json:"owner,omitempty"`
}

// versionTimeout bounds one `--version` run. A program that does not answer
// (an ACP server waiting on stdin) is "version unknown", never a hang.
const versionTimeout = 10 * time.Second

// detect looks for an install the manager did not make: the command on the
// PATH this process has (the desktop gives its children the member's
// login-shell environment).
func (s *Service) detect(ctx context.Context, e *Entry) Detected {
	path, err := exec.LookPath(e.Detect.Command)
	if err != nil {
		return Detected{}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	command := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	d := Detected{Command: command, Path: path}
	d.Kind, d.Owner = classify(e, path, s.Store.Dir)
	if len(e.Detect.Args) > 0 {
		d.Version = s.readVersion(ctx, e, path)
	}
	return d
}

// readVersion runs the command's version flag with no input and a bound.
func (s *Service) readVersion(ctx context.Context, e *Entry, path string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, e.Detect.Args...)
	cmd.Stdin = nil // the null device
	out, _ := cmd.CombinedOutput()
	if len(out) > 8192 {
		out = out[:8192]
	}
	return ParseVersion(string(out), e.Detect.parse)
}

// classify names how an install came to be, from where it resolves: the
// manager's own store, an npm global prefix, a place an entry's rule names, or
// unknown.
func classify(e *Entry, path, storeDir string) (kind, owner string) {
	p := normPath(path)
	if strings.HasPrefix(p, normPath(storeDir)+"/") {
		return KindManaged, ""
	}
	for _, f := range e.Found {
		if (f.Contains != "" && strings.Contains(p, strings.ToLower(f.Contains))) ||
			(f.Dir != "" && dirExists(expandDir(f.Dir))) {
			return f.Kind, f.Owner
		}
	}
	if e.Install != nil && inNPMGlobal(p) {
		return KindNPMGlobal, ""
	}
	return KindUnknown, ""
}

// inNPMGlobal reports a path inside an npm global prefix: <prefix>/lib/node_modules
// (Unix) or <prefix>/npm/node_modules (Windows). A node_modules anywhere else
// is a project's own (a tool that carries its copy), which `npm install -g`
// would not update, so it is not a global install.
func inNPMGlobal(p string) bool {
	return strings.Contains(p, "/lib/node_modules/") || strings.Contains(p, "/npm/node_modules/")
}

func normPath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// expandDir expands ~ and ${VAR} in a catalog directory; an unset variable
// leaves the path empty, which matches nothing.
func expandDir(dir string) string {
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, dir[2:])
	}
	unset := false
	dir = os.Expand(dir, func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			unset = true
		}
		return v
	})
	if unset {
		return ""
	}
	return filepath.FromSlash(dir)
}

func dirExists(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}
