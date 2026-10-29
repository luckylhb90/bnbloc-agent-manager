package manager

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- synthetic wheels ----------------------------------------------------------

// wheelSpec is a wheel built in memory: a zip with the layout pip reads.
type wheelSpec struct {
	name, version string
	files         map[string]string // path in the wheel -> content
	entry         string            // entry_points.txt
	requires      []string
}

func (w wheelSpec) filename() string { return w.name + "-" + w.version + "-py3-none-any.whl" }

// built caches the bytes of each wheel: a zip carries its entries in map order, so
// building one twice gives two different files, and an index must serve the file
// whose hash it announced.
var built sync.Map

func (w wheelSpec) bytes(t *testing.T) []byte {
	t.Helper()
	key := w.filename() + "|" + w.entry + "|" + strings.Join(w.requires, ",")
	names := make([]string, 0, len(w.files))
	for k := range w.files {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		sum := sha256.Sum256([]byte(w.files[k]))
		key += "|" + k + ":" + hex.EncodeToString(sum[:4])
	}
	if b, ok := built.Load(key); ok {
		return b.([]byte)
	}
	b := w.build(t)
	built.Store(key, b)
	return b
}

func (w wheelSpec) build(t *testing.T) []byte {
	t.Helper()
	di := w.name + "-" + w.version + ".dist-info"
	all := map[string]string{}
	for k, v := range w.files {
		all[k] = v
	}
	meta := "Metadata-Version: 2.1\nName: " + w.name + "\nVersion: " + w.version + "\n"
	for _, r := range w.requires {
		meta += "Requires-Dist: " + r + "\n"
	}
	all[di+"/METADATA"] = meta
	all[di+"/WHEEL"] = "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
	if w.entry != "" {
		all[di+"/entry_points.txt"] = w.entry
	}
	var rec strings.Builder
	for k, v := range all {
		sum := sha256.Sum256([]byte(v))
		fmt.Fprintf(&rec, "%s,sha256=%s,%d\n", k, strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "="), len(v))
	}
	all[di+"/RECORD"] = rec.String() + di + "/RECORD,,\n"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for k, v := range all {
		f, err := zw.Create(k)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(v))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	depWheel = wheelSpec{name: "fakedep", version: "1.0", files: map[string]string{"fakedep/__init__.py": "X = 1\n"}}
	appWheel = wheelSpec{
		name: "fakepy", version: "2.0.0", requires: []string{"fakedep>=1.0"},
		files: map[string]string{
			"fakepy/__init__.py": "",
			"fakepy/cli.py": "import sys\nfrom fakedep import X\n" +
				"def main():\n    print('fakepy 2.0.0', X, sys.argv[1:])\n    return 0\n",
			"fakepy-2.0.0.data/data/locales/en.yaml": "hello: hi\n",
		},
		entry: "[console_scripts]\nfakepy = fakepy.cli:main\n",
	}
)

