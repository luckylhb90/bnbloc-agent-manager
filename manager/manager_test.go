package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testCatalog has one agent the manager installs with npm (and registers),
// and one that is only found on the computer and updates itself.
const testCatalog = `{
  "version": "t1",
  "agents": [
    {"id": "fake", "name": "Fake Agent",
     "install": {"method": "npm", "package": "fake-agent", "bin": "fakeagent"},
     "latest": {"from": "npm"},
     "detect": {"command": "fakeagent", "args": ["--version"]},
     "register": {"name": "fake", "args": ["acp"]}},
    {"id": "ext", "name": "Ext Agent",
     "detect": {"command": "extagent", "args": ["--version"]},
     "found": [{"kind": "vendor", "contains": "/extbin/"}],
     "update": [{"kind": "vendor", "args": ["upgrade"], "pinArgs": ["upgrade", "{version}"]}],
     "register": {"name": "ext"}},
    {"id": "nopin", "name": "No Pin Agent",
     "detect": {"command": "nopinagent", "args": ["--version"]},
     "found": [{"kind": "vendor", "contains": "/nopinbin/"}],
     "update": [{"kind": "vendor", "args": ["update"]}]}
  ]
}`

// fakeNPM puts an `npm` on the PATH that installs a fake package: a command
// named fakeagent that prints the version asked for. FAKE_NPM_MODE: "" works,
// "fail" exits 1, "wrong" installs a command reporting another version,
// "sleep" prints nothing and waits.
func fakeNPM(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake npm is a shell script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
prefix=""; last=""
while [ $# -gt 0 ]; do
  case "$1" in --prefix) prefix="$2"; shift;; esac
  last="$1"; shift
done
case "$FAKE_NPM_MODE" in
  fail) echo "npm ERR! boom" >&2; exit 1;;
  sleep) sleep 30; exit 0;;
esac
ver="${last##*@}"
[ "$FAKE_NPM_MODE" = wrong ] && ver="9.9.9"
mkdir -p "$prefix/node_modules/.bin"
printf '#!/bin/sh\necho "fakeagent %s"\n' "$ver" > "$prefix/node_modules/.bin/fakeagent"
chmod +x "$prefix/node_modules/.bin/fakeagent"
echo "added 1 package"
`
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type registryOpts struct {
	latest string
	times  map[string]string
}

func newRegistry(t *testing.T, o registryOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/fake-agent") {
			http.NotFound(w, r)
			return
		}
		versions := map[string]any{}
		for v := range o.times {
			versions[v] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dist-tags": map[string]string{"latest": o.latest}, "time": o.times, "versions": versions})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newService(t *testing.T, catalog string, reg *httptest.Server) *Service {
	t.Helper()
	cat, err := ParseCatalog([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := &Service{
		Catalog: cat, Store: Store{Dir: filepath.Join(dir, "data")}, ACPHome: filepath.Join(dir, "acp"),
		Version: "test", StallWindow: time.Minute,
	}
	if reg != nil {
		svc.RegistryURL, svc.HTTP = reg.URL, reg.Client()
	}
	if err := svc.Init(); err != nil {
		t.Fatal(err)
	}
	return svc
}

func do(t *testing.T, svc *Service, id, action string) error {
	t.Helper()
	err := svc.Do(context.Background(), id, action, nil)
	return err
}

func readDropIn(t *testing.T, svc *Service) (dropInDoc, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	if err != nil {
		return dropInDoc{}, false
	}
	var d dropInDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d, true
}

type dropInDoc struct {
	Agents []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"agents"`
}

func view(svc *Service, id string) AgentView {
	for _, a := range svc.Snapshot().Agents {
		if a.ID == id {
			return a
		}
	}
	return AgentView{}
}

