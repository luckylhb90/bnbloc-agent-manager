package manager

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A Python agent is installed from wheels only, with the hash of every wheel
// checked, and no code of the package runs while it is installed: a wheel is
// unpacked, never built, and pip is told to refuse any source distribution. That
// is the guarantee `npm --ignore-scripts` gives for the npm agents, and it is why
// the same method serves the sandbox (sandbox.go): the wheels for the guest's
// platform are fetched and checked the same way and unpacked by this component,
// so nothing of the package runs on this computer for either placement.
//
// How a version is installed:
//
//  1. pip's own resolver, in a dry run with `--only-binary=:all:`, names the
//     closure (the package, its extras and every dependency) with each wheel's
//     URL and sha256 (`--report`). A dependency that has no wheel for the
//     platform fails the install here, before anything is downloaded.
//  2. The wheels are downloaded (pip, `--no-deps`, each pinned `name==version`
//     with `--hash`) into the version's own folder.
//  3. A virtual environment is made and the wheels are installed into it
//     offline (`--no-index --find-links`), `--require-hashes`, `--no-deps`.
//  4. The command must report the version asked for.
//
// The version folder appears only complete (a `.partial` sibling, renamed), as
// for npm.

// DefaultPyPI is the Python Package Index's JSON API; only https is accepted.
const DefaultPyPI = "https://pypi.org"

// maxPyPIDoc bounds an index answer.
const maxPyPIDoc = 32 << 20

// pypiDoc is the part of the index's answer used here.
type pypiDoc struct {
	Info struct {
		Version        string `json:"version"`
		RequiresPython string `json:"requires_python"`
	} `json:"info"`
	Releases map[string][]struct {
		PackageType string    `json:"packagetype"`
		UploadTime  time.Time `json:"upload_time_iso_8601"`
		Yanked      bool      `json:"yanked"`
	} `json:"releases"`
}

// latestFromPyPI reads the newest stable version of a package that has a wheel.
// With holdDays above 0 it is the newest one published at least that long ago,
// as for npm. A version with no wheel (an sdist only) is never offered: this
// component installs no source distribution.
func (s *Service) latestFromPyPI(ctx context.Context, pkg string, holdDays int) (string, error) {
	base := s.PyPIURL
	if base == "" {
		base = DefaultPyPI
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("the index %q is not an https address", base)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/pypi/"+url.PathEscape(pkg)+"/json", nil)
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
		return "", fmt.Errorf("the index did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("the package %s was not found in the index", pkg)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the index answered %d", resp.StatusCode)
	}
	var doc pypiDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPyPIDoc)).Decode(&doc); err != nil {
		return "", fmt.Errorf("the index's answer for %s could not be read: %w", pkg, err)
	}
	cutoff := s.now().Add(-time.Duration(holdDays) * 24 * time.Hour)
	best := ""
	for v, files := range doc.Releases {
		if !ValidVersion(v) {
			continue // a pre-release, a post-release or a local version
		}
		var wheel bool
		var oldest time.Time
		for _, f := range files {
			if f.Yanked || f.PackageType != "bdist_wheel" {
				continue
			}
			wheel = true
			if oldest.IsZero() || f.UploadTime.Before(oldest) {
				oldest = f.UploadTime
			}
		}
		if !wheel || (holdDays > 0 && oldest.After(cutoff)) {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no version of %s with a wheel was found in the index", pkg)
	}
	return best, nil
}

var pyVersionOut = regexp.MustCompile(`Python (3)\.(\d+)\.(\d+)`)

// pythonAt reports the version of an interpreter as major.minor, or "".
func pythonAt(ctx context.Context, path string) (major, minor int) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return 0, 0
	}
	m := pyVersionOut.FindStringSubmatch(string(out))
	if m == nil {
		return 0, 0
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor
}

// minorOf reads "3.13" as 13.
func minorOf(v string) int {
	_, after, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(after)
	return n
}