// pyIndex serves wheels as a PEP 503 simple index on a loopback address.
func pyIndex(t *testing.T, ws ...wheelSpec) *httptest.Server {
	t.Helper()
	byName := map[string][]wheelSpec{}
	for _, w := range ws {
		byName[w.name] = append(byName[w.name], w)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")
		switch {
		case path == "":
			w.Header().Set("Content-Type", "text/html")
			for n := range byName {
				fmt.Fprintf(w, `<a href="/%s/">%s</a>`, n, n)
			}
		case len(parts) == 1:
			list, ok := byName[parts[0]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			for _, s := range list {
				sum := sha256.Sum256(s.bytes(t))
				fmt.Fprintf(w, `<a href="/%s/%s#sha256=%s">%s</a>`, s.name, s.filename(), hex.EncodeToString(sum[:]), s.filename())
			}
		case len(parts) == 2:
			for _, s := range byName[parts[0]] {
				if s.filename() == parts[1] {
					_, _ = w.Write(s.bytes(t))
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func needPython(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the checks run shell and python on a unix path")
	}
	svc := &Service{}
	if _, err := svc.findPython(context.Background(), &PythonRange{Min: "3.9"}); err != nil {
		t.Skip("no Python 3.9 or newer on this computer: " + err.Error())
	}
}

const pipCatalog = `{"version":"t","agents":[
 {"id":"fakepy","name":"Fake Py","install":{"method":"pip","package":"fakepy","bin":"fakepy","python":{"min":"3.9"}},
  "latest":{"from":"pypi"},"detect":{"command":"fakepy-not-on-path","args":["--version"],"parse":"fakepy (\\d+\\.\\d+\\.\\d+)"},
  "register":{"name":"fakepy","args":["acp"]}}]}`

func pipService(t *testing.T, idx *httptest.Server) *Service {
	t.Helper()
	svc := newService(t, pipCatalog, nil)
	svc.PyIndexURL = idx.URL + "/"
	return svc
}

// ---- the host install ----------------------------------------------------------

// A package is installed from wheels with their hashes checked, into a virtual
// environment built where it stays, and the command must report the version asked
// for before anything may point at it.
func TestPipInstallBuildsAnEnvironmentFromHashedWheels(t *testing.T) {
	needPython(t)
	idx := pyIndex(t, depWheel, appWheel)
	svc := pipService(t, idx)
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	dir, err := svc.pipInstall(context.Background(), op, e, "2.0.0")
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(op.log, "\n"))
	}
	if dir != svc.Store.versionDir("fakepy", "2.0.0") {
		t.Fatalf("installed at %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, installingMarker)); err == nil {
		t.Fatal("the folder still says it is being made")
	}
	cmd := installedCommand(dir, e.Install)
	out, err := exec.Command(cmd, "x").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "fakepy 2.0.0 1 ['x']") {
		t.Fatalf("the installed command: %v %q (the dependency must have come along)", err, out)
	}
	lock, _ := os.ReadFile(filepath.Join(dir, "requirements.lock"))
	for _, want := range []string{"fakedep==1.0 --hash=sha256:", "fakepy==2.0.0 --hash=sha256:"} {
		if !strings.Contains(string(lock), want) {
			t.Errorf("the lock lacks %q:\n%s", want, lock)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "wheels")); err == nil {
		t.Error("the downloaded wheels were left beside the environment that holds them")
	}
	// The log says the install used only wheels and checked hashes.
	log := strings.Join(op.log, "\n")
	for _, want := range []string{"--only-binary=:all:", "--require-hashes", "--no-index"} {
		if !strings.Contains(log, want) {
			t.Errorf("the pip commands lack %s:\n%s", want, log)
		}
	}
}

// The environment is built at its final path: a virtual environment writes its own
// path into its commands, so one made elsewhere and renamed would run nothing.
func TestPipInstalledCommandSurvivesTheStoreBeingRead(t *testing.T) {
	needPython(t)
	svc := pipService(t, pyIndex(t, depWheel, appWheel))
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	dir, err := svc.pipInstall(context.Background(), op, e, "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(installedCommand(dir, e.Install))
	if first := strings.SplitN(string(raw), "\n", 2)[0]; !strings.Contains(first, dir) || strings.Contains(first, partialSuffix) {
		t.Fatalf("the command's interpreter line is %q, not a path in %s", first, dir)
	}
}

// A package with a dependency that has no wheel (a source distribution only) is not
// installed at all: nothing is built, and nothing is left behind.
func TestPipInstallRefusesWhatHasNoWheelAndLeavesNothing(t *testing.T) {
	needPython(t)
	idx := pyIndex(t, appWheel) // fakedep has no wheel on this index
	svc := pipService(t, idx)
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	_, err := svc.pipInstall(context.Background(), op, e, "2.0.0")
	if err == nil {
		t.Fatal("installed a package whose dependency has no wheel")
	}
	if _, serr := os.Stat(svc.Store.versionDir("fakepy", "2.0.0")); serr == nil {
		t.Fatal("a failed install left its folder")
	}
}

// A wheel whose bytes are not the index's hash is refused by pip's own check.
func TestPipInstallRefusesAWheelThatIsNotWhatTheIndexSaid(t *testing.T) {
	needPython(t)
	good := pyIndex(t, depWheel, appWheel)
	// An index that names the right hash and serves other bytes for the dependency.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, depWheel.filename()) {
			_, _ = w.Write(append(depWheel.bytes(t), []byte("tampered")...))
			return
		}
		resp, err := http.Get(good.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	svc := newService(t, pipCatalog, nil)
	svc.PyIndexURL = srv.URL + "/"
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	if _, err := svc.pipInstall(context.Background(), op, e, "2.0.0"); err == nil {
		t.Fatal("installed a wheel that does not match its hash")
	}
	if _, serr := os.Stat(svc.Store.versionDir("fakepy", "2.0.0")); serr == nil {
		t.Fatal("a failed install left its folder")
	}
}

func TestPipInstallNeedsAPythonInRangeAndSaysWhat(t *testing.T) {
	svc := newService(t, strings.Replace(pipCatalog, `"min":"3.9"`, `"min":"3.99"`, 1), nil)
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	_, err := svc.pipInstall(context.Background(), op, e, "2.0.0")
	if err == nil || !strings.Contains(err.Error(), "Python 3.99") {
		t.Fatalf("err = %v, want it to name the Python needed", err)
	}
}

// pip runs without the member's own configuration or index: a PIP_* variable, a
// pip.conf or a PYTHONPATH of theirs does not change what is installed.
func TestPipEnvironmentIsNotTheMembers(t *testing.T) {
	t.Setenv("PIP_INDEX_URL", "https://evil.example/simple")
	t.Setenv("PIP_EXTRA_INDEX_URL", "https://evil.example/simple")
	t.Setenv("PYTHONPATH", "/somewhere")
	svc := newService(t, pipCatalog, nil)
	svc.PyIndexURL = "http://127.0.0.1:1/"
	env := strings.Join(svc.pipEnv("/cache"), "\n")
	for _, bad := range []string{"evil.example", "PIP_EXTRA_INDEX_URL", "PYTHONPATH=/somewhere"} {
		if strings.Contains(env, bad) {
			t.Errorf("the member's %q reaches pip", bad)
		}
	}
	for _, want := range []string{"PIP_CONFIG_FILE=/dev/null", "PIP_INDEX_URL=http://127.0.0.1:1/", "PIP_CACHE_DIR=/cache", "PYTHONNOUSERSITE=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("pip lacks %q", want)
		}
	}
}

// An update installs the new version beside the old one; the registration follows;
// the old one stays for a rollback. The whole path, with a real Python.
func TestPipAgentUpdatesAndRollsBack(t *testing.T) {
	needPython(t)
	v1 := wheelSpec{name: "fakepy", version: "1.0.0", requires: []string{"fakedep>=1.0"}, files: map[string]string{
		"fakepy/__init__.py": "", "fakepy/cli.py": "def main():\n    print('fakepy 1.0.0')\n",
	}, entry: "[console_scripts]\nfakepy = fakepy.cli:main\n"}
	svc := pipService(t, pyIndex(t, depWheel, v1, appWheel))
	svc.cat().Agents[0].Detect.Command = "fakepy" // the version is read from the managed command
	e, _ := svc.cat().Entry("fakepy")
	op := newOperation("install", time.Minute, func() {}, time.Now())
	if _, err := svc.pipInstall(context.Background(), op, e, "1.0.0"); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(op.log, "\n"))
	}
	if _, err := svc.pipInstall(context.Background(), op, e, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		out, err := exec.Command(installedCommand(svc.Store.versionDir("fakepy", v), e.Install)).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "fakepy "+v) {
			t.Fatalf("%s: %v %q", v, err, out)
		}
	}
}

