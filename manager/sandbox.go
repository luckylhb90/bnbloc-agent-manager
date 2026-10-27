package manager

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// An agent that lives only in the member's sandbox (docs/prd/
// 2026-10-26-sandbox-only-agents-design.md in bnbloc). Its files are a Linux
// build that cannot run on this computer, so nothing of it is installed or run
// here: the manager downloads the package's tarball, checks it against the
// registry's integrity hash and unpacks it (no npm, no script of the package)
// into a folder of its own under the workspaces root. The bridge gives each of
// the member's rooms a tree of hard links to that folder inside its sandbox
// space, and the ACP core launches the agent there. The folder is the whole
// install: a version folder is never changed once it is published.

// SandboxInstall is how an agent is made available in the sandbox.
type SandboxInstall struct {
	// Package is the npm package whose tarball holds the agent's files for the
	// guest; {cpu} is the guest's architecture (arm64 or x64).
	Package string `json:"package"`
	// Bin is the agent's program inside the tarball (below its root).
	Bin string `json:"bin"`
	// Latest is the npm package the newest sandbox version is read from, when
	// it is not the entry's own (an agent whose newest generation ships under
	// another name).
	Latest string `json:"latest,omitempty"`
	// ApproxMB is shown before the choice is made: what the one copy takes.
	ApproxMB int `json:"approxMB,omitempty"`
	// Note says what differs from installing on this computer (shown on the
	// choice).
	Note string `json:"note,omitempty"`
}

var sandboxBinRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

func (in *SandboxInstall) validate(e *Entry) error {
	if !packageRE.MatchString(strings.ReplaceAll(in.Package, "{cpu}", "arm64")) {
		return fmt.Errorf("sandbox.package %q is not an npm package name", in.Package)
	}
	if !sandboxBinRE.MatchString(in.Bin) {
		return fmt.Errorf("sandbox.bin %q is not a path inside the package", in.Bin)
	}
	if in.Latest != "" && !packageRE.MatchString(in.Latest) {
		return fmt.Errorf("sandbox.latest %q is not an npm package name", in.Latest)
	}
	if in.Latest == "" && e.Package() == "" {
		return errors.New("a sandbox agent needs a package to read its version from (sandbox.latest, latest.package or install.package)")
	}
	if e.Register == nil {
		return errors.New("a sandbox agent needs register: rooms are the only way to run it")
	}
	if e.For != "" {
		return errors.New("an adapter is not put in the sandbox")
	}
	return nil
}

// guestCPU is the architecture of the sandbox's guest: the computer's own (the
// sandbox is a virtual machine of this computer).
func guestCPU() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// guestPackage is the package to unpack for this computer's guest.
func (in *SandboxInstall) guestPackage() string {
	return strings.ReplaceAll(in.Package, "{cpu}", guestCPU())
}

// SandboxReady reports whether the desktop gave this app the places a sandbox
// agent needs: the folder the machine shares and the file the bridge reads the
// mounts from.
func (s *Service) SandboxReady() bool {
	return s.MountRoot != "" && s.MountsFile != "" && s.GuestBase != ""
}

// sandboxStore is the store of sandbox agents: its agents live in
// <mount root>/agents/<id>/<version>.
func (s *Service) sandboxStore() Store { return Store{Dir: s.MountRoot} }

// PlaceSandbox and PlaceHost say where a managed install lives.
const (
	PlaceHost    = "host"
	PlaceSandbox = "sandbox"
)

// placementOf is where the manager's install of the agent lives ("" when it
// has none): recorded when it was installed, never guessed.
func (s *Service) placementOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return placementIn(s.state.Agents[id])
}

func placementIn(a *AgentState) string {
	switch {
	case a == nil || a.Version == "":
		return ""
	case a.Method == "sandbox":
		return PlaceSandbox
	}
	return PlaceHost
}

// storeOf is where the versions of an agent live.
func (s *Service) storeOf(e *Entry) Store {
	if s.placementOf(e.ID) == PlaceSandbox {
		return s.sandboxStore()
	}
	return s.Store
}

// sandboxPackage is the npm package the sandbox's newest version is read from.
func (e *Entry) sandboxPackage() string {
	if e.Sandbox != nil && e.Sandbox.Latest != "" {
		return e.Sandbox.Latest
	}
	return e.Package()
}

// packageIn is the package the newest version of the agent in a placement is
// read from.
func (e *Entry) packageIn(placement string) string {
	if placement == PlaceSandbox {
		return e.sandboxPackage()
	}
	return e.Package()
}