// findPython looks for an interpreter in the range on this computer's PATH
// (python3.13 down to the range's minimum, then python3 and python), the newest
// that fits. It also asks uv, which keeps interpreters outside the PATH.
func (s *Service) findPython(ctx context.Context, r *PythonRange) (string, error) {
	lo := minorOf(r.Min)
	hi := 99
	if r.Max != "" {
		hi = minorOf(r.Max)
	}
	var names []string
	for m := min(hi, 20); m >= lo; m-- {
		names = append(names, "python3."+strconv.Itoa(m))
	}
	names = append(names, "python3", "python")
	seen := map[string]bool{}
	var cands []string
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil && !seen[p] {
			seen[p] = true
			cands = append(cands, p)
		}
	}
	if uv, err := exec.LookPath("uv"); err == nil {
		for m := min(hi, 20); m >= lo; m-- {
			out, err := exec.CommandContext(ctx, uv, "python", "find", "3."+strconv.Itoa(m)).Output()
			if p := strings.TrimSpace(string(out)); err == nil && p != "" && !seen[p] {
				seen[p] = true
				cands = append(cands, p)
			}
		}
	}
	best, bestMinor := "", -1
	for _, p := range cands {
		major, minor := pythonAt(ctx, p)
		if major == 3 && minor >= lo && minor <= hi && minor > bestMinor {
			best, bestMinor = p, minor
		}
	}
	if best == "" {
		want := "Python " + r.Min
		if r.Max != "" {
			want += " to " + r.Max
		}
		return "", fmt.Errorf("%s is needed and was not found. Install it (uv python install %s, or your package manager), then try again", want, r.Min)
	}
	return best, nil
}