// ---- the newest version, from the index -----------------------------------------

func TestLatestFromPyPIReadsTheNewestVersionThatHasAWheel(t *testing.T) {
	old := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/some-agent/json" {
			http.NotFound(w, r)
			return
		}
		file := func(kind, at string) map[string]any {
			return map[string]any{"packagetype": kind, "upload_time_iso_8601": at, "yanked": false}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"releases": map[string]any{
			"1.0.0":    []any{file("bdist_wheel", old)},
			"1.1.0":    []any{file("bdist_wheel", recent)},
			"1.2.0":    []any{file("sdist", old)},       // source only: never offered
			"1.3.0rc1": []any{file("bdist_wheel", old)}, // a pre-release
			"1.4.0":    []any{file("bdist_wheel", old)},
		}})
	}))
	t.Cleanup(srv.Close)
	svc := &Service{PyPIURL: srv.URL, HTTP: srv.Client()}
	got, err := svc.latestFromPyPI(context.Background(), "some-agent", 0)
	if err != nil || got != "1.4.0" {
		t.Fatalf("latest = %q %v, want 1.4.0 (newest stable with a wheel)", got, err)
	}
	// Hold: a version younger than the hold is not offered yet.
	if got, err := svc.latestFromPyPI(context.Background(), "some-agent", 7); err != nil || got != "1.4.0" {
		t.Fatalf("held latest = %q %v", got, err)
	}
	if _, err := svc.latestFromPyPI(context.Background(), "nothing", 0); err == nil {
		t.Fatal("an unknown package was found")
	}
	svc.PyPIURL = "http://plain.example"
	if _, err := svc.latestFromPyPI(context.Background(), "some-agent", 0); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain http index: %v", err)
	}
}

// ---- the sandbox's build --------------------------------------------------------