// mountEnv is the variable that names an agent's folder in every room's space.
func mountEnv(id string) string {
	return "BNBLOC_MOUNT_AGENT_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// guestCommand is the agent's command line in the guest: the variable and the
// program's path in the package.
func guestCommand(e *Entry) string { return "$" + mountEnv(e.ID) + "/" + e.Sandbox.Bin }

// sandboxLimits bound what an unpacked tarball may be.
const (
	maxTarFiles = 100_000
	maxTarBytes = 4 << 30
)

type tarballInfo struct {
	URL       string
	Integrity string // sha512-<base64>
}

// registryTarball reads where a version's tarball is and its integrity hash.
func (s *Service) registryTarball(ctx context.Context, pkg, version string) (tarballInfo, error) {
	base := s.RegistryURL
	if base == "" {
		base = DefaultRegistry
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return tarballInfo{}, fmt.Errorf("the registry %q is not an https address", base)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+url.PathEscape(pkg)+"/"+url.PathEscape(version), nil)
	if err != nil {
		return tarballInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return tarballInfo{}, fmt.Errorf("the registry did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return tarballInfo{}, fmt.Errorf("%s %s was not found in the registry", pkg, version)
	}
	if resp.StatusCode != http.StatusOK {
		return tarballInfo{}, fmt.Errorf("the registry answered %d", resp.StatusCode)
	}
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return tarballInfo{}, fmt.Errorf("the registry's answer for %s could not be read: %w", pkg, err)
	}
	if doc.Name != pkg || doc.Version != version {
		return tarballInfo{}, fmt.Errorf("the registry answered for %s %s, not %s %s", doc.Name, doc.Version, pkg, version)
	}
	if tu, err := url.Parse(doc.Dist.Tarball); err != nil || tu.Scheme != "https" || tu.Host == "" {
		return tarballInfo{}, fmt.Errorf("the registry's tarball address for %s is not an https address", pkg)
	}
	if !strings.HasPrefix(doc.Dist.Integrity, "sha512-") {
		return tarballInfo{}, fmt.Errorf("the registry gives no sha512 integrity hash for %s %s, so the download cannot be checked", pkg, version)
	}
	return tarballInfo{URL: doc.Dist.Tarball, Integrity: doc.Dist.Integrity}, nil
}

func (s *Service) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

// sandboxInstall puts the version of the agent's package into a new folder of
// the sandbox store and publishes it only whole: downloaded, checked against
// the registry's hash, unpacked, its program present and readable by the
// guest's user. A failure removes what was made.
func (s *Service) sandboxInstall(ctx context.Context, op *operation, e *Entry, version string) error {
	if !ValidVersion(version) {
		return fmt.Errorf("%q is not a version this component installs", version)
	}
	if !s.SandboxReady() {
		return errors.New("the sandbox's folders are not available to this app: use the desktop app with the sandbox on")
	}
	in := e.Sandbox
	pkg := in.guestPackage()
	info, err := s.registryTarball(ctx, pkg, version)
	if err != nil {
		return err
	}
	st := s.sandboxStore()
	final := st.versionDir(e.ID, version)
	partial := final + partialSuffix
	_ = os.RemoveAll(partial)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(partial)
		}
	}()
	op.logf("downloading %s %s", pkg, version)
	tgz, err := s.download(ctx, op, info, filepath.Join(partial, ".download.tgz"))
	if err != nil {
		return err
	}
	op.logf("unpacking")
	n, err := unpackTarball(tgz, partial, op.touch)
	_ = os.Remove(tgz)
	if err != nil {
		return err
	}
	op.logf("%d files", n)
	if err := checkPackageManifest(partial, pkg, version); err != nil {
		return err
	}
	bin := filepath.Join(partial, filepath.FromSlash(in.Bin))
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("the package has no program %s that can run: the catalog's path may be wrong", in.Bin)
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	ok = true
	return nil
}

// download fetches the tarball to dest, hashing as it goes, and refuses a file
// whose sha512 is not the registry's. Every chunk counts as activity.
func (s *Service) download(ctx context.Context, op *operation, info tarballInfo, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("the download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the download answered %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	buf := make([]byte, 256<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxTarBytes {
				return "", errors.New("the download is larger than this component accepts")
			}
			h.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				return "", err
			}
			op.touch()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("the download failed: %w", rerr)
		}
	}
	if got := "sha512-" + base64.StdEncoding.EncodeToString(h.Sum(nil)); got != info.Integrity {
		return "", errors.New("the download does not match the registry's integrity hash; nothing was installed")
	}
	return dest, nil
}

// unpackTarball extracts a package tarball into dir, dropping the leading
// "package/" folder. It accepts only files, folders and links that stay inside
// dir, gives a file the mode the guest's user needs (readable by all, runnable
// when the archive says so) and bounds the number and size of the files.
func unpackTarball(tgz, dir string, touch func()) (int, error) {
	f, err := os.Open(tgz)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("the package is not a gzip archive: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var files int
	var bytes int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("the package is damaged: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return files, fmt.Errorf("the package holds an unsafe path %q", h.Name)
		}
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[i+1:] // the archive's one top folder (package/)
		} else if h.Typeflag == tar.TypeDir {
			continue
		}
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if files++; files > maxTarFiles {
				return files, errors.New("the package holds more files than this component accepts")
			}
			if bytes += h.Size; bytes > maxTarBytes {
				return files, errors.New("the package is larger than this component accepts")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			mode := fs.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
			if err != nil {
				return files, err
			}
			_, cerr := io.Copy(out, tr)
			if err := errors.Join(cerr, out.Close()); err != nil {
				return files, err
			}
			if err := os.Chmod(target, mode); err != nil {
				return files, err
			}
			touch()
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) || strings.HasPrefix(path.Clean(path.Join(path.Dir(name), h.Linkname)), "..") {
				return files, fmt.Errorf("the package holds a link %q that leaves the package", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return files, err
			}
		default:
			return files, fmt.Errorf("the package holds %q, which is neither a file, a folder nor a link", h.Name)
		}
	}
}