// pipReport is the part of pip's `--report` used here.
type pipReport struct {
	Install []struct {
		DownloadInfo struct {
			URL         string `json:"url"`
			ArchiveInfo struct {
				Hashes map[string]string `json:"hashes"`
			} `json:"archive_info"`
		} `json:"download_info"`
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"install"`
}

// pinnedWheel is one wheel of a version's closure.
type pinnedWheel struct {
	Name, Version, SHA256, URL string
}

// pipPlatform names the platform the wheels are for: nothing for this
// computer's own interpreter, or the guest's (sandbox.go).
type pipPlatform struct {
	// Platforms are pip's --platform values, Python the --python-version, ABIs the
	// --abi values; empty = this interpreter's own.
	Platforms []string
	Python    string
	ABIs      []string
}

func (p pipPlatform) args() []string {
	if len(p.Platforms) == 0 {
		return nil
	}
	var a []string
	for _, x := range p.Platforms {
		a = append(a, "--platform", x)
	}
	for _, x := range p.ABIs {
		a = append(a, "--abi", x)
	}
	return append(a, "--python-version", p.Python, "--implementation", "cp")
}

// pipEnv is the environment pip runs in: the member's, minus anything that
// changes where it looks or what it may build, and no user configuration.
//
// The index pip installs from is the Python Package Index, or the service's
// PyIndexURL (an organization's mirror; a test's local index), never a variable
// of the member's environment.
func (s *Service) pipEnv(cache string) []string {
	var out []string
	for _, kv := range s.commandEnv() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "PIP_") || name == "PYTHONPATH" || name == "PYTHONHOME" || name == "PYTHONSTARTUP" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "PIP_CONFIG_FILE=/dev/null", "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PIP_CACHE_DIR="+cache, "PYTHONNOUSERSITE=1")
	if s.PyIndexURL != "" {
		out = append(out, "PIP_INDEX_URL="+s.PyIndexURL)
	}
	return out
}

// resolveWheels names the closure of pkg[extras]==version as pinned wheels, by
// pip's resolver in a dry run that may use only wheels. Nothing is installed.
func (s *Service) resolveWheels(ctx context.Context, op *operation, py string, in *Install, version string, plat pipPlatform) ([]pinnedWheel, error) {
	dir, err := os.MkdirTemp("", "bnbloc-pip-resolve-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	report := filepath.Join(dir, "report.json")
	spec := in.Package
	if len(in.Extras) > 0 {
		spec += "[" + strings.Join(in.Extras, ",") + "]"
	}
	spec += "==" + version
	args := []string{"-m", "pip", "install", "--dry-run", "--ignore-installed", "--only-binary=:all:", "--quiet",
		"--report", report, "--target", filepath.Join(dir, "t")}
	args = append(args, plat.args()...)
	args = append(args, spec)
	op.logf("resolving %s (wheels only)", spec)
	if err := s.runEnv(ctx, op, s.pipEnv(s.Store.pipCacheDir()), py, args...); err != nil {
		return nil, fmt.Errorf("no set of wheels satisfies %s: %w", spec, err)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		return nil, fmt.Errorf("pip made no report: %w", err)
	}
	var rep pipReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("pip's report could not be read: %w", err)
	}
	var out []pinnedWheel
	for _, i := range rep.Install {
		sum := i.DownloadInfo.ArchiveInfo.Hashes["sha256"]
		if sum == "" || !strings.HasSuffix(strings.SplitN(i.DownloadInfo.URL, "?", 2)[0], ".whl") {
			return nil, fmt.Errorf("%s %s is not a wheel with a hash: this component installs only those", i.Metadata.Name, i.Metadata.Version)
		}
		out = append(out, pinnedWheel{Name: i.Metadata.Name, Version: i.Metadata.Version, SHA256: sum, URL: i.DownloadInfo.URL})
	}
	if len(out) == 0 {
		return nil, errors.New("pip named nothing to install")
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// lockFile renders the closure as a pip requirements file with every hash.
func lockFile(ws []pinnedWheel) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "%s==%s --hash=sha256:%s\n", w.Name, w.Version, w.SHA256)
	}
	return b.String()
}

// downloadWheels fetches every wheel of the closure into dir, checked against
// its hash by pip itself (--require-hashes).
func (s *Service) downloadWheels(ctx context.Context, op *operation, py, dir, lock string, plat pipPlatform) error {
	args := []string{"-m", "pip", "download", "--only-binary=:all:", "--no-deps", "--require-hashes", "--quiet", "--dest", dir, "-r", lock}
	args = append(args, plat.args()...)
	op.logf("downloading the wheels")
	return s.runEnv(ctx, op, s.pipEnv(s.Store.pipCacheDir()), py, args...)
}

// pipInstall installs pkg==version into a new version folder of the store: a
// virtual environment with the wheels installed into it offline and hash-checked.
//
// Unlike npm's, this folder is built where it stays. A virtual environment bakes
// its own path into the launchers of its commands (`#!<dir>/venv/bin/python`), so
// one built in a `.partial` folder and renamed would carry a command that runs
// nothing. The folder is therefore made at its final name, with a marker file
// inside (`.installing`) that is removed only when everything has been checked; a
// folder that has the marker is an unfinished install, never offered, and a failed
// install removes it (SweepPartials removes one a crash left).
func (s *Service) pipInstall(ctx context.Context, op *operation, e *Entry, version string) (string, error) {
	in := e.Install
	if !ValidVersion(version) {
		return "", fmt.Errorf("%q is not a version this component installs", version)
	}
	py, err := s.findPython(ctx, in.Python)
	if err != nil {
		return "", err
	}
	dir := s.Store.versionDir(e.ID, version)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	marker := filepath.Join(dir, installingMarker)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	wheels, err := s.resolveWheels(ctx, op, py, in, version, pipPlatform{})
	if err != nil {
		return "", err
	}
	wdir := filepath.Join(dir, "wheels")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		return "", err
	}
	lock := filepath.Join(dir, "requirements.lock")
	if err := os.WriteFile(lock, []byte(lockFile(wheels)), 0o600); err != nil {
		return "", err
	}
	if err := s.downloadWheels(ctx, op, py, wdir, lock, pipPlatform{}); err != nil {
		return "", err
	}
	venv := filepath.Join(dir, "venv")
	op.logf("making the environment")
	if err := s.runEnv(ctx, op, s.pipEnv(s.Store.pipCacheDir()), py, "-m", "venv", venv); err != nil {
		return "", err
	}
	vpy := venvPython(venv)
	op.logf("installing %d wheels (offline, hashes checked)", len(wheels))
	if err := s.runEnv(ctx, op, s.pipEnv(s.Store.pipCacheDir()), vpy, "-m", "pip", "install", "--no-index", "--find-links", wdir,
		"--only-binary=:all:", "--no-deps", "--require-hashes", "--quiet", "-r", lock); err != nil {
		return "", err
	}
	_ = os.RemoveAll(wdir) // the environment holds them now; the lock stays
	bin := venvBin(venv, in.Bin)
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("pip finished and %s is not in the environment: the catalog's command name may be wrong", in.Bin)
	}
	if len(e.Detect.Args) > 0 {
		if got := s.readVersion(ctx, e, bin); got != version {
			if got == "" {
				got = "none"
			}
			return "", fmt.Errorf("the installed command reports version %s, not %s; not registered", got, version)
		}
	}
	if err := os.Remove(marker); err != nil {
		return "", err
	}
	ok = true
	return dir, nil
}