func writeWheels(t *testing.T, dir string, ws ...wheelSpec) {
	t.Helper()
	for _, w := range ws {
		if err := os.WriteFile(filepath.Join(dir, w.filename()), w.bytes(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnpackWheelsLaysOutPackagesAndDataAndNothingElse(t *testing.T) {
	dir, site := t.TempDir(), filepath.Join(t.TempDir(), "v", "site")
	writeWheels(t, dir, depWheel, appWheel)
	n, err := unpackWheels(dir, site, func() {})
	if err != nil || n < 6 {
		t.Fatalf("unpacked %d files: %v", n, err)
	}
	for path, want := range map[string]string{
		filepath.Join(site, "fakepy", "cli.py"):                         "def main",
		filepath.Join(site, "fakedep", "__init__.py"):                   "X = 1",
		filepath.Join(filepath.Dir(site), "data", "locales", "en.yaml"): "hello",
	} {
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), want) {
			t.Errorf("%s: %v %q", path, err, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(site, "fakepy-2.0.0.data")); err == nil {
		t.Error("the wheel's .data folder was left in the packages")
	}
}

func TestUnpackWheelsRefusesWhatCouldRunCodeOrEscape(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a .pth file":              {"evil.pth": "import os; os.system('x')\n"},
		"a scripts part":           {"bad-1.0.data/scripts/run": "#!/bin/sh\n"},
		"a headers part":           {"bad-1.0.data/headers/h.h": "x"},
		"a parent path":            {"../escape.py": "x"},
		"an absolute path":         {"/etc/passwd": "x"},
		"a backslash path":         {`..\escape.py`: "x"},
		"a pth inside a data part": {"bad-1.0.data/purelib/x.pth": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			w := wheelSpec{name: "bad", version: "1.0", files: files}
			writeWheels(t, dir, w)
			site := filepath.Join(t.TempDir(), "v", "site")
			if _, err := unpackWheels(dir, site, func() {}); err == nil {
				t.Fatalf("unpacked a wheel with %s", name)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(site), "escape.py")); err == nil {
				t.Fatal("a file escaped")
			}
		})
	}
}

func TestUnpackWheelsRefusesALink(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "bad/link"}
	h.SetMode(os.ModeSymlink | 0o777)
	f, _ := zw.CreateHeader(h)
	_, _ = f.Write([]byte("/etc/passwd"))
	_ = zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "bad-1.0-py3-none-any.whl"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := unpackWheels(dir, filepath.Join(t.TempDir(), "v", "site"), func() {}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("err = %v, want a refusal of the link", err)
	}
}

func TestConsoleEntryReadsTheCommandsModuleAndFunction(t *testing.T) {
	dir, site := t.TempDir(), filepath.Join(t.TempDir(), "v", "site")
	writeWheels(t, dir, appWheel)
	if _, err := unpackWheels(dir, site, func() {}); err != nil {
		t.Fatal(err)
	}
	mod, fn, err := consoleEntry(site, "fakepy", "fakepy")
	if err != nil || mod != "fakepy.cli" || fn != "main" {
		t.Fatalf("entry = %q %q %v", mod, fn, err)
	}
	if _, _, err := consoleEntry(site, "fakepy", "other"); err == nil {
		t.Error("a command the package does not declare was found")
	}
	if _, _, err := consoleEntry(site, "nothere", "x"); err == nil {
		t.Error("a package that is not there was found")
	}
	if err := checkInstalledDist(site, "fakepy", "2.0.0"); err != nil {
		t.Error(err)
	}
	if err := checkInstalledDist(site, "fakepy", "9.9.9"); err == nil {
		t.Error("a version that is not there was accepted")
	}
	// An entry point with a module or function that is not an identifier is refused,
	// since the launcher would put it into a command.
	for name, entry := range map[string]string{
		"a shell metacharacter": "[console_scripts]\nfakepy = fakepy;rm -rf /:main\n",
		"a path":                "[console_scripts]\nfakepy = ../x:main\n",
		"no function":           "[console_scripts]\nfakepy = fakepy.cli\n",
		"a call":                "[console_scripts]\nfakepy = fakepy.cli:main()\n",
	} {
		w := appWheel
		w.entry = entry
		d, s := t.TempDir(), filepath.Join(t.TempDir(), "v", "site")
		writeWheels(t, d, w)
		if _, err := unpackWheels(d, s, func() {}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := consoleEntry(s, "fakepy", "fakepy"); err == nil {
			t.Errorf("%s: an entry point the launcher cannot take safely was accepted", name)
		}
	}
	// extras on an entry point are dropped, not passed on
	w := appWheel
	w.entry = "[console_scripts]\nfakepy = fakepy.cli:main [extra]\n"
	d, s := t.TempDir(), filepath.Join(t.TempDir(), "v", "site")
	writeWheels(t, d, w)
	if _, err := unpackWheels(d, s, func() {}); err != nil {
		t.Fatal(err)
	}
	if mod, fn, err := consoleEntry(s, "fakepy", "fakepy"); err != nil || mod != "fakepy.cli" || fn != "main" {
		t.Errorf("extras: %q %q %v", mod, fn, err)
	}
}

// The launcher sets the path and the data variables and runs the entry point on the
// interpreter named; run here with this computer's python, it is the guest's launch
// in everything but the interpreter.
func TestLauncherRunsTheEntryPointFromTheUnpackedPackages(t *testing.T) {
	needPython(t)
	root := t.TempDir()
	site := filepath.Join(root, "site")
	writeWheels(t, t.TempDir())
	dir := t.TempDir()
	writeWheels(t, dir, depWheel, appWheel)
	if _, err := unpackWheels(dir, site, func() {}); err != nil {
		t.Fatal(err)
	}
	py := "3." + fmt.Sprint(pythonMinorHere(t))
	if err := writeLauncher(filepath.Join(root, "bin"), "fakepy", py, "fakepy.cli", "main", map[string]string{"FAKE_LOCALES": "locales"}); err != nil {
		t.Fatal(err)
	}
	script, _ := os.ReadFile(filepath.Join(root, "bin", "fakepy"))
	if !strings.Contains(string(script), `FAKE_LOCALES="$here/data/locales"`) || strings.Contains(string(script), "%") {
		t.Fatalf("launcher:\n%s", script)
	}
	if fi, _ := os.Stat(filepath.Join(root, "bin", "fakepy")); fi.Mode().Perm()&0o111 == 0 {
		t.Fatal("the launcher cannot run")
	}
	// Run from another directory with a hostile environment: the launcher is the
	// folder's own, wherever it is called from, and the member's PYTHONPATH is last.
	cmd := exec.Command(filepath.Join(root, "bin", "fakepy"), "a b", "c")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "fakepy 2.0.0 1 ['a b', 'c']") {
		t.Fatalf("%v %q", err, out)
	}
}

