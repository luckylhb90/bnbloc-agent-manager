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
	for _, v := range []string{"", "1.2", "1.2.3-", "1.2.3-beta/../x", "1.2.3+build", "1.2.3-a..b", "../1.2.3", "1.2.3/..", "v1.2.3", "1.2.3 ", "latest", "1234567890.1.1"} {
		if ValidVersion(v) {
			t.Errorf("%q must not be valid", v)
		}
	}
	for _, v := range []string{"0.2.1-rc.2", "1.0.0-alpha", "1.0.0-beta.11", "1.0.0-x-y.1"} {
		if !ValidVersion(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	// semver order: a release is above its pre-releases, parts compare one by one.
	ordered := []string{"0.2.1-alpha.5", "0.2.1-rc.2", "0.2.1-rc.3", "0.2.1-rc.10", "0.2.1", "0.2.2-rc.1", "0.10.0"}
	for i := 1; i < len(ordered); i++ {
		if Compare(ordered[i-1], ordered[i]) != -1 || Compare(ordered[i], ordered[i-1]) != 1 {
			t.Errorf("%s should sort below %s", ordered[i-1], ordered[i])
		}
	}
	if got := ParseVersion("dsh 0.2.1-rc.2\n", nil); got != "0.2.1-rc.2" {
		t.Errorf("a pre-release is read whole: %q", got)
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

func TestAnAdapterIsAnEntryOfItsOwnUnderAnAgent(t *testing.T) {
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	// What the member knows (the command they installed) and what rooms run
	// (its ACP adapter) are different programs, so two entries.
	for _, id := range []string{"claude", "codex"} {
		agent, _ := c.Entry(id)
		adapter, ok := c.Entry(id + "-acp")
		if agent.Register != nil || !ok || adapter.For != id || adapter.Register == nil || adapter.Register.Name != id {
			t.Errorf("%s: agent %+v adapter %+v", id, agent.Register, adapter)
		}
	}
	base := `{"id":"a","name":"A","detect":{"command":"a"}}`
	for name, extra := range map[string]string{
		"for an unknown agent":     `{"id":"b","name":"B","for":"zzz","detect":{"command":"b"}}`,
		"for itself":               `{"id":"b","name":"B","for":"b","detect":{"command":"b"}}`,
		"an adapter of an adapter": `{"id":"b","name":"B","for":"a","detect":{"command":"b"}},{"id":"c","name":"C","for":"b","detect":{"command":"c"}}`,
	} {
		if _, err := ParseCatalog([]byte(`{"version":"x","agents":[` + base + `,` + extra + `]}`)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	svc := newService(t, `{"version":"x","agents":[`+base+`,{"id":"b","name":"B","for":"a","detect":{"command":"b"}}]}`, nil)
	if a, b := view(svc, "a"), view(svc, "b"); a.Adapter != "b" || b.For != "a" {
		t.Errorf("the page learns who serves whom: %+v %+v", a, b)
	}
}

func TestAnOrganizationsBuildAddsEntriesBesideThePublicOnes(t *testing.T) {
	base, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := base.With([]byte(`{"version":"org","agents":[{"id":"house","name":"House agent","detect":{"command":"house","args":["--version"]},"found":[{"kind":"own","contains":"/house/"}],"update":[{"kind":"own","args":["update"]}]},{"id":"house-acp","name":"Adapter","for":"claude","detect":{"command":"x"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Agents) != len(base.Agents)+2 || merged.Version != base.Version+"+org" {
		t.Errorf("merged: %d agents, version %q", len(merged.Agents), merged.Version)
	}
	if _, ok := merged.Entry("house"); !ok {
		t.Error("the house agent is there")
	}
	if len(base.Agents) == len(merged.Agents) {
		t.Error("the base catalog is not changed")
	}
	for name, extra := range map[string]string{
		"an id taken":      `{"version":"o","agents":[{"id":"gemini","name":"Dup","detect":{"command":"d"}}]}`,
		"a name taken":     `{"version":"o","agents":[{"id":"mine","name":"M","detect":{"command":"m"},"register":{"name":"gemini"}}]}`,
		"an unknown field": `{"version":"o","agents":[],"x":1}`,
		"a shell method":   `{"version":"o","agents":[{"id":"mine","name":"M","install":{"method":"script","package":"x","bin":"x"},"detect":{"command":"m"}}]}`,
	} {
		if _, err := base.With([]byte(extra)); err == nil {
			t.Errorf("%s was accepted", name)
		}
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
	s, _, err := ReadSettings(path, cat)
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
	if _, _, err := ReadSettings(filepath.Join(t.TempDir(), "missing.json"), cat); err == nil {
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

func TestAnUpdateMayRunAPackageManagerInsteadOfTheAgent(t *testing.T) {
	dir := foundAgent(t, "viaagent", "viabin", "1.0.0")
	// The package manager: a program of its own that bumps the agent's version.
	helper := filepath.Join(t.TempDir(), "viahelper")
	script := "#!/bin/sh\necho \"$@\" > \"" + filepath.Join(dir, "ran") + "\"\necho 2.0.0 > \"" + filepath.Join(dir, "version") + "\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(helper)+string(os.PathListSeparator)+os.Getenv("PATH"))
	svc := newService(t, `{"version":"t","agents":[{"id":"via","name":"Via","detect":{"command":"viaagent","args":["--version"]},
	  "found":[{"kind":"brewed","contains":"/viabin/"}],
	  "update":[{"kind":"brewed","command":"viahelper","args":["upgrade","viaagent"]}]}]}`, nil)
	svc.RefreshDetected(context.Background())
	v := view(svc, "via")
	if !v.Can.Update || !strings.HasSuffix(v.UpdateCommand, "") || !strings.HasPrefix(v.UpdateCommand, "viahelper upgrade viaagent") {
		t.Fatalf("the command shown is the helper's, not the agent's: %+v", v)
	}
	if err := do(t, svc, "via", ActUpdate); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ran")); strings.TrimSpace(string(got)) != "upgrade viaagent" {
		t.Errorf("the helper ran with its arguments: %q", got)
	}
	if v = view(svc, "via"); v.External.Version != "2.0.0" {
		t.Errorf("the new version is read back: %+v", v.External)
	}
	// A helper that is not on the computer is said, not guessed.
	t.Setenv("PATH", filepath.Join(dir)+string(os.PathListSeparator)+"/usr/bin:/bin")
	svc.RefreshDetected(context.Background())
	if err := do(t, svc, "via", ActUpdate); err == nil || !strings.Contains(err.Error(), "viahelper is needed") {
		t.Errorf("a missing helper: %v", err)
	}
	if _, err := ParseCatalog([]byte(`{"version":"x","agents":[{"id":"a","name":"A","detect":{"command":"a"},"found":[{"kind":"k","contains":"/x/"}],"update":[{"kind":"k","command":"/bin/sh","args":["-c","x"]}]}]}`)); err == nil {
		t.Error("an update command that is a path was accepted")
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

// writeSettings writes a component settings file and points the service at it.
func writeSettings(t *testing.T, svc *Service, values map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(values)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.SettingsFile = path
	svc.loadSettings()
}

func TestAMemberAddsAnAgentOfTheirOwnWithItsUpdater(t *testing.T) {
	dir := foundAgent(t, "houseagent", "housebin", "1.0.0")
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"myAgents": []string{
		`{"id":"house","name":"House agent","command":"houseagent","versionArgs":["--version"],"updateArgs":["upgrade"],"acpArgs":["acp"],"signIn":"Run it once."}`,
	}})
	svc.RefreshDetected(context.Background())
	v := view(svc, "house")
	if v.Name != "House agent" || v.Source != SourceMember || v.External == nil || v.External.Version != "1.0.0" || v.External.Kind != configuredKind {
		t.Fatalf("a configured agent is found like any: %+v", v)
	}
	if !v.Can.Update || !v.UpdateUnknown || !strings.HasSuffix(v.UpdateCommand, "houseagent upgrade") || !v.HasACP {
		t.Fatalf("its updater is shown before the click: %+v", v)
	}
	if err := do(t, svc, "house", ActUpdate); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "version")); strings.TrimSpace(string(got)) != "2.0.0" {
		t.Errorf("its own updater ran: %q", got)
	}
	// Given to rooms only by the member's click, under the id's name.
	if _, ok := readDropIn(t, svc); ok {
		t.Fatal("finding registers nothing")
	}
	if err := do(t, svc, "house", ActRegister); err != nil {
		t.Fatal(err)
	}
	if d, _ := readDropIn(t, svc); len(d.Agents) != 1 || d.Agents[0].Name != "house" || d.Agents[0].Args[0] != "acp" {
		t.Errorf("registered with the line's launch args: %+v", d)
	}
}

func TestAMemberPointsABuiltInAgentAtAnotherCommandAndNothingElse(t *testing.T) {
	foundAgent(t, "ext", "elsewhere", "3.1.4")
	other := filepath.Join(t.TempDir(), "ext") // an absolute path, a place that is not on the PATH
	_ = os.WriteFile(other, []byte("#!/bin/sh\necho ext 9.9.9\n"), 0o755)
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"myAgents": []string{
		`{"id":"ext","command":"` + other + `","versionArgs":["--version"]}`,
		`{"id":"nopin","command":"x","updateArgs":["rm","-rf"]}`,
	}})
	svc.RefreshDetected(context.Background())
	v := view(svc, "ext")
	if v.Source != SourceMember || v.External == nil || v.External.Version != "9.9.9" || v.External.Path == "" {
		t.Fatalf("the agent is found where the member said: %+v", v)
	}
	if !strings.Contains(strings.Join(svc.Snapshot().Notes, "\n"), `"nopin" is already an agent`) {
		t.Errorf("a line that does more than point an agent elsewhere is refused and said: %v", svc.Snapshot().Notes)
	}
	if view(svc, "nopin").Source != "" {
		t.Error("the built-in entry is unchanged by a refused line")
	}
}

func TestAnOrganizationAddsAgentsToRecogniseAndNeverToRun(t *testing.T) {
	foundAgent(t, "orgagent", "orgbin", "5.0.0")
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"orgAgents": []string{
		`{"id":"corp","name":"Corp CLI","command":"orgagent","installHint":"Install it from the portal."}`,
		`{"id":"corp-flag","command":"orgagent","versionArgs":["-v"]}`,
		// Everything below must be refused: each would make members' computers run what an admin typed.
		`{"id":"bad-update","command":"orgagent","updateArgs":["update"]}`,
		`{"id":"bad-cmd","command":"orgagent","updateCommand":"brew","updateArgs":["upgrade"]}`,
		`{"id":"bad-acp","command":"orgagent","acpArgs":["acp"]}`,
		`{"id":"bad-args","command":"orgagent","versionArgs":["-c","id"]}`,
		`{"id":"bad-path","command":"/bin/sh"}`,
		`{"id":"fake","command":"orgagent"}`,
		`{"id":"bad-field","command":"orgagent","run":"x"}`,
		`not json`,
	}})
	svc.RefreshDetected(context.Background())
	corp := view(svc, "corp")
	if corp.Source != SourceOrg || corp.External == nil || corp.External.Version != "5.0.0" || corp.Can.Update || corp.Can.Register {
		t.Fatalf("recognised, with nothing to run: %+v", corp)
	}
	if view(svc, "corp-flag").Name == "" || view(svc, "corp-flag").Source != SourceOrg {
		t.Error("an allowed version flag is accepted")
	}
	for _, id := range []string{"bad-update", "bad-cmd", "bad-acp", "bad-args", "bad-path", "bad-field"} {
		if view(svc, id).ID != "" {
			t.Errorf("%s was accepted", id)
		}
	}
	if view(svc, "fake").Source != "" {
		t.Error("an organization cannot change a catalog entry")
	}
	notes := strings.Join(svc.Snapshot().Notes, "\n")
	for _, want := range []string{"cannot set an install or update command", "ACP launch line", "version argument", "bare command name", `"fake" is already an agent`, "known fields", "not a JSON object"} {
		if !strings.Contains(notes, want) {
			t.Errorf("a refused line is named (%q): %s", want, notes)
		}
	}
}

func TestConfiguredAgentsFollowTheOrganizationsOwnPolicy(t *testing.T) {
	foundAgent(t, "orgagent", "orgbin", "5.0.0")
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{
		"orgAgents": []string{`{"id":"corp","command":"orgagent"}`},
		"allowed":   []string{"corp"},
		"pinned":    []string{"corp@5.0.0", "ghost@1.0.0"},
	})
	svc.RefreshDetected(context.Background())
	if v := view(svc, "fake"); v.Status != "blocked" {
		t.Errorf("an allowed list that names a configured agent offers only it: %+v", v)
	}
	if v := view(svc, "corp"); v.Status != "installed" || v.Pinned != "5.0.0" {
		t.Errorf("a configured agent may be allowed and pinned: %+v", v)
	}
	if !strings.Contains(strings.Join(svc.Snapshot().Notes, "\n"), "ghost") {
		t.Error("a pin of an agent nobody defined is named")
	}
}

func TestAConfiguredLineNeverRunsAtParseTime(t *testing.T) {
	// Parsing and applying lines runs nothing; only a version check on a found
	// command does, with the arguments the rules above allow.
	marker := filepath.Join(t.TempDir(), "ran")
	cat, _ := ParseCatalog([]byte(testCatalog))
	_, notes := cat.WithConfigured([]string{`{"id":"x","command":"touch","versionArgs":["` + marker + `"]}`}, nil)
	if _, err := os.Stat(marker); err == nil || len(notes) == 0 {
		t.Errorf("an organization's arguments are refused and nothing ran: %v", notes)
	}
}

func TestOnlyANpmGlobalPrefixCountsAsAGlobalInstall(t *testing.T) {
	e := &Entry{ID: "x", Install: &Install{Method: "npm", Package: "x", Bin: "x"}}
	store := "/data/store"
	for path, want := range map[string]string{
		"/home/u/.local/share/mise/installs/node/24/lib/node_modules/@openai/codex/bin/codex.js": KindNPMGlobal,
		"/usr/local/lib/node_modules/x/bin/x.js":                                                 KindNPMGlobal,
		"C:/Users/u/AppData/Roaming/npm/node_modules/x/bin/x.js":                                 KindNPMGlobal,
		// A tool's own copy inside its project: `npm install -g` would not update it.
		"/home/u/.local/share/other-tool/pi/node_modules/@scope/x/dist/cli.js": KindUnknown,
		"/work/project/node_modules/x/bin/x.js":                                KindUnknown,
		"/data/store/agents/x/1.0.0/node_modules/x/bin/x.js":                   KindManaged,
	} {
		if got, _ := classify(e, path, store); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

func TestAMemberInstallsWithTheirOwnInstallLineAndOnlyOnAClick(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `printf '#!/bin/sh\necho "instagent 1.2.3"\n' > ` + filepath.Join(bin, "instagent") + ` && chmod +x ` + filepath.Join(bin, "instagent")
	line, _ := json.Marshal(map[string]any{"id": "inst", "name": "Inst", "command": "instagent", "installArgs": []string{"-c", script}})
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"myAgents": []string{string(line)}})
	svc.RefreshDetected(context.Background())
	v := view(svc, "inst")
	if v.Status != "absent" || !v.Can.Install || !strings.HasPrefix(v.InstallCommand, "sh -c ") {
		t.Fatalf("absent, with the command shown before the click: %+v", v)
	}
	if err := do(t, svc, "inst", ActInstall); err != nil {
		t.Fatal(err)
	}
	if v = view(svc, "inst"); v.Status != "installed" || v.External == nil || v.External.Version != "1.2.3" || v.Can.Install {
		t.Fatalf("what the installer put on the PATH is found: %+v", v)
	}
}

func TestAnInstallerThatLeavesTheAgentOffThePathSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	line, _ := json.Marshal(map[string]any{"id": "ghost", "command": "ghostagent-not-installed", "installArgs": []string{"-c", "true"}})
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"myAgents": []string{string(line)}})
	svc.RefreshDetected(context.Background())
	if err := do(t, svc, "ghost", ActInstall); err == nil || !strings.Contains(err.Error(), "not on the PATH") {
		t.Errorf("an installer whose result is not found: %v", err)
	}
	if view(svc, "ghost").Status != "absent" {
		t.Error("and it stays absent")
	}
}

func TestAnOrganizationCannotSetAnInstallLine(t *testing.T) {
	svc := newService(t, testCatalog, nil)
	writeSettings(t, svc, map[string]any{"orgAgents": []string{
		`{"id":"corp","command":"corp","installArgs":["-c","curl x | sh"]}`,
		`{"id":"corp2","command":"corp","installCommand":"sh","installArgs":["-c","x"]}`,
	}})
	if view(svc, "corp").ID != "" || view(svc, "corp2").ID != "" {
		t.Error("an install line from an organization was accepted")
	}
	if !strings.Contains(strings.Join(svc.Snapshot().Notes, "\n"), "cannot set an install or update command") {
		t.Errorf("and it is said: %v", svc.Snapshot().Notes)
	}
}

func TestAnAgentMayBeWrittenOverSeveralLinesOrSeveralToALine(t *testing.T) {
	cat, _ := ParseCatalog([]byte(testCatalog))
	// What a member pasted: one object cut at its line breaks into three lines.
	pasted := []string{
		`{"id":"corpcli","name":"corpcli","command":"corpcli","versionArgs":["--version"],`,
		` "installArgs":["-c","curl -fsSL https://example.com/install.sh | bash"],`,
		` "updateArgs":["update"],"signIn":"Run ` + "`corpcli login`" + `."}`,
	}
	got, notes := cat.WithConfigured(nil, pasted)
	e, ok := got.Entry("corpcli")
	if !ok || len(notes) != 0 || e.InstallRun == nil || e.InstallRun.Args[1] != "curl -fsSL https://example.com/install.sh | bash" || len(e.Update) != 1 {
		t.Fatalf("an object over three lines is one agent: %v %+v", notes, e)
	}
	// Two on a line, braces and quotes inside strings, an escaped quote.
	got, notes = cat.WithConfigured(nil, []string{`{"id":"a1","command":"x","signIn":"use { and } and \"quotes\""} {"id":"a2","command":"y"}`})
	if _, ok := got.Entry("a1"); !ok || len(notes) != 0 {
		t.Errorf("braces inside strings do not count: %v", notes)
	}
	if _, ok := got.Entry("a2"); !ok {
		t.Error("two objects on a line are two agents")
	}
	// What cannot be used is said, once, and the rest still works.
	got, notes = cat.WithConfigured(nil, []string{"corpcli:", `{"id":"b1","command":"x"}`, `{"id":"b2","command":"y"`})
	if _, ok := got.Entry("b1"); !ok {
		t.Error("a good object beside bad text is kept")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "outside any {") || !strings.Contains(joined, "never closed") {
		t.Errorf("stray text and an unclosed object are named: %v", notes)
	}
	if _, ok := got.Entry("b2"); ok {
		t.Error("an unclosed object is not guessed at")
	}
}

func TestAnAdapterIsToldWhereItsAgentIs(t *testing.T) {
	fakeNPM(t)
	reg := newRegistry(t, registryOpts{latest: "1.0.0", times: map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}})
	cat := `{"version":"t","agents":[
	  {"id":"fake","name":"Fake","install":{"method":"npm","package":"fake-agent","bin":"fakeagent"},"latest":{"from":"npm"},"detect":{"command":"fakeagent","args":["--version"]}},
	  {"id":"fake-acp","name":"Adapter","for":"fake","install":{"method":"npm","package":"fake-agent","bin":"fakeagent"},"latest":{"from":"npm"},"detect":{"command":"fakeacp"},
	   "register":{"name":"fake","commandEnv":"FAKE_AGENT_COMMAND","env":{"KEEP":"1"}}}]}`
	svc := newService(t, cat, reg)
	// The agent is not on the computer: the adapter is registered without a path (it finds the agent itself).
	if err := do(t, svc, "fake-acp", ActInstall); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	var doc struct {
		Agents []struct {
			Env map[string]string `json:"env"`
		} `json:"agents"`
	}
	_ = json.Unmarshal(raw, &doc)
	if len(doc.Agents) != 1 || len(doc.Agents[0].Env) != 1 || doc.Agents[0].Env["KEEP"] != "1" {
		t.Fatalf("no agent, no path: %s", raw)
	}
	// Installed by the manager: the adapter is told that exact copy.
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	if err := do(t, svc, "fake-acp", ActUnregister); err != nil {
		t.Fatal(err)
	}
	if err := do(t, svc, "fake-acp", ActRegister); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	doc.Agents = nil
	_ = json.Unmarshal(raw, &doc)
	want := binPath(svc.Store.versionDir("fake", "1.0.0"), "fakeagent")
	if len(doc.Agents) != 1 || doc.Agents[0].Env["FAKE_AGENT_COMMAND"] != want || doc.Agents[0].Env["KEEP"] != "1" {
		t.Errorf("the adapter is told where its agent is: %s", raw)
	}
	for name, bad := range map[string]string{
		"a variable that changes how a process runs": `"register":{"name":"x","commandEnv":"NODE_OPTIONS"}`,
		"a lower-case name":                          `"register":{"name":"x","commandEnv":"agent_cmd"}`,
	} {
		if _, err := ParseCatalog([]byte(`{"version":"x","agents":[{"id":"a","name":"A","detect":{"command":"a"}},{"id":"b","name":"B","for":"a","detect":{"command":"b"},` + bad + `}]}`)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseCatalog([]byte(`{"version":"x","agents":[{"id":"a","name":"A","detect":{"command":"a"},"register":{"name":"x","commandEnv":"X_CMD"}}]}`)); err == nil {
		t.Error("commandEnv on an entry that serves no agent was accepted")
	}
}

func TestARowSaysWhenTheMembersOwnFileDefinesItsName(t *testing.T) {
	foundAgent(t, "extagent", "extbin", "1.0.0")
	svc := newService(t, testCatalog, nil)
	svc.RefreshDetected(context.Background())
	if view(svc, "ext").Shadowed {
		t.Fatal("no agents.json, nothing shadows")
	}
	_ = os.MkdirAll(svc.ACPHome, 0o755)
	own := filepath.Join(svc.ACPHome, OwnConfigFile)
	_ = os.WriteFile(own, []byte(`{"agents":[{"name":"ext","command":"mine"},{"name":"other","command":"x"}]}`), 0o600)
	if v := view(svc, "ext"); !v.Shadowed {
		t.Errorf("the member's own entry wins on this name, and the row says so: %+v", v)
	}
	if view(svc, "fake").Shadowed {
		t.Error("another agent's name is not shadowed")
	}
	_ = os.WriteFile(own, []byte("{not json"), 0o600)
	if view(svc, "ext").Shadowed {
		t.Error("a file that does not parse names nothing (the core keeps its previous config, which this cannot know)")
	}
}

// What fill-settings answered, run against the real assist with the settings'
// own help text and a pasted install note (2026-10-24): it must be a line the
// manager takes as an agent with its install and update.
func TestWhatTheFillAssistWritesIsAnAgent(t *testing.T) {
	line := `{"id":"corpcli","name":"Corp CLI","command":"corpcli","installArgs":["-c","curl -fsSL https://example.com/install.sh | bash"],"updateArgs":["update"],"versionArgs":["--version"]}`
	if len(line) > 512 {
		t.Fatal("longer than the one-line bound of a lines setting")
	}
	cat, _ := ParseCatalog([]byte(testCatalog))
	got, notes := cat.WithConfigured(nil, []string{line})
	e, ok := got.Entry("corpcli")
	if !ok || len(notes) != 0 || e.InstallRun == nil || e.InstallRun.Command != "sh" || e.InstallRun.Args[0] != "-c" || len(e.Update) != 1 {
		t.Fatalf("%v %+v", notes, e)
	}
}

// The help of each configurable setting is what an AI sees when it fills the
// setting from a note, so its example is held to the rules: it must be an
// agent the manager accepts for that scope, and the help stays within the
// platform's 300 characters.
func TestTheSettingsHelpCarriesAWorkingExample(t *testing.T) {
	raw, err := os.ReadFile("../package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Settings []struct{ Key, Help string } `json:"settings"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	scope := map[string]string{SettingMyAgents: SourceMember, SettingOrgAgents: SourceOrg}
	seen := 0
	for _, s := range pkg.Settings {
		if len(s.Help) > 300 {
			t.Errorf("%s: help is %d characters, the platform allows 300", s.Key, len(s.Help))
		}
		sc, ok := scope[s.Key]
		if !ok {
			continue
		}
		seen++
		objs, _ := splitObjects(s.Help)
		if len(objs) == 0 {
			t.Errorf("%s: the help gives no example object", s.Key)
			continue
		}
		a, err := decodeLine(objs[0])
		if err == nil {
			err = a.check(sc)
		}
		if err != nil {
			t.Errorf("%s: its own example is not accepted: %v", s.Key, err)
		}
	}
	if seen != 2 {
		t.Errorf("both configurable settings are declared, found %d", seen)
	}
}