// installingMarker is a file in a version folder that is still being made.
const installingMarker = ".installing"

func venvPython(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

// venvBin is a command of the environment.
func venvBin(venv, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", name+".exe")
	}
	return filepath.Join(venv, "bin", name)
}

// installedCommand is the command of a version folder, whichever method made it.
func installedCommand(versionDir string, in *Install) string {
	if in.Method == "pip" {
		return venvBin(filepath.Join(versionDir, "venv"), in.Bin)
	}
	return binPath(versionDir, in.Bin)
}

// ---- the sandbox's build -----------------------------------------------------

// guestPlatform is what the guest's wheels are chosen for: a Linux of this
// computer's architecture, glibc (the image is Debian), and the guest's own
// python3. The ABI tags are the interpreter's own, the stable ABI and none.
func guestPlatform(python string) pipPlatform {
	cpu := "aarch64"
	if runtime.GOARCH == "amd64" {
		cpu = "x86_64"
	}
	return pipPlatform{
		Platforms: []string{"manylinux_2_28_" + cpu, "manylinux_2_17_" + cpu, "manylinux2014_" + cpu, "linux_" + cpu, "any"},
		Python:    python,
		ABIs:      []string{"cp" + strings.ReplaceAll(python, ".", ""), "abi3", "none"},
	}
}