func pythonMinorHere(t *testing.T) int {
	t.Helper()
	svc := &Service{}
	p, err := svc.findPython(context.Background(), &PythonRange{Min: "3.9"})
	if err != nil {
		t.Skip(err)
	}
	_, minor := pythonAt(context.Background(), p)
	if _, err := exec.LookPath("python3." + fmt.Sprint(minor)); err != nil {
		t.Skip("the launcher names python3.<minor>, which is not on this PATH")
	}
	return minor
}

func TestGuestPlatformNamesLinuxGlibcAndTheGuestsPython(t *testing.T) {
	p := guestPlatform("3.13")
	args := strings.Join(p.args(), " ")
	for _, want := range []string{"--platform manylinux_2_28_", "--platform any", "--abi cp313", "--abi abi3", "--abi none", "--python-version 3.13", "--implementation cp"} {
		if !strings.Contains(args, want) {
			t.Errorf("platform args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "macosx") || strings.Contains(args, "win_") {
		t.Errorf("a platform that is not the guest's: %s", args)
	}
	if got := (pipPlatform{}).args(); len(got) != 0 {
		t.Errorf("this computer's own platform has args: %v", got)
	}
}

// ---- the catalog ----------------------------------------------------------------

func TestCatalogValidatesThePipMethod(t *testing.T) {
	ok := func(install string, extra string) string {
		return `{"version":"x","agents":[{"id":"a","name":"A","install":` + install + `,"detect":{"command":"a"}` + extra + `}]}`
	}
	good := `{"method":"pip","package":"a-pkg","bin":"a","python":{"min":"3.11","max":"3.13"}}`
	if _, err := ParseCatalog([]byte(ok(good, ""))); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]string{
		"no python range":                                 ok(`{"method":"pip","package":"a","bin":"a"}`, ""),
		"a bad python version":                            ok(`{"method":"pip","package":"a","bin":"a","python":{"min":"3"}}`, ""),
		"a bad package":                                   ok(`{"method":"pip","package":"a b","bin":"a","python":{"min":"3.9"}}`, ""),
		"a bad extra":                                     ok(`{"method":"pip","package":"a","extras":["x y"],"bin":"a","python":{"min":"3.9"}}`, ""),
		"pip fields on npm":                               ok(`{"method":"npm","package":"a","bin":"a","extras":["x"]}`, ""),
		"an unknown method":                               ok(`{"method":"conda","package":"a","bin":"a"}`, ""),
		"an unknown source":                               ok(good, `,"latest":{"from":"github"}`),
		"a pip sandbox without a pip install":             `{"version":"x","agents":[{"id":"a","name":"A","install":{"method":"npm","package":"a","bin":"a"},"detect":{"command":"a"},"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.13"}}]}`,
		"a sandbox python outside the range":              ok(good, `,"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.14"}`),
		"a sandbox package":                               ok(good, `,"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.13","package":"x"}`),
		"a data variable that changes how a program runs": ok(good, `,"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.13","dataEnv":{"PYTHONPATH":"x"}}`),
		"a data folder that escapes":                      ok(good, `,"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.13","dataEnv":{"A_DIR":"../x"}}`),
	} {
		if _, err := ParseCatalog([]byte(c)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseCatalog([]byte(ok(good, `,"register":{"name":"a"},"sandbox":{"method":"pip","python":"3.13","dataEnv":{"A_LOCALES":"locales"}}`))); err != nil {
		t.Errorf("a valid pip sandbox build: %v", err)
	}
}

// The shipped catalog's Hermes entry: a managed pip install with the acp extra, and
// a sandbox build of the same package, no installer script.
func TestShippedCatalogInstallsHermesFromWheels(t *testing.T) {
	cat, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cat.Entry("hermes")
	if !ok {
		t.Fatal("no hermes")
	}
	if e.Install == nil || e.Install.Method != "pip" || e.Install.Package != "hermes-agent" || strings.Join(e.Install.Extras, ",") != "acp,anthropic,mcp" {
		// acp: its ACP server. anthropic: a model served over the Anthropic protocol (the company
		// route serves Claude models that way) fails at its first turn without it: "The 'anthropic'
		// package is required for the Anthropic provider" (seen 2026-10-29, a room's first turn).
		// mcp: without it Hermes's MCP client is a no-op that logs at debug level, so the room's
		// tools and every host-run tool silently never reach the agent (a real guest run: the host
		// tool received initialize from the bridge and nothing from Hermes).
		t.Fatalf("install = %+v", e.Install)
	}
	if e.InstallHint != "" {
		t.Errorf("the entry still tells the member to run an installer script: %q", e.InstallHint)
	}
	if e.Sandbox == nil || e.Sandbox.Unavailable != "" || e.Sandbox.method() != sandboxPip || e.sandboxRefusal() != "" {
		t.Fatalf("sandbox = %+v (%q)", e.Sandbox, e.sandboxRefusal())
	}
	if guestCommand(e) != "$BNBLOC_MOUNT_AGENT_HERMES/bin/hermes" {
		t.Errorf("guest command = %q", guestCommand(e))
	}
	if e.latestFrom(PlaceHost) != "pypi" || e.latestFrom(PlaceSandbox) != "pypi" {
		t.Error("the newest Hermes is not read from the index")
	}
	if e.Sandbox.DataEnv["HERMES_BUNDLED_LOCALES"] != "locales" {
		t.Errorf("dataEnv = %v: Hermes would look for its locales where pip puts them, which this layout has not", e.Sandbox.DataEnv)
	}
}

// ---- the whole path, through the service's actions ----------------------------------

// pypiAPI is the index's JSON API, as the manager reads the newest version from it.
func pypiAPI(t *testing.T, versions ...string) *httptest.Server {
	t.Helper()
	old := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/fakepy/json" {
			http.NotFound(w, r)
			return
		}
		rel := map[string]any{}
		for _, v := range versions {
			rel[v] = []any{map[string]any{"packagetype": "bdist_wheel", "upload_time_iso_8601": old}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"releases": rel})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPipAgentLifecycleThroughTheServicesActions(t *testing.T) {
	needPython(t)
	v1 := wheelSpec{name: "fakepy", version: "1.0.0", requires: []string{"fakedep>=1.0"}, files: map[string]string{
		"fakepy/__init__.py": "", "fakepy/cli.py": "def main():\n    print('fakepy 1.0.0')\n",
	}, entry: "[console_scripts]\nfakepy = fakepy.cli:main\n"}
	idx := pyIndex(t, depWheel, v1, appWheel)
	api := pypiAPI(t, "1.0.0")
	svc := newService(t, strings.Replace(pipCatalog, `"command":"fakepy-not-on-path"`, `"command":"fakepy-not-on-path"`, 1), api)
	svc.PyPIURL, svc.HTTP, svc.PyIndexURL = api.URL, api.Client(), idx.URL+"/"
	// the manager reads the version from the command it installed
	svc.cat().Agents[0].Detect.Args = []string{"--version"}
	svc.cat().Agents[0].Detect.Parse = ""

	if err := do(t, svc, "fakepy", ActInstall); err != nil {
		t.Fatal(err)
	}
	v := view(svc, "fakepy")
	if v.Managed == nil || v.Managed.Version != "1.0.0" || !v.Registered {
		t.Fatalf("after install: %+v", v)
	}
	d, ok := readDropIn(t, svc)
	want := installedCommand(svc.Store.versionDir("fakepy", "1.0.0"), svc.cat().Agents[0].Install)
	if !ok || len(d.Agents) != 1 || d.Agents[0].Command != want || !strings.HasSuffix(want, filepath.Join("venv", "bin", "fakepy")) && runtime.GOOS != "windows" {
		t.Fatalf("registration: %+v, want the command %s", d, want)
	}
	if got := view(svc, "fakepy"); got.Managed == nil || got.Managed.SizeBytes <= 0 {
		t.Errorf("the size of an environment is shown: %+v", got.Managed)
	}

	// A new version arrives on the index.
	api2 := pypiAPI(t, "1.0.0", "2.0.0")
	svc.PyPIURL, svc.HTTP = api2.URL, api2.Client()
	if err := do(t, svc, "fakepy", ActCheck); err != nil {
		t.Fatal(err)
	}
	if v = view(svc, "fakepy"); !v.UpdateAvailable || v.Target != "2.0.0" || !v.Can.Update {
		t.Fatalf("update available: %+v", v)
	}
	if err := do(t, svc, "fakepy", ActUpdate); err != nil {
		t.Fatal(err)
	}
	if v = view(svc, "fakepy"); v.Managed.Version != "2.0.0" || v.Managed.Previous != "1.0.0" || !v.Can.Rollback {
		t.Fatalf("after update: %+v", v.Managed)
	}
	for _, ver := range []string{"1.0.0", "2.0.0"} {
		out, err := exec.Command(installedCommand(svc.Store.versionDir("fakepy", ver), svc.cat().Agents[0].Install)).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "fakepy "+ver) {
			t.Errorf("version %s must stay runnable: %v %q", ver, err, out)
		}
	}
	if d, _ = readDropIn(t, svc); !strings.Contains(d.Agents[0].Command, "2.0.0") {
		t.Errorf("the registration moved: %q", d.Agents[0].Command)
	}
	if err := do(t, svc, "fakepy", ActRollback); err != nil {
		t.Fatal(err)
	}
	if d, _ = readDropIn(t, svc); !strings.Contains(d.Agents[0].Command, "1.0.0") {
		t.Errorf("rollback moves the registration back: %q", d.Agents[0].Command)
	}
	if err := do(t, svc, "fakepy", ActRemove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.Store.agentDir("fakepy")); !os.IsNotExist(err) {
		t.Error("remove deletes every version")
	}
}

// A crash in the middle of an install leaves a folder that says so; the next start
// removes it, and it is never taken for an installed version.
func TestAnUnfinishedPipInstallIsSweptAtStart(t *testing.T) {
	svc := newService(t, pipCatalog, nil)
	dir := svc.Store.versionDir("fakepy", "2.0.0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, installingMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := svc.Store.versionDir("fakepy", "1.0.0") // a finished one, no marker
	if err := os.MkdirAll(done, 0o700); err != nil {
		t.Fatal(err)
	}
	svc.Store.SweepPartials()
	if _, err := os.Stat(dir); err == nil {
		t.Error("an unfinished install survived the sweep")
	}
	if _, err := os.Stat(done); err != nil {
		t.Errorf("a finished install was swept: %v", err)
	}
}

// ---- a Python agent in the sandbox, through the service ---------------------------

const pipSandboxCatalog = `{"version":"t","agents":[
 {"id":"fakepy","name":"Fake Py","install":{"method":"pip","package":"fakepy","bin":"fakepy","python":{"min":"3.9","max":"3.13"}},
  "latest":{"from":"pypi"},"detect":{"command":"fakepy-not-on-path","args":["--version"]},
  "register":{"name":"fakepy","args":["acp"]},
  "sandbox":{"method":"pip","python":"3.13","dataEnv":{"FAKE_LOCALES":"locales"},"approxMB":1}}]}`

func pipSandboxService(t *testing.T, idx *httptest.Server, api *httptest.Server) *Service {
	t.Helper()
	svc := newService(t, pipSandboxCatalog, api)
	root := t.TempDir()
	svc.MountRoot, svc.MountsFile, svc.GuestBase = filepath.Join(root, "mounts", "agent-manager"), filepath.Join(root, "bridge", "mounts.json"), "/guest/home/.bnbloc-agents"
	svc.PyPIURL, svc.HTTP, svc.PyIndexURL = api.URL, api.Client(), idx.URL+"/"
	return svc
}

// The sandbox's build is the same package from the same index, wheels chosen for
// the guest, checked by hash and unpacked here: nothing of it runs on this computer.
// What the member and the guest see is one folder, a launcher and the registration.
func TestAPythonAgentIsUnpackedForTheGuestAndRegisteredAsAGuestAgent(t *testing.T) {
	needPython(t)
	svc := pipSandboxService(t, pyIndex(t, depWheel, appWheel), pypiAPI(t, "2.0.0"))
	if err := do(t, svc, "fakepy", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteRegistration(); err != nil {
		t.Fatal(err)
	}
	dir := svc.sandboxStore().versionDir("fakepy", "2.0.0")
	for _, f := range []string{"site/fakepy/cli.py", "site/fakedep/__init__.py", "data/locales/en.yaml", "requirements.lock", "bin/fakepy"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin", "fakepy")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the launcher must be runnable by the guest's user: %v", fi)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "site", "fakepy", "cli.py")); fi == nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("a package file must be readable by the guest's user: %v", fi)
	}
	if _, err := os.Stat(svc.Store.versionDir("fakepy", "2.0.0")); err == nil {
		t.Error("a sandbox install put something in this computer's own store")
	}
	if _, err := os.Stat(dir + partialSuffix); err == nil {
		t.Error("a finished install leaves no partial folder")
	}
	d, ok := readDropIn(t, svc)
	if !ok || len(d.Agents) != 1 || d.Agents[0].Command != "$BNBLOC_MOUNT_AGENT_FAKEPY/bin/fakepy" || strings.Join(d.Agents[0].Args, " ") != "acp" {
		t.Fatalf("the ACP core's entry: %+v", d)
	}
	raw, _ := os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	if !strings.Contains(string(raw), `"sandbox": "guest"`) {
		t.Errorf("the entry must be a guest agent, never run on this computer: %s", raw)
	}
	if ms := readMounts(t, svc); len(ms) != 1 || ms[0].Host != dir || !ms[0].PerSpace || ms[0].Env != "BNBLOC_MOUNT_AGENT_FAKEPY" {
		t.Errorf("the bridge's descriptor: %+v", ms)
	}
	v := view(svc, "fakepy")
	if !v.Sandbox || v.Managed == nil || v.Managed.Version != "2.0.0" || v.Managed.SizeBytes == 0 {
		t.Errorf("the row: %+v", v)
	}
	// The lock holds every wheel with its hash: the install can be checked later.
	lock, _ := os.ReadFile(filepath.Join(dir, "requirements.lock"))
	if !strings.Contains(string(lock), "fakepy==2.0.0 --hash=sha256:") || !strings.Contains(string(lock), "fakedep==1.0 --hash=sha256:") {
		t.Errorf("lock:\n%s", lock)
	}
}

// A wheel that could run code (a .pth file) fails the install and leaves nothing.
func TestASandboxInstallOfAWheelThatCouldRunCodeLeavesNothing(t *testing.T) {
	needPython(t)
	bad := appWheel
	bad.files = map[string]string{"fakepy/__init__.py": "", "fakepy/cli.py": "def main(): pass\n", "evil.pth": "import os\n"}
	svc := pipSandboxService(t, pyIndex(t, depWheel, bad), pypiAPI(t, "2.0.0"))
	err := do(t, svc, "fakepy", ActInstallSandbox)
	if err == nil || !strings.Contains(err.Error(), ".pth") {
		t.Fatalf("err = %v, want a refusal that names the .pth file", err)
	}
	entries, _ := os.ReadDir(svc.sandboxStore().agentDir("fakepy"))
	if len(entries) != 0 {
		t.Errorf("a failed install left %v", entries)
	}
	if v := view(svc, "fakepy"); v.Managed != nil || v.Registered {
		t.Errorf("a failed install is not recorded: %+v", v)
	}
}

// An agent lives in one place: a copy on this computer and one in the sandbox would
// register under one name. Installing it here while the sandbox has it, and the other
// way round, is refused with the reason.
func TestAPythonAgentLivesInOnePlace(t *testing.T) {
	needPython(t)
	svc := pipSandboxService(t, pyIndex(t, depWheel, appWheel), pypiAPI(t, "2.0.0"))
	if err := do(t, svc, "fakepy", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	if err := do(t, svc, "fakepy", ActInstall); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("install on this computer while it is in the sandbox: %v", err)
	}
}