func TestVersionHelpers(t *testing.T) {
	for _, v := range []string{"1.2.3", "0.0.1", "10.20.30"} {
		if !ValidVersion(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	// A version names a directory and an install command: nothing else passes.
	for _, v := range []string{"", "1.2", "1.2.3-beta", "../1.2.3", "1.2.3/..", "v1.2.3", "1.2.3 ", "latest", "1234567890.1.1"} {
		if ValidVersion(v) {
			t.Errorf("%q must not be valid", v)
		}
	}
	if got := ParseVersion("Hermes Agent v0.12.0 (2026.4.30)", nil); got != "0.12.0" {
		t.Errorf("first x.y.z of the output: %q", got)
	}
	if Compare("1.10.0", "1.9.9") != 1 || Compare("1.2.3", "1.2.3") != 0 || Compare("0.9.0", "1.0.0") != -1 {
		t.Error("Compare orders numerically, not as text")
	}
	if !AtLeast("22.19.1", "22.19") || AtLeast("22.18.9", "22.19") || !AtLeast("v24.1.0", "22") || AtLeast("20.0.0", "22") {
		t.Error("AtLeast compares only the parts the minimum names")
	}
}

func TestEmbeddedCatalogIsValid(t *testing.T) {
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Agents) < 6 {
		t.Errorf("the first batch has six agents, got %d", len(c.Agents))
	}
}

func TestCatalogRefusesWhatItCannotVerify(t *testing.T) {
	cases := map[string]string{
		"a shell-string method":    `{"id":"a","name":"A","install":{"method":"script","package":"x","bin":"x"},"detect":{"command":"a"}}`,
		"a path as the command":    `{"id":"a","name":"A","detect":{"command":"/bin/a"}}`,
		"a bad package name":       `{"id":"a","name":"A","install":{"method":"npm","package":"x; rm -rf ~","bin":"x"},"detect":{"command":"a"}}`,
		"a bad id":                 `{"id":"../a","name":"A","detect":{"command":"a"}}`,
		"an unknown latest source": `{"id":"a","name":"A","latest":{"from":"url"},"detect":{"command":"a"}}`,
		"pinArgs without version":  `{"id":"a","name":"A","detect":{"command":"a"},"found":[{"kind":"k","contains":"/x/"}],"update":[{"kind":"k","args":["u"],"pinArgs":["u","1.0.0"]}]}`,
		"an update with no rule":   `{"id":"a","name":"A","detect":{"command":"a"},"update":[{"kind":"k","args":["u"]}]}`,
		"a reserved found kind":    `{"id":"a","name":"A","detect":{"command":"a"},"found":[{"kind":"managed","contains":"/x/"}]}`,
		"a parse with no group":    `{"id":"a","name":"A","detect":{"command":"a","parse":"\\d+"}}`,
	}
	for name, agent := range cases {
		if _, err := ParseCatalog([]byte(`{"version":"x","agents":[` + agent + `]}`)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseCatalog([]byte(`{"version":"x","agents":[],"extra":1}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func TestSettingsKeepWhatIsValidAndNameWhatIsNot(t *testing.T) {
	cat, _ := ParseCatalog([]byte(testCatalog))
	path := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(path, []byte(`{"allowed":["fake","nope"],"pinned":["fake@1.2.3","fake@latest","ghost@1.0.0","bad"],"holdDays":"3","autoUpdate":"on"}`), 0o600)
	s, err := ReadSettings(path, cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Allowed) != 1 || s.Pinned["fake"] != "1.2.3" || len(s.Pinned) != 1 || s.HoldDays != 3 || !s.AutoUpdate {
		t.Errorf("settings: %+v", s)
	}
	if len(s.Notes) != 4 {
		t.Errorf("every ignored line is named, got %v", s.Notes)
	}
	if s.Allows("ext") || !s.Allows("fake") {
		t.Error("an allowed list offers only its agents")
	}
	if _, err := ReadSettings(filepath.Join(t.TempDir(), "missing.json"), cat); err == nil {
		t.Error("a named settings file that cannot be read is an error")
	}
}

func TestHoldDaysOffersOnlyAVersionOldEnough(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d int) string { return now.Add(-time.Duration(d) * 24 * time.Hour).Format(time.RFC3339) }
	reg := newRegistry(t, registryOpts{latest: "1.3.0", times: map[string]string{"1.1.0": ago(30), "1.2.0": ago(10), "1.3.0": ago(1)}})
	svc := newService(t, testCatalog, reg)
	svc.Now = func() time.Time { return now }
	got, err := svc.latestFromRegistry(context.Background(), "fake-agent", 0)
	if err != nil || got != "1.3.0" {
		t.Fatalf("no hold: %q %v", got, err)
	}
	if got, _ = svc.latestFromRegistry(context.Background(), "fake-agent", 3); got != "1.2.0" {
		t.Errorf("a release one day old is not offered with a 3-day hold: %q", got)
	}
	if _, err := svc.latestFromRegistry(context.Background(), "fake-agent", 90); err == nil {
		t.Error("with nothing old enough the answer is an error, not the newest")
	}
	if _, err := svc.latestFromRegistry(context.Background(), "missing", 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("an unknown package says so: %v", err)
	}
	svc.RegistryURL = "http://registry.example.com"
	if _, err := svc.latestFromRegistry(context.Background(), "fake-agent", 0); err == nil {
		t.Error("a registry that is not https is refused")
	}
}

func TestInstallUpdateRollbackRemove(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)

	if _, ok := readDropIn(t, svc); ok {
		t.Fatal("nothing is registered before the member acts")
	}
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	v := view(svc, "fake")
	if v.Managed == nil || v.Managed.Version != "1.0.0" || !v.Registered {
		t.Fatalf("after install: %+v", v)
	}
	d, ok := readDropIn(t, svc)
	if !ok || len(d.Agents) != 1 || d.Agents[0].Name != "fake" || d.Agents[0].Args[0] != "acp" {
		t.Fatalf("registration: %+v", d)
	}
	want := binPath(svc.Store.versionDir("fake", "1.0.0"), "fakeagent")
	if d.Agents[0].Command != want {
		t.Errorf("the entry names the exact version directory: %q, want %q", d.Agents[0].Command, want)
	}
	if err := do(t, svc, "fake", ActInstall); err == nil {
		t.Error("a second install of a managed agent is refused")
	}

	// A new version arrives: installed beside the old one, then switched.
	reg2 := newRegistry(t, registryOpts{latest: "1.1.0", times: map[string]string{"1.1.0": "2026-01-01T00:00:00Z"}})
	svc.RegistryURL, svc.HTTP = reg2.URL, reg2.Client()
	if err := do(t, svc, "fake", ActCheck); err != nil {
		t.Fatal(err)
	}
	if v = view(svc, "fake"); !v.UpdateAvailable || v.Target != "1.1.0" || !v.Can.Update {
		t.Fatalf("update available: %+v", v)
	}
	if err := do(t, svc, "fake", ActUpdate); err != nil {
		t.Fatal(err)
	}
	v = view(svc, "fake")
	if v.Managed.Version != "1.1.0" || v.Managed.Previous != "1.0.0" || !v.Can.Rollback || v.UpdateAvailable {
		t.Fatalf("after update: %+v", v.Managed)
	}
	for _, ver := range []string{"1.0.0", "1.1.0"} {
		if _, err := os.Stat(svc.Store.versionDir("fake", ver)); err != nil {
			t.Errorf("version %s must be kept: %v", ver, err)
		}
	}
	if d, _ = readDropIn(t, svc); !strings.Contains(d.Agents[0].Command, "1.1.0") {
		t.Errorf("the registration moved to the new version: %q", d.Agents[0].Command)
	}

	if err := do(t, svc, "fake", ActRollback); err != nil {
		t.Fatal(err)
	}
	if v = view(svc, "fake"); v.Managed.Version != "1.0.0" || v.Managed.Previous != "1.1.0" {
		t.Fatalf("after rollback: %+v", v.Managed)
	}
	if d, _ = readDropIn(t, svc); !strings.Contains(d.Agents[0].Command, "1.0.0") {
		t.Errorf("rollback moves the registration back: %q", d.Agents[0].Command)
	}

	// A fresh service reads the same state: nothing lives only in memory.
	again := &Service{Catalog: svc.Catalog, Store: svc.Store, ACPHome: svc.ACPHome}
	if err := again.Init(); err != nil {
		t.Fatal(err)
	}
	if v = view(again, "fake"); v.Managed == nil || v.Managed.Version != "1.0.0" {
		t.Errorf("after a restart: %+v", v)
	}

	if err := do(t, svc, "fake", ActRemove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.Store.agentDir("fake")); !os.IsNotExist(err) {
		t.Error("remove deletes every version")
	}
	if _, ok := readDropIn(t, svc); ok {
		t.Error("with nothing registered the drop-in file is removed")
	}
}

func TestAFailedInstallLeavesNothingBehind(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	for _, mode := range []string{"fail", "wrong"} {
		t.Setenv("FAKE_NPM_MODE", mode)
		if err := do(t, svc, "fake", ActInstall); err == nil {
			t.Fatalf("%s: install must fail", mode)
		}
		entries, _ := os.ReadDir(svc.Store.agentDir("fake"))
		if len(entries) != 0 {
			t.Errorf("%s: a failed install left %v", mode, entries)
		}
		if v := view(svc, "fake"); v.Managed != nil || v.Op == nil || v.Op.Error == "" {
			t.Errorf("%s: nothing installed and the error is shown: %+v", mode, v)
		}
		if _, ok := readDropIn(t, svc); ok {
			t.Errorf("%s: nothing registered", mode)
		}
	}
	// The wrong-version case names the mismatch.
	if err := do(t, svc, "fake", ActInstall); err == nil || !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("an install whose command reports another version is refused naming it: %v", err)
	}
}

func TestAFailedUpdateKeepsTheWorkingVersion(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	reg2 := newRegistry(t, registryOpts{latest: "2.0.0", times: map[string]string{"2.0.0": "2026-01-01T00:00:00Z"}})
	svc.RegistryURL, svc.HTTP = reg2.URL, reg2.Client()
	t.Setenv("FAKE_NPM_MODE", "fail")
	if err := do(t, svc, "fake", ActUpdate); err == nil {
		t.Fatal("update must fail")
	}
	v := view(svc, "fake")
	if v.Managed.Version != "1.0.0" {
		t.Errorf("the working version stays: %+v", v.Managed)
	}
	d, _ := readDropIn(t, svc)
	if len(d.Agents) != 1 || !strings.Contains(d.Agents[0].Command, "1.0.0") {
		t.Errorf("the registration is untouched: %+v", d)
	}
	if _, err := os.Stat(svc.Store.versionDir("fake", "1.0.0")); err != nil {
		t.Error("the working version's files are untouched")
	}
}

func TestAStalledOperationEndsByActivityNotByTotalTime(t *testing.T) {
	fakeNPM(t)
	t.Setenv("FAKE_NPM_MODE", "sleep")
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	svc.StallWindow = 400 * time.Millisecond
	start := time.Now()
	err := do(t, svc, "fake", ActInstall)
	if err == nil || !strings.Contains(err.Error(), "no activity") && view(svc, "fake").Op.Error == "" {
		t.Fatalf("a silent install is stopped: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the stall ended it, not the 30s sleep")
	}
	if op := view(svc, "fake").Op; op == nil || !strings.Contains(op.Error, "no activity") {
		t.Errorf("the page says why: %+v", op)
	}
}

func TestCancelEndsARunningOperation(t *testing.T) {
	fakeNPM(t)
	t.Setenv("FAKE_NPM_MODE", "sleep")
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	if err := svc.Begin("fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	if err := svc.Begin("fake", ActInstall); err != ErrBusy {
		t.Errorf("one operation per agent at a time: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for view(svc, "fake").Op == nil || len(view(svc, "fake").Op.Log) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the install never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := svc.Cancel("fake"); err != nil {
		t.Fatal(err)
	}
	for view(svc, "fake").Op.Running {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("cancel did not end the operation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op := view(svc, "fake").Op; op.Error != "cancelled" {
		t.Errorf("cancelled is said as such: %q", op.Error)
	}
	if entries, _ := os.ReadDir(svc.Store.agentDir("fake")); len(entries) != 0 {
		t.Errorf("a cancelled install leaves no directory: %v", entries)
	}
}

// foundAgent puts an executable named name in a directory named dirName on
// the PATH. It prints "<name> <version>" read from <dir>/version, and on
// `upgrade [v]` / `update` writes the new version (v, else 2.0.0) to it.
func foundAgent(t *testing.T, name, dirName, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0o644)
	script := fmt.Sprintf(`#!/bin/sh
d="$(dirname "$0")"
case "$1" in
  --version) echo "%s $(cat "$d/version")";;
  upgrade|update) echo "${2:-2.0.0}" > "$d/version"; echo "updated";;
esac
`, name)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestAnInstallTheMemberMadeIsUpdatedOnlyByItsOwnMethodAndOnlyOnAClick(t *testing.T) {
	dir := foundAgent(t, "extagent", "extbin", "1.0.0")
	svc := newService(t, testCatalog, nil)
	svc.RefreshDetected(context.Background())

	v := view(svc, "ext")
	if v.External == nil || v.External.Kind != "vendor" || v.External.Version != "1.0.0" || v.Managed != nil {
		t.Fatalf("found: %+v", v)
	}
	if v.Can.Install || v.Can.Remove {
		t.Error("the manager neither installs over nor removes what the member made")
	}
	if !v.Can.Update || !v.UpdateUnknown || !strings.HasSuffix(v.UpdateCommand, "extagent upgrade") {
		t.Errorf("no newer version is known, so the updater is offered as such, with the exact command: %+v", v)
	}
	if _, ok := readDropIn(t, svc); ok {
		t.Fatal("finding an agent registers nothing: the member's click does")
	}
	if err := do(t, svc, "ext", ActRegister); err != nil {
		t.Fatal(err)
	}
	d, ok := readDropIn(t, svc)
	if !ok || len(d.Agents) != 1 || d.Agents[0].Name != "ext" || !strings.HasSuffix(d.Agents[0].Command, "extbin/extagent") {
		t.Fatalf("registered by its found command: %+v", d)
	}
	if err := do(t, svc, "ext", ActUpdate); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "version")); strings.TrimSpace(string(got)) != "2.0.0" {
		t.Errorf("its own updater ran: %q", got)
	}
	if v = view(svc, "ext"); v.External.Version != "2.0.0" {
		t.Errorf("the new version is read back: %+v", v.External)
	}
	if err := do(t, svc, "ext", ActRemove); err == nil {
		t.Error("remove refuses an install the manager did not make")
	}
}

func TestAPinThatAnInstallCannotHonorDisablesTheUpdate(t *testing.T) {
	foundAgent(t, "extagent", "extbin", "1.0.0")
	foundAgent(t, "nopinagent", "nopinbin", "1.0.0")
	svc := newService(t, testCatalog, nil)
	svc.Settings = Settings{Pinned: map[string]string{"ext": "1.5.0", "nopin": "1.5.0"}}
	svc.RefreshDetected(context.Background())

	// ext can be set to a chosen version: the pinned form runs.
	v := view(svc, "ext")
	if !v.Can.Update || !strings.HasSuffix(v.UpdateCommand, "extagent upgrade 1.5.0") {
		t.Errorf("a pin runs the pinned form: %+v", v)
	}
	// nopin cannot: the update is off and says why, never run unpinned.
	v = view(svc, "nopin")
	if v.Can.Update || !strings.Contains(v.Reason, "pinned") {
		t.Errorf("a pin the install cannot honor disables its update: %+v", v)
	}
	if err := do(t, svc, "nopin", ActUpdate); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("and the action refuses: %v", err)
	}
}

func TestAnAgentTheOrganizationDoesNotOfferCannotBeActedOn(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	svc.Settings = Settings{Allowed: []string{"ext"}}
	v := view(svc, "fake")
	if v.Status != "blocked" || v.Can.Install {
		t.Errorf("blocked: %+v", v)
	}
	if err := do(t, svc, "fake", ActInstall); err == nil || !strings.Contains(err.Error(), "organization") {
		t.Errorf("install refused: %v", err)
	}
}

func TestAnAgentThatIsNotForThisSystemSaysSoInsteadOfOfferingAnInstall(t *testing.T) {
	cat := strings.Replace(testCatalog, `{"id": "fake", "name": "Fake Agent",`, `{"id": "fake", "name": "Fake Agent", "platforms": ["plan9"],`, 1)
	svc := newService(t, cat, nil)
	if v := view(svc, "fake"); v.Status != "unsupported" || v.Can.Install {
		t.Errorf("%+v", v)
	}
	if err := do(t, svc, "fake", ActInstall); err == nil {
		t.Error("install refused on another system")
	}
}

func TestStartSweepsAnInterruptedInstall(t *testing.T) {
	svc := newService(t, testCatalog, nil)
	partial := svc.Store.versionDir("fake", "1.0.0") + partialSuffix
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.Store.SweepPartials()
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Error("a partial directory is never referenced and is swept")
	}
}

func TestAnUnreadableStateStopsTheStartInsteadOfBeingReplaced(t *testing.T) {
	svc := newService(t, testCatalog, nil)
	_ = os.MkdirAll(svc.Store.Dir, 0o700)
	_ = os.WriteFile(svc.Store.statePath(), []byte("{not json"), 0o600)
	again := &Service{Catalog: svc.Catalog, Store: svc.Store}
	if err := again.Init(); err == nil {
		t.Fatal("the state records what was installed: it is never silently reset")
	}
	if raw, _ := os.ReadFile(svc.Store.statePath()); string(raw) != "{not json" {
		t.Error("and the file is left as it was")
	}
}

func TestAutomaticUpdateRunsOnlyWhenOnAndNeverLoopsOnAFailure(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	reg2 := newRegistry(t, registryOpts{latest: "1.1.0", times: map[string]string{"1.1.0": "2026-01-01T00:00:00Z"}})
	svc.RegistryURL, svc.HTTP = reg2.URL, reg2.Client()
	svc.CheckAll(context.Background())

	svc.autoUpdate() // off: nothing happens
	if view(svc, "fake").Managed.Version != "1.0.0" {
		t.Fatal("automatic updates are off by default")
	}
	svc.Settings.AutoUpdate = true
	t.Setenv("FAKE_NPM_MODE", "fail")
	svc.autoUpdate()
	waitIdle(t, svc, "fake")
	first := view(svc, "fake").Op.Started
	svc.autoUpdate() // the same target failed: not tried again
	if got := view(svc, "fake").Op.Started; !got.Equal(first) {
		t.Error("a failed automatic update is not retried for the same target")
	}
	t.Setenv("FAKE_NPM_MODE", "")
	reg3 := newRegistry(t, registryOpts{latest: "1.2.0", times: map[string]string{"1.2.0": "2026-01-01T00:00:00Z"}})
	svc.RegistryURL, svc.HTTP = reg3.URL, reg3.Client()
	svc.CheckAll(context.Background())
	svc.autoUpdate()
	waitIdle(t, svc, "fake")
	if v := view(svc, "fake"); v.Managed.Version != "1.2.0" {
		t.Errorf("a new target is tried: %+v", v.Managed)
	}
}

func waitIdle(t *testing.T, svc *Service, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if op := view(svc, id).Op; op != nil && !op.Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the operation did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAPIRequiresTheBearerAndRunsActions(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	srv := httptest.NewServer(svc.Handler("secret", false))
	defer srv.Close()
	call := func(method, path, token string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for _, tok := range []string{"", "wrong", "secre"} {
		if code, _ := call("GET", "/v1/state", tok); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	if code, body := call("GET", "/v1/state", "secret"); code != 200 || body["catalogVersion"] != "t1" {
		t.Errorf("state: %d %v", code, body)
	}
	if code, _ := call("POST", "/v1/agents/fake/explode", "secret"); code != http.StatusBadRequest {
		t.Errorf("an unknown action is a 400: %d", code)
	}
	if code, _ := call("POST", "/v1/agents/../fake/install", "secret"); code == http.StatusAccepted {
		t.Error("a path trick is not an install")
	}
	if code, _ := call("POST", "/v1/agents/nobody/install", "secret"); code != http.StatusBadRequest {
		t.Errorf("an unknown agent is a 400: %d", code)
	}
	if code, _ := call("POST", "/v1/agents/fake/install", "secret"); code != http.StatusAccepted {
		t.Fatalf("install starts: %d", code)
	}
	waitIdle(t, svc, "fake")
	if v := view(svc, "fake"); v.Managed == nil {
		t.Errorf("the install ran: %+v", v)
	}
}

func TestTheOperationLogNeverCarriesTheEnvironment(t *testing.T) {
	fakeNPM(t)
	t.Setenv("SECRET_TOKEN_FOR_TEST", "hunter2-do-not-log")
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	svc := newService(t, testCatalog, reg)
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	for _, l := range view(svc, "fake").Op.Log {
		if strings.Contains(l, "hunter2") {
			t.Fatalf("a log line carries the environment: %q", l)
		}
	}
}