// sandboxPipInstall puts a Python agent in the sandbox's store: the wheels of the
// guest's platform, resolved and fetched with their hashes exactly as for this
// computer, unpacked here by this component into one folder of packages, and a
// launcher that runs the guest's python3 on them. Nothing of the package runs on
// this computer: a wheel is a zip, and unpacking it is what pip does for one.
//
// The folder is
//
//	<version>/site/        the packages of every wheel, flat (what site-packages is)
//	<version>/bin/<cmd>    the launcher (a shell script: the ACP core gives a guest
//	                       agent no environment, so the program sets its own)
//	<version>/requirements.lock
//
// A wheel's own scripts (console entry points) are not used: the launcher runs the
// entry point's module, read from the wheel's entry_points.txt.
func (s *Service) sandboxPipInstall(ctx context.Context, op *operation, e *Entry, version string) error {
	in := e.Install
	sb := e.Sandbox
	py, err := s.findPython(ctx, in.Python)
	if err != nil {
		return fmt.Errorf("%w (it is needed here to choose the wheels for the sandbox; nothing of the agent runs on this computer)", err)
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
	plat := guestPlatform(sb.Python)
	wheels, err := s.resolveWheels(ctx, op, py, in, version, plat)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "bnbloc-pip-guest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	lock := filepath.Join(partial, "requirements.lock")
	if err := os.WriteFile(lock, []byte(lockFile(wheels)), 0o644); err != nil {
		return err
	}
	if err := s.downloadWheels(ctx, op, py, work, lock, plat); err != nil {
		return err
	}
	site := filepath.Join(partial, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		return err
	}
	op.logf("unpacking %d wheels", len(wheels))
	files, err := unpackWheels(work, site, op.touch)
	if err != nil {
		return err
	}
	op.logf("%d files", files)
	module, call, err := consoleEntry(site, in.Package, sb.command(e))
	if err != nil {
		return err
	}
	if err := writeLauncher(filepath.Join(partial, "bin"), sb.command(e), sb.Python, module, call, sb.DataEnv); err != nil {
		return err
	}
	if err := checkInstalledDist(site, in.Package, version); err != nil {
		return err
	}
	n, err := makeReadableByGuest(partial)
	if err != nil {
		return err
	}
	if n > maxTarFiles*2 {
		return fmt.Errorf("the install holds %d files, more than this component accepts", n)
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

// ---- unpacking wheels --------------------------------------------------------

// unpackWheels extracts every wheel in dir into site, the way pip lays a pure or
// binary wheel out, and nothing more. A wheel is a zip; what is accepted is a
// regular file or folder whose path stays inside site, with the number and size
// bounded. Not used, by design: a `.data/scripts` entry (an install-time script
// that pip would rewrite and run through a launcher), a `.pth` file (it can run
// code at every interpreter start, which a file that only lists a folder need not),
// and a link. A wheel that has one is refused, not skipped: the install would be
// something else than the package says.
//
// The `.data/<scheme>/` folders of a wheel go where pip would put them. Only `data`
// is understood: it goes to the version folder's `data/` (pip puts it under the
// environment's prefix, which this layout has not), and the launcher names that
// folder to the programs that look for it (the package says how: Hermes reads
// HERMES_BUNDLED_LOCALES). `purelib` and `platlib` go to site; `scripts`, `headers`
// and anything else are refused.
func unpackWheels(dir, site string, touch func()) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var files int
	var total int64
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".whl") {
			continue
		}
		zr, err := zip.OpenReader(filepath.Join(dir, ent.Name()))
		if err != nil {
			return files, fmt.Errorf("%s is not a wheel: %w", ent.Name(), err)
		}
		for _, f := range zr.File {
			touch()
			name := path.Clean(f.Name)
			if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(f.Name, `\`) {
				zr.Close()
				return files, fmt.Errorf("%s holds an unsafe path %q", ent.Name(), f.Name)
			}
			dest := site
			rel := name
			if i := strings.Index(name, ".data/"); i > 0 && !strings.Contains(name[:i], "/") {
				scheme, rest, _ := strings.Cut(name[i+len(".data/"):], "/")
				switch scheme {
				case "purelib", "platlib":
					rel = rest
				case "data":
					dest, rel = filepath.Join(filepath.Dir(site), "data"), rest
				default:
					zr.Close()
					return files, fmt.Errorf("%s has a %q part (%s): this component installs no such part of a wheel", ent.Name(), scheme, name)
				}
			}
			if rel == "" || rel == "." {
				continue
			}
			if strings.HasSuffix(rel, ".pth") {
				zr.Close()
				return files, fmt.Errorf("%s has a .pth file (%s): it could run code at every start of the interpreter", ent.Name(), name)
			}
			mode := f.Mode()
			if mode&fs.ModeSymlink != 0 || mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
				zr.Close()
				return files, fmt.Errorf("%s holds a link or a special file (%s)", ent.Name(), name)
			}
			target := filepath.Join(dest, filepath.FromSlash(rel))
			if !strings.HasPrefix(target, filepath.Clean(dest)+string(filepath.Separator)) {
				zr.Close()
				return files, fmt.Errorf("%s holds a path outside the install (%s)", ent.Name(), name)
			}
			if f.FileInfo().IsDir() {
				if err := os.MkdirAll(target, 0o755); err != nil {
					zr.Close()
					return files, err
				}
				continue
			}
			if files++; files > maxTarFiles {
				zr.Close()
				return files, errors.New("the wheels hold more files than this component accepts")
			}
			if total += int64(f.UncompressedSize64); total > maxTarBytes {
				zr.Close()
				return files, errors.New("the wheels are larger than this component accepts")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				zr.Close()
				return files, err
			}
			if err := extractZipFile(f, target); err != nil {
				zr.Close()
				return files, err
			}
		}
		zr.Close()
	}
	return files, nil
}

func extractZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	// Mode: readable by all, runnable only where the wheel says so (a shared library
	// or a script a package ships in its own folder).
	mode := fs.FileMode(0o644)
	if f.Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)+1)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// distInfoName is the folder name pip gives a distribution's metadata: the name
// with runs of `-_.` made one `_`, then `-<version>.dist-info`.
var distSep = regexp.MustCompile(`[-_.]+`)

func distInfoDir(name, version string) string {
	return distSep.ReplaceAllString(name, "_") + "-" + version + ".dist-info"
}

// checkInstalledDist confirms site holds the package's metadata at the version
// asked for (nothing can be run to report it: it is the guest's platform's).
func checkInstalledDist(site, pkg, version string) error {
	entries, _ := os.ReadDir(site)
	want := strings.ToLower(distInfoDir(pkg, version))
	for _, e := range entries {
		if strings.ToLower(e.Name()) == want {
			return nil
		}
	}
	return fmt.Errorf("the unpacked wheels do not hold %s %s", pkg, version)
}

// consoleEntry reads the package's entry_points.txt and returns the module and
// function the console command runs ("hermes_cli.main", "main").
func consoleEntry(site, pkg, command string) (module, call string, err error) {
	entries, _ := os.ReadDir(site)
	var dir string
	prefix := strings.ToLower(distSep.ReplaceAllString(pkg, "_")) + "-"
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".dist-info") && strings.HasPrefix(n, prefix) {
			dir = filepath.Join(site, e.Name())
		}
	}
	if dir == "" {
		return "", "", fmt.Errorf("the unpacked wheels have no metadata for %s", pkg)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "entry_points.txt"))
	if err != nil {
		return "", "", fmt.Errorf("%s declares no commands: %w", pkg, err)
	}
	inScripts := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inScripts = line == "[console_scripts]"
			continue
		}
		if !inScripts || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, target, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != command {
			continue
		}
		target = strings.TrimSpace(target)
		if i := strings.Index(target, "["); i >= 0 { // extras of the entry point
			target = strings.TrimSpace(target[:i])
		}
		mod, fn, ok := strings.Cut(target, ":")
		if !ok || !pyModuleRE.MatchString(mod) || !pyIdentRE.MatchString(fn) {
			return "", "", fmt.Errorf("the command %s has an entry point %q this component cannot launch", command, target)
		}
		return mod, fn, nil
	}
	return "", "", fmt.Errorf("%s declares no command named %s", pkg, command)
}

var (
	pyModuleRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	pyIdentRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// writeLauncher writes <dir>/<command>: the program the ACP core starts in the
// guest. It sets what the package needs (the packages' folder on the path, and the
// bundled data folder for the variables the catalog names), picks the interpreter
// the wheels were chosen for and runs the entry point with the arguments it was
// given. The values written are the folder's own location (from the launcher's
// own path) and names that passed the checks above, never text from a package.
//
// dataEnv maps a variable to a path under the version folder's data/ ("" = none):
// `HERMES_BUNDLED_LOCALES` -> `locales`.
func writeLauncher(dir, command, python, module, call string, dataEnv map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(dataEnv))
	for k := range dataEnv {
		names = append(names, k)
	}
	sort.Strings(names)
	var data strings.Builder
	for _, k := range names {
		fmt.Fprintf(&data, "%s=\"$here/data/%s\"; export %s\n", k, dataEnv[k], k)
	}
	script := "#!/bin/sh\n" +
		"# Starts " + command + " on the guest's python " + python + " from the packages beside this folder.\n" +
		"here=$(cd \"$(dirname \"$0\")/..\" && pwd)\n" +
		"PYTHONPATH=\"$here/site${PYTHONPATH:+:$PYTHONPATH}\"\n" +
		"export PYTHONPATH PYTHONDONTWRITEBYTECODE=1 PYTHONNOUSERSITE=1\n" +
		data.String() +
		"exec python" + python + " -c 'import sys; from " + module + " import " + call + " as _m; sys.argv[0] = \"" + command + "\"; sys.exit(_m())' \"$@\"\n"
	return os.WriteFile(filepath.Join(dir, command), []byte(script), 0o755)
}