// checkPackageManifest confirms that what was unpacked is the package and
// version asked for.
func checkPackageManifest(dir, pkg, version string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return errors.New("the package has no package.json")
	}
	var m struct{ Name, Version string }
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("the package's package.json cannot be read: %w", err)
	}
	if m.Name != pkg || m.Version != version {
		return fmt.Errorf("the package says it is %s %s, not %s %s", m.Name, m.Version, pkg, version)
	}
	return nil
}

// MountsFileName is the contract file the bridge reads (bnbloc.sandbox.mounts/v1).
const mountsContract = "bnbloc.sandbox.mounts/v1"

type mountEntry struct {
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	For      string `json:"for,omitempty"`
	PerSpace bool   `json:"perSpace,omitempty"`
	Env      string `json:"env,omitempty"`
}

// mountsDoc is what the bridge is told: every registered sandbox agent's
// current folder, once per room's space.
func (s *Service) mountsDoc() []mountEntry {
	out := []mountEntry{}
	for i := range s.cat().Agents {
		e := &s.cat().Agents[i]
		a := s.state.Agents[e.ID]
		if e.Sandbox == nil || placementIn(a) != PlaceSandbox || !a.Registered || !s.Settings.Allows(e.ID) {
			continue
		}
		out = append(out, mountEntry{
			Host:     s.sandboxStore().versionDir(e.ID, a.Version),
			Guest:    path.Join(filepath.ToSlash(s.GuestBase), e.ID),
			For:      "own",
			PerSpace: true,
			Env:      mountEnv(e.ID),
		})
	}
	return out
}

// writeMounts brings the bridge's descriptor up to date: written atomically
// and only when it changed, emptied (not removed: the desktop owns the file's
// folder) when no sandbox agent is registered.
func (s *Service) writeMounts() error {
	if s.MountsFile == "" {
		return nil
	}
	s.mu.Lock()
	doc := map[string]any{"contract": mountsContract, "mounts": s.mountsDoc()}
	s.mu.Unlock()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(s.MountsFile); err == nil && string(old) == string(raw) {
		return nil
	}
	return writeFileAtomic(s.MountsFile, raw, 0o644)
}

// heldInSandbox reports whether a live session runs the sandbox agent: the
// core publishes the launch line of each session, and this agent's line names
// its variable. Unknown (no word from the core) counts as held: removing the
// files of a conversation that cannot be seen loses it.
func (s *Service) heldInSandbox(e *Entry) bool {
	state, refs := s.readInUse()
	switch state {
	case inUseUnknown:
		return true
	case inUseGone:
		return false
	}
	prefix := "$" + mountEnv(e.ID) + "/"
	for _, r := range refs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// installInSandbox installs the newest allowed version of a sandbox agent and
// gives it to rooms.
func (s *Service) installInSandbox(ctx context.Context, op *operation, e *Entry) error {
	if e.Sandbox == nil {
		return fmt.Errorf("%s cannot be put in the sandbox", e.Name)
	}
	if cur, _ := s.managedVersion(e.ID); cur != "" {
		where := "on this computer"
		if s.placementOf(e.ID) == PlaceSandbox {
			where = "in your sandbox"
		}
		return fmt.Errorf("%s %s is already installed %s; remove it first to put it somewhere else", e.Name, cur, where)
	}
	v, err := s.targetIn(ctx, e, PlaceSandbox)
	if err != nil {
		return err
	}
	op.logf("putting %s %s in your sandbox", e.Name, v)
	if err := s.sandboxInstall(ctx, op, e, v); err != nil {
		return err
	}
	return s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous, a.Method, a.InstalledAt = v, "", "sandbox", s.now()
		a.Registered = true
		a.Latest, a.CheckErr = v, ""
	})
}

// updateInSandbox installs the new version beside the old one and only then
// points the rooms' mount at it; the old folder stays for a rollback. A room's
// tree is replaced when its next turn starts, so a conversation continues with
// the new version, which is why this is never automatic.
func (s *Service) updateInSandbox(ctx context.Context, op *operation, e *Entry, cur, v string) error {
	op.logf("updating %s %s to %s", e.Name, cur, v)
	if err := s.sandboxInstall(ctx, op, e, v); err != nil {
		return err
	}
	if err := s.mutate(func(st *State) {
		a := s.agentStateIn(st, e.ID)
		a.Version, a.Previous, a.InstalledAt = v, cur, s.now()
		a.Latest, a.CheckErr = v, ""
	}); err != nil {
		return err
	}
	s.sandboxStore().Prune(e.ID, v, cur)
	return nil
}

// sandboxOnly reports an agent the organization offers only in the sandbox.
func (s *Service) sandboxOnly(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings.SandboxOnly[id]
}
