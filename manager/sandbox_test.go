package manager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const sandboxCatalog = `{
  "version": "s1",
  "agents": [
    {"id": "sbx", "name": "Sandbox Agent",
     "sandbox": {"package": "sbx-linux-{cpu}", "bin": "bin/tool"},
     "latest": {"from": "npm", "package": "sbx"},
     "detect": {"command": ""},
     "register": {"name": "sbx", "args": ["acp"]}}
  ]
}`

type tarFile struct {
	name string
	body string
	mode int64
	link string // a symlink target
	typ  byte   // 0: file
}

// packageTarball is an npm package tarball: everything under package/.
func packageTarball(t *testing.T, files ...tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if f.link != "" {
			h = &tar.Header{Name: f.name, Linkname: f.link, Typeflag: tar.TypeSymlink, Mode: 0o777}
		} else if f.typ != 0 {
			h = &tar.Header{Name: f.name, Typeflag: f.typ, Mode: 0o755}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if f.link == "" && f.typ == 0 {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func goodPackage(t *testing.T, pkg, version string) []byte {
	return packageTarball(t,
		tarFile{name: "package/package.json", body: fmt.Sprintf(`{"name":%q,"version":%q}`, pkg, version), mode: 0o644},
		tarFile{name: "package/bin/tool", body: "#!/bin/sh\necho tool " + version + "\n", mode: 0o700},
		tarFile{name: "package/README.md", body: "hello", mode: 0o600},
		tarFile{name: "package/bin/tool-link", link: "tool"},
	)
}

type sandboxRegistry struct {
	srv *httptest.Server
	// tarballs by version (what the registry serves) and the hash it claims.
	tarballs map[string][]byte
	claim    map[string]string // version -> integrity override
}

func newSandboxRegistry(t *testing.T) *sandboxRegistry {
	t.Helper()
	r := &sandboxRegistry{tarballs: map[string][]byte{}, claim: map[string]string{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		p := strings.TrimPrefix(q.URL.Path, "/")
		switch {
		case strings.HasPrefix(p, "tarball/"):
			if b, ok := r.tarballs[path.Base(p)]; ok {
				w.Write(b)
				return
			}
		case p == "sbx": // the newest version, read for the update check
			v := ""
			for k := range r.tarballs {
				if v == "" || Compare(k, v) > 0 {
					v = k
				}
			}
			versions := map[string]any{}
			times := map[string]string{}
			for k := range r.tarballs {
				versions[k] = map[string]any{}
				times[k] = "2026-01-01T00:00:00Z"
			}
			json.NewEncoder(w).Encode(map[string]any{"dist-tags": map[string]string{"latest": v}, "time": times, "versions": versions})
			return
		case p == "fake-agent": // the host package of an agent that can also live in the sandbox
			json.NewEncoder(w).Encode(map[string]any{"dist-tags": map[string]string{"latest": "1.0.0"}, "time": map[string]string{"1.0.0": "2026-01-01T00:00:00Z"}, "versions": map[string]any{"1.0.0": map[string]any{}}})
			return
		case strings.HasPrefix(p, "sbx-linux-"):
			name, version := path.Dir(p), path.Base(p)
			if b, ok := r.tarballs[version]; ok {
				sum := sha512.Sum512(b)
				integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
				if c := r.claim[version]; c != "" {
					integrity = c
				}
				json.NewEncoder(w).Encode(map[string]any{"name": name, "version": version, "dist": map[string]string{"tarball": "https://" + q.Host + "/tarball/" + version, "integrity": integrity}})
				return
			}
		}
		http.NotFound(w, q)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// sandboxService is a service under the desktop: the folders the sandbox needs.
func sandboxService(t *testing.T, reg *sandboxRegistry) *Service {
	t.Helper()
	svc := newService(t, sandboxCatalog, nil)
	root := t.TempDir()
	svc.MountRoot, svc.MountsFile, svc.GuestBase = filepath.Join(root, "mounts", "agent-manager"), filepath.Join(root, "bridge", "mounts.json"), "/guest/home/.bnbloc-agents"
	svc.RegistryURL, svc.HTTP = reg.srv.URL, reg.srv.Client()
	return svc
}

func (r *sandboxRegistry) publish(t *testing.T, version string) {
	r.tarballs[version] = goodPackage(t, "sbx-linux-"+guestCPU(), version)
}

func readMounts(t *testing.T, svc *Service) []mountEntry {
	t.Helper()
	raw, err := os.ReadFile(svc.MountsFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Contract string       `json:"contract"`
		Mounts   []mountEntry `json:"mounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Contract != "bnbloc.sandbox.mounts/v1" {
		t.Fatalf("mounts file %s: %v", raw, err)
	}
	return doc.Mounts
}

func TestASandboxAgentIsUnpackedIntoItsOwnFolderAndGivenToRoomsAsAPerSpaceMount(t *testing.T) {
	reg := newSandboxRegistry(t)
	reg.publish(t, "1.0.0")
	svc := sandboxService(t, reg)
	if err := do(t, svc, "sbx", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteRegistration(); err != nil {
		t.Fatal(err)
	}
	dir := svc.sandboxStore().versionDir("sbx", "1.0.0")
	if !strings.HasPrefix(dir, svc.MountRoot) {
		t.Fatalf("%s is not under the mount root %s", dir, svc.MountRoot)
	}
	if fi, err := os.Stat(filepath.Join(dir, "bin", "tool")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the program must be runnable by the guest's user (0755): %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "README.md")); fi == nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("a file the guest's user must read is 0644: %v", fi)
	}
	if got, err := os.Readlink(filepath.Join(dir, "bin", "tool-link")); err != nil || got != "tool" {
		t.Errorf("a link inside the package is kept: %q %v", got, err)
	}
	if _, err := os.Stat(dir + partialSuffix); err == nil {
		t.Error("a finished install leaves no partial folder")
	}
	ms := readMounts(t, svc)
	if len(ms) != 1 || ms[0].Host != dir || ms[0].Guest != "/guest/home/.bnbloc-agents/sbx" || !ms[0].PerSpace || ms[0].Env != "BNBLOC_MOUNT_AGENT_SBX" || ms[0].For != "own" {
		t.Errorf("the bridge's descriptor: %+v", ms)
	}
	d, ok := readDropIn(t, svc)
	if !ok || len(d.Agents) != 1 || d.Agents[0].Name != "sbx" || d.Agents[0].Command != "$BNBLOC_MOUNT_AGENT_SBX/bin/tool" || strings.Join(d.Agents[0].Args, " ") != "acp" {
		t.Errorf("the ACP core's entry: %+v", d)
	}
	raw, _ := os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	if !strings.Contains(string(raw), `"sandbox": "guest"`) {
		t.Errorf("the entry must be a guest agent, never run on this computer: %s", raw)
	}
	v := view(svc, "sbx")
	if !v.Sandbox || v.Status != "installed" || v.Managed == nil || v.Managed.Version != "1.0.0" || v.Managed.SizeBytes == 0 || v.External != nil {
		t.Errorf("the row: %+v managed %+v", v, v.Managed)
	}
}

func TestASandboxAgentIsOfferedOnlyWhereTheDesktopGaveItTheSandboxFolders(t *testing.T) {
	reg := newSandboxRegistry(t)
	reg.publish(t, "1.0.0")
	svc := sandboxService(t, reg)
	svc.MountRoot, svc.MountsFile, svc.GuestBase = "", "", ""
	v := view(svc, "sbx")
	if v.Can.InstallSandbox {
		t.Errorf("without the sandbox's folders the sandbox cannot be chosen: %+v", v.Can)
	}
	if !strings.Contains(v.Reason, "sandbox") {
		t.Errorf("and the row says why: %q", v.Reason)
	}
	if err := do(t, svc, "sbx", ActInstallSandbox); err == nil {
		t.Error("an install without the sandbox's folders must fail, not fall back to this computer")
	}
	if _, err := os.Stat(svc.Store.agentDir("sbx")); err == nil {
		t.Error("nothing is put in the app's own folder")
	}
}

func TestADownloadThatIsNotWhatTheRegistryHashedInstallsNothing(t *testing.T) {
	reg := newSandboxRegistry(t)
	reg.publish(t, "1.0.0")
	reg.claim["1.0.0"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	svc := sandboxService(t, reg)
	err := do(t, svc, "sbx", ActInstallSandbox)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("a tampered download: %v", err)
	}
	if entries, _ := os.ReadDir(svc.sandboxStore().agentDir("sbx")); len(entries) != 0 {
		t.Errorf("a failed install leaves nothing behind: %v", entries)
	}
	if view(svc, "sbx").Managed != nil {
		t.Error("and records nothing")
	}
	reg.claim["1.0.0"] = "sha1-abc"
	if err := do(t, svc, "sbx", ActInstallSandbox); err == nil || !strings.Contains(err.Error(), "sha512") {
		t.Errorf("a registry that gives no sha512 hash gives nothing to check against: %v", err)
	}
}

func TestAPackageThatWritesOutsideItsFolderOrIsNotWhatWasAskedForIsRefused(t *testing.T) {
	cases := map[string][]tarFile{
		"a path that leaves the folder": {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}, {name: "package/../../evil", body: "x", mode: 0o644}},
		"an absolute path":              {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}, {name: "/tmp/evil-sbx", body: "x", mode: 0o644}},
		"a link that leaves the folder": {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}, {name: "package/l", link: "../../../etc/passwd"}},
		"an absolute link":              {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}, {name: "package/l", link: "/etc/passwd"}},
		"a device":                      {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}, {name: "package/dev", typ: tar.TypeChar}},
		"another package":               {{name: "package/package.json", body: `{"name":"other","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}},
		"another version":               {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"9.9.9"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o755}},
		"no program":                    {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}},
		"a program that cannot run":     {{name: "package/package.json", body: `{"name":"sbx-linux-` + guestCPU() + `","version":"1.0.0"}`, mode: 0o644}, {name: "package/bin/tool", body: "x", mode: 0o644}},
	}
	for name, files := range cases {
		reg := newSandboxRegistry(t)
		reg.tarballs["1.0.0"] = packageTarball(t, files...)
		svc := sandboxService(t, reg)
		if err := do(t, svc, "sbx", ActInstallSandbox); err == nil {
			t.Errorf("%s: installed", name)
		}
		if entries, _ := os.ReadDir(svc.sandboxStore().agentDir("sbx")); len(entries) != 0 {
			t.Errorf("%s: left %v behind", name, entries)
		}
		if _, err := os.Stat(filepath.Join(svc.MountRoot, "..", "evil")); err == nil {
			t.Errorf("%s: wrote outside the folder", name)
		}
		if view(svc, "sbx").Managed != nil {
			t.Errorf("%s: recorded an install", name)
		}
	}
}

func TestASandboxAgentUpdatesBesideTheOldVersionAndRollsBack(t *testing.T) {
	reg := newSandboxRegistry(t)
	reg.publish(t, "1.0.0")
	svc := sandboxService(t, reg)
	if err := do(t, svc, "sbx", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	reg.publish(t, "1.1.0")
	if err := do(t, svc, "sbx", ActCheck); err != nil {
		t.Fatal(err)
	}
	if v := view(svc, "sbx"); !v.UpdateAvailable || !v.Can.Update {
		t.Fatalf("an update is offered: %+v", v)
	}
	if err := do(t, svc, "sbx", ActUpdate); err != nil {
		t.Fatal(err)
	}
	_ = svc.WriteRegistration()
	if ms := readMounts(t, svc); len(ms) != 1 || !strings.HasSuffix(ms[0].Host, "/1.1.0") {
		t.Errorf("rooms are pointed at the new version: %+v", ms)
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := os.Stat(svc.sandboxStore().versionDir("sbx", v)); err != nil {
			t.Errorf("%s stays (the new one, and the one a rollback goes to): %v", v, err)
		}
	}
	if err := do(t, svc, "sbx", ActRollback); err != nil {
		t.Fatal(err)
	}
	_ = svc.WriteRegistration()
	if ms := readMounts(t, svc); len(ms) != 1 || !strings.HasSuffix(ms[0].Host, "/1.0.0") {
		t.Errorf("after the rollback: %+v", ms)
	}
	// It is never updated by itself: a conversation resumes with the new version.
	svc.Settings.AutoUpdate = true
	reg.publish(t, "1.2.0")
	_ = do(t, svc, "sbx", ActCheck)
	svc.autoUpdate()
	time.Sleep(50 * time.Millisecond)
	if cur, _ := svc.managedVersion("sbx"); cur != "1.0.0" {
		t.Errorf("an automatic update moved a sandbox agent to %s", cur)
	}
}

func TestASandboxAgentIsNotRemovedFromUnderARoomThatRunsIt(t *testing.T) {
	reg := newSandboxRegistry(t)
	reg.publish(t, "1.0.0")
	svc := sandboxService(t, reg)
	if err := do(t, svc, "sbx", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	// No word from the core: its sessions cannot be seen.
	if err := do(t, svc, "sbx", ActRemove); err == nil {
		t.Fatal("removed with no word from the core")
	}
	writeInUse(t, svc, time.Now(), false, "$BNBLOC_MOUNT_AGENT_SBX/bin/tool")
	if err := do(t, svc, "sbx", ActRemove); err == nil || !strings.Contains(err.Error(), "still be running") {
		t.Fatalf("removed from under a live room: %v", err)
	}
	if _, err := os.Stat(svc.sandboxStore().versionDir("sbx", "1.0.0")); err != nil {
		t.Error("the files must still be there")
	}
	// Another agent's session does not hold it.
	writeInUse(t, svc, time.Now(), false, "$BNBLOC_MOUNT_AGENT_OTHER/bin/x", "/usr/bin/sbx")
	if err := do(t, svc, "sbx", ActRemove); err != nil {
		t.Fatalf("no room runs it: %v", err)
	}
	_ = svc.WriteRegistration()
	if ms := readMounts(t, svc); len(ms) != 0 {
		t.Errorf("rooms get nothing once it is removed: %+v", ms)
	}
	if _, err := os.Stat(svc.sandboxStore().agentDir("sbx")); err == nil {
		t.Error("its folder is gone")
	}
	if _, ok := readDropIn(t, svc); ok {
		t.Error("and the ACP core is told nothing")
	}
}

func TestACatalogEntryForTheSandboxIsValidatedLikeAnyOther(t *testing.T) {
	bad := map[string]string{
		"an install too":    `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"install":{"method":"npm","package":"p","bin":"x"},"latest":{"from":"npm","package":"p"},"register":{"name":"a"}}`,
		"no register":       `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"latest":{"from":"npm","package":"p"}}`,
		"a path that exits": `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"../x"},"latest":{"from":"npm","package":"p"},"register":{"name":"a"}}`,
		"no version source": `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"register":{"name":"a"}}`,
		"a bad package":     `{"id":"a","name":"A","sandbox":{"package":"P Q","bin":"bin/x"},"latest":{"from":"npm","package":"p"},"register":{"name":"a"}}`,
	}
	for name, entry := range bad {
		if _, err := ParseCatalog([]byte(`{"version":"x","agents":[` + entry + `]}`)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An agent the member may put on this computer or in the sandbox: one row, the
// choice made when installing, recorded, and followed by everything after.
const bothCatalog = `{
  "version": "b1",
  "agents": [
    {"id": "fake", "name": "Fake Agent",
     "install": {"method": "npm", "package": "fake-agent", "bin": "fakeagent"},
     "sandbox": {"package": "sbx-linux-{cpu}", "bin": "bin/tool", "latest": "sbx", "approxMB": 7, "note": "the new generation"},
     "latest": {"from": "npm"},
     "detect": {"command": "fakeagent", "args": ["--version"]},
     "register": {"name": "fake", "args": ["acp"]}}
  ]
}`

func bothService(t *testing.T, reg *sandboxRegistry) *Service {
	t.Helper()
	svc := sandboxService(t, reg)
	cat, err := ParseCatalog([]byte(bothCatalog))
	if err != nil {
		t.Fatal(err)
	}
	svc.Catalog = cat
	svc.eff.Store(nil)
	return svc
}

// An agent that runs on this computer and can live in the sandbox has two lines, each with its own
// state, store folder and registration: installing, updating or removing one never touches the other.
// (A member who put Hermes in the sandbox could no longer see the Hermes already on their computer:
// one row showed one install and hid the other, and one registration meant they could not be used
// side by side.)
func TestAnAgentHasAComputerLineAndASandboxLineThatDoNotTouchEachOther(t *testing.T) {
	fakeNPM(t)
	reg := newSandboxRegistry(t)
	reg.publish(t, "2.0.0")
	svc := bothService(t, reg)

	host, sb := view(svc, "fake"), view(svc, "fake-sandbox")
	if host.Sandbox || !sb.Sandbox || sb.SandboxOf != "fake" || sb.Name != "Fake Agent (sandbox)" {
		t.Fatalf("two lines: %+v / %+v", host, sb)
	}
	if !host.Can.Install || host.Can.InstallSandbox || !sb.Can.InstallSandbox || sb.Can.Install {
		t.Fatalf("each line offers its own install only: host %+v, sandbox %+v", host.Can, sb.Can)
	}
	if sb.ApproxMB != 7 || !strings.Contains(sb.Note, "the new generation") {
		t.Errorf("the sandbox line says what it is: %d MB %q", sb.ApproxMB, sb.Note)
	}

	// The sandbox line.
	if err := do(t, svc, "fake-sandbox", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	if got := svc.placementOf("fake-sandbox"); got != PlaceSandbox {
		t.Fatalf("placement %q", got)
	}
	if got := svc.placementOf("fake"); got != "" {
		t.Fatalf("the computer line has nothing installed, placement %q", got)
	}
	sb, host = view(svc, "fake-sandbox"), view(svc, "fake")
	if sb.SignIn != sandboxSignIn || sb.Managed == nil || sb.Managed.Version != "2.0.0" || sb.Can.InstallSandbox {
		t.Errorf("the sandbox line is installed: %+v", sb)
	}
	if host.Managed != nil || !host.Can.Install {
		t.Errorf("and the computer line is untouched, still installable: %+v", host)
	}
	if d, ok := readDropIn(t, svc); !ok || len(d.Agents) != 1 || d.Agents[0].Name != "fake-sandbox" || d.Agents[0].Command != "$BNBLOC_MOUNT_AGENT_FAKE_SANDBOX/bin/tool" {
		t.Errorf("rooms get the guest agent under its own name: %+v", d)
	}

	// The computer line too, beside it: both reach rooms, each under its own name.
	if err := do(t, svc, "fake", ActInstall); err != nil {
		t.Fatal(err)
	}
	if got := svc.placementOf("fake"); got != PlaceHost {
		t.Fatalf("placement %q", got)
	}
	d, _ := readDropIn(t, svc)
	names := map[string]string{}
	for _, a := range d.Agents {
		names[a.Name] = a.Command
	}
	if len(names) != 2 || !strings.Contains(names["fake-sandbox"], "BNBLOC_MOUNT") || strings.Contains(names["fake"], "BNBLOC_MOUNT") || names["fake"] == "" {
		t.Fatalf("a host command and a guest command, side by side: %+v", d)
	}
	if ms := readMounts(t, svc); len(ms) != 1 || !strings.Contains(ms[0].Env, "FAKE_SANDBOX") {
		t.Errorf("the sandbox gets only its own line's folder: %+v", ms)
	}

	// Removing one leaves the other, and its registration.
	writeInUse(t, svc, time.Now(), true)
	if err := do(t, svc, "fake-sandbox", ActRemove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.sandboxStore().agentDir("fake-sandbox")); err == nil {
		t.Error("the sandbox copy is removed from the sandbox's folder")
	}
	if _, err := os.Stat(svc.Store.agentDir("fake")); err != nil {
		t.Errorf("the computer copy is still there: %v", err)
	}
	if ms := readMounts(t, svc); len(ms) != 0 {
		t.Errorf("mounts after the remove: %+v", ms)
	}
	d, _ = readDropIn(t, svc)
	if len(d.Agents) != 1 || d.Agents[0].Name != "fake" {
		t.Errorf("the computer line is still registered under its own name: %+v", d)
	}
	// An install on the wrong line is refused, naming the right place.
	if err := do(t, svc, "fake-sandbox", ActInstall); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("a computer install of the sandbox line: %v", err)
	}
	if err := do(t, svc, "fake", ActInstallSandbox); err == nil {
		t.Errorf("a sandbox install of the computer line was accepted")
	}
}

func TestAnOrganizationCanOfferAnAgentOnlyInTheSandbox(t *testing.T) {
	fakeNPM(t)
	reg := newSandboxRegistry(t)
	reg.publish(t, "2.0.0")
	svc := bothService(t, reg)
	svc.Settings.SandboxOnly = map[string]bool{"fake": true}
	host, sb := view(svc, "fake"), view(svc, "fake-sandbox")
	if host.Can.Install || !strings.Contains(host.Reason, "only in the sandbox") {
		t.Fatalf("the computer line is off and says why: %+v", host)
	}
	if !sb.Can.InstallSandbox {
		t.Fatalf("the sandbox line is offered: %+v", sb)
	}
	if err := do(t, svc, "fake", ActInstall); err == nil || !strings.Contains(err.Error(), "only in the sandbox") {
		t.Errorf("the host install is refused, not just hidden: %v", err)
	}
	if err := do(t, svc, "fake-sandbox", ActInstallSandbox); err != nil {
		t.Errorf("the sandbox install works: %v", err)
	}
}

func TestSandboxOnlyNamesOnlyAgentsThatCanLiveInTheSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(`{"sandboxOnly":["fake","ext","nobody"]}`), 0o600)
	cat, _ := ParseCatalog([]byte(`{"version":"x","agents":[
	  {"id":"fake","name":"F","install":{"method":"npm","package":"f","bin":"f"},"sandbox":{"package":"f-{cpu}","bin":"bin/f"},"latest":{"from":"npm"},"detect":{"command":"f"},"register":{"name":"fake"}},
	  {"id":"ext","name":"E","detect":{"command":"e"}}]}`))
	set, _, err := ReadSettings(path, cat)
	if err != nil {
		t.Fatal(err)
	}
	if !set.SandboxOnly["fake"] || set.SandboxOnly["ext"] || set.SandboxOnly["nobody"] || len(set.Notes) != 2 {
		t.Errorf("sandboxOnly %v notes %v", set.SandboxOnly, set.Notes)
	}
}

func TestTheNewestVersionIsLookedForWhereTheAgentLives(t *testing.T) {
	fakeNPM(t)
	reg := newSandboxRegistry(t)
	reg.publish(t, "2.0.0")
	svc := bothService(t, reg)
	if err := do(t, svc, "fake-sandbox", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	reg.publish(t, "2.1.0")
	if err := do(t, svc, "fake-sandbox", ActCheck); err != nil {
		t.Fatal(err)
	}
	if v := view(svc, "fake-sandbox"); !v.UpdateAvailable || v.Target != "2.1.0" {
		t.Errorf("the sandbox install is checked against the sandbox's package (the host one is not even in this registry): %+v", v)
	}
}

// An agent that is a package with dependencies: this computer's npm installs it
// for the guest's platform, never running a script, and rooms run it with the
// guest's node.
const npmSandboxCatalog = `{
  "version": "n1",
  "agents": [
    {"id": "dep", "name": "Dep Agent",
     "install": {"method": "npm", "package": "dep-agent", "bin": "depagent"},
     "sandbox": {"method": "npm", "bin": "depagent", "approxMB": 100},
     "latest": {"from": "npm"},
     "detect": {"command": "depagent", "args": ["--version"]},
     "register": {"name": "dep", "args": ["--acp"]}}
  ]
}`

// fakeCrossNPM is an npm that records its arguments and installs the package
// the way npm does: node_modules/<pkg>/package.json and a .bin link.
func fakeCrossNPM(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake npm is a shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "` + version + `"; exit 0; fi
echo "$@" >> '` + log + `'
prefix=""; last=""
while [ $# -gt 0 ]; do
  case "$1" in --prefix) prefix="$2"; shift;; esac
  last="$1"; shift
done
pkg="${last%@*}"; ver="${last##*@}"
mkdir -p "$prefix/node_modules/$pkg" "$prefix/node_modules/.bin"
printf '{"name":"%s","version":"%s"}' "$pkg" "$ver" > "$prefix/node_modules/$pkg/package.json"
printf '#!/usr/bin/env node\nconsole.log("x")\n' > "$prefix/node_modules/$pkg/cli.js"
chmod 600 "$prefix/node_modules/$pkg/cli.js"
ln -s "../$pkg/cli.js" "$prefix/node_modules/.bin/depagent"
chmod 700 "$prefix/node_modules/$pkg"
`
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// setSandbox changes the sandbox block of an agent and of its sandbox line (a catalog is read once,
// the line is made from the agent: a test that edits the agent afterwards edits both).
func setSandbox(svc *Service, id string, f func(*SandboxInstall)) {
	for i := range svc.Catalog.Agents {
		if e := &svc.Catalog.Agents[i]; e.ID == id || e.SandboxOf == id {
			sb := *e.Sandbox
			f(&sb)
			e.Sandbox = &sb
		}
	}
	svc.eff.Store(nil)
}

func npmSandboxService(t *testing.T) *Service {
	t.Helper()
	reg := newSandboxRegistry(t)
	svc := sandboxService(t, reg)
	cat, err := ParseCatalog([]byte(npmSandboxCatalog))
	if err != nil {
		t.Fatal(err)
	}
	svc.Catalog = cat
	svc.eff.Store(nil)
	return svc
}

func TestAnAgentWithDependenciesIsInstalledForTheGuestsPlatformWithoutRunningAScript(t *testing.T) {
	log := fakeCrossNPM(t, "11.9.0")
	svc := npmSandboxService(t)
	svc.Settings.Pinned = map[string]string{"dep": "3.1.4"} // the registry here has no such package; the pin names the version
	if err := do(t, svc, "dep-sandbox", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	for _, want := range []string{"--os=linux", "--cpu=" + npmCPU(), "--libc=glibc", "--ignore-scripts", "dep-agent@3.1.4"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("npm was run as %q, want %s", strings.TrimSpace(string(raw)), want)
		}
	}
	dir := svc.sandboxStore().versionDir("dep-sandbox", "3.1.4")
	if fi, err := os.Stat(filepath.Join(dir, "node_modules", "dep-agent", "cli.js")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("a file the guest's user must read is 0644 (npm left 0600): %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "node_modules", "dep-agent")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("a folder the guest's user must list is 0755 (npm left 0700): %v", fi)
	}
	if got, err := os.Readlink(filepath.Join(dir, "node_modules", ".bin", "depagent")); err != nil || got != "../dep-agent/cli.js" {
		t.Errorf("the command link is kept as npm made it: %q %v", got, err)
	}
	_ = svc.WriteRegistration()
	d, _ := readDropIn(t, svc)
	if len(d.Agents) != 1 || d.Agents[0].Command != "$BNBLOC_MOUNT_AGENT_DEP_SANDBOX/node_modules/.bin/depagent" || strings.Join(d.Agents[0].Args, " ") != "--acp" {
		t.Errorf("rooms launch it through the guest's node_modules/.bin: %+v", d)
	}
	if ms := readMounts(t, svc); len(ms) != 1 || ms[0].Host != dir {
		t.Errorf("mounts %+v", ms)
	}
}

func TestAnOldNpmThatWouldInstallThisComputersPlatformIsRefused(t *testing.T) {
	log := fakeCrossNPM(t, "10.2.0")
	svc := npmSandboxService(t)
	svc.Settings.Pinned = map[string]string{"dep": "3.1.4"}
	err := do(t, svc, "dep-sandbox", ActInstallSandbox)
	if err == nil || !strings.Contains(err.Error(), "npm 10.5.0 or newer") {
		t.Fatalf("an npm without --libc: %v", err)
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("npm installed nothing, but ran: %s", raw)
	}
}

func TestAPackageThatNamesAnotherVersionOrNoCommandIsNotInstalled(t *testing.T) {
	fakeCrossNPM(t, "11.9.0")
	svc := npmSandboxService(t)
	svc.Settings.Pinned = map[string]string{"dep": "3.1.4"}
	setSandbox(svc, "dep", func(sb *SandboxInstall) { sb.Bin = "nosuchcommand" })
	if err := do(t, svc, "dep-sandbox", ActInstallSandbox); err == nil || !strings.Contains(err.Error(), "nosuchcommand") {
		t.Fatalf("a missing command: %v", err)
	}
	if entries, _ := os.ReadDir(svc.sandboxStore().agentDir("dep-sandbox")); len(entries) != 0 {
		t.Errorf("a failed install leaves nothing behind: %v", entries)
	}
}

func TestAnAgentThatNeedsAKeyTheSandboxCannotTakeIsOfferedAndSaysWhatIsMissing(t *testing.T) {
	fakeCrossNPM(t, "11.9.0")
	svc := npmSandboxService(t)
	setSandbox(svc, "dep", func(sb *SandboxInstall) { sb.SignIn = "key" })
	svc.Settings.Pinned = map[string]string{"dep": "3.1.4"}
	v := view(svc, "dep-sandbox")
	if !v.Can.InstallSandbox || !strings.Contains(v.Note, "API key cannot be used in the sandbox yet") {
		t.Fatalf("the sandbox line is on and says what is missing: %+v", v)
	}
	if !view(svc, "dep").Can.Install {
		t.Fatalf("the computer line is offered too")
	}
	if err := do(t, svc, "dep-sandbox", ActInstallSandbox); err != nil {
		t.Fatalf("it installs: %v", err)
	}
}

func TestAnAgentWhoseOnlyWayInIsAnAccountLoginIsNotOfferedInTheSandbox(t *testing.T) {
	fakeCrossNPM(t, "11.9.0")
	svc := npmSandboxService(t)
	setSandbox(svc, "dep", func(sb *SandboxInstall) { sb.SignIn = "login" })
	svc.Settings.Pinned = map[string]string{"dep": "3.1.4"}
	v := view(svc, "dep-sandbox")
	if v.Can.InstallSandbox || !strings.Contains(v.Reason, "account login") {
		t.Fatalf("the sandbox line is off and says why: %+v", v)
	}
	if !view(svc, "dep").Can.Install {
		t.Fatalf("the computer line stays")
	}
	if err := do(t, svc, "dep-sandbox", ActInstallSandbox); err == nil || !strings.Contains(err.Error(), "account login") {
		t.Errorf("refused, not just hidden: %v", err)
	}
	if entries, _ := os.ReadDir(svc.sandboxStore().agentDir("dep-sandbox")); len(entries) != 0 {
		t.Errorf("nothing was installed: %v", entries)
	}
	if _, err := ParseCatalog([]byte(`{"version":"x","agents":[{"id":"a","name":"A","install":{"method":"npm","package":"a","bin":"a"},"sandbox":{"method":"npm","bin":"a","signIn":"sometimes"},"latest":{"from":"npm"},"detect":{"command":"a"},"register":{"name":"a"}}]}`)); err == nil {
		t.Error("an unknown signIn value is refused")
	}
}

// Every agent of the real catalog that runs on this computer and can be put in the sandbox has the
// two lines, the sandbox one right after it; one that cannot has a reason on its sandbox line or
// none at all. Never a sandbox line without a reason for being off.
func TestEveryAgentOfTheCatalogHasItsTwoLinesAndSaysWhyOneIsOff(t *testing.T) {
	cat, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	reg := newSandboxRegistry(t)
	svc := sandboxService(t, reg)
	svc.Catalog = cat
	svc.eff.Store(nil)
	rows := svc.Snapshot().Agents
	index := map[string]int{}
	for i, a := range rows {
		index[a.ID] = i
	}
	for _, a := range rows {
		if a.Sandbox && a.SandboxOf != "" {
			if i, ok := index[a.SandboxOf]; !ok || index[a.ID] != i+1 {
				t.Errorf("%s is not right after %s", a.ID, a.SandboxOf)
			}
			if !a.Can.InstallSandbox && a.Managed == nil && a.Reason == "" {
				t.Errorf("%s is off with no reason", a.ID)
			}
			if a.Can.Install {
				t.Errorf("%s offers a computer install", a.ID)
			}
		}
	}
	// OpenCode 1.x and OpenCode 2 are two products. OpenCode 2 has no side on this computer: one
	// line, in the sandbox, and no twin.
	if _, ok := index["opencode2"]; !ok || index["opencode2-sandbox"] != 0 {
		t.Errorf("OpenCode 2 is one line: %v", index)
	}
	if _, ok := index["opencode-sandbox"]; !ok {
		t.Errorf("OpenCode 1.x has a sandbox line: %v", index)
	}
	if e, _ := cat.Entry("opencode2"); e.sandboxRefusal() != "" || e.Install != nil || !e.isSandboxLine() {
		t.Error("OpenCode 2 is a sandbox agent and is offered there")
	}
}

// A copy the member installed themselves does not stop the sandbox copy, and the two are used side
// by side: the computer's copy stays registered under its own name while the sandbox line is
// installed (it used to be dropped from the ACP core's list for as long as the sandbox copy lived),
// and neither is ever touched by the other's install or remove.
func TestAnAgentAlreadyOnThisComputerCanAlsoBePutInTheSandbox(t *testing.T) {
	fakeNPM(t)
	reg := newSandboxRegistry(t)
	reg.publish(t, "2.0.0")
	svc := bothService(t, reg)
	hostCmd := foundAgent(t, "fakeagent", "somebin", "1.0.0")
	svc.RefreshDetected(context.Background())

	host, sb := view(svc, "fake"), view(svc, "fake-sandbox")
	if host.External == nil || host.External.Version != "1.0.0" {
		t.Fatalf("the member's copy is shown on its own line: %+v", host)
	}
	if sb.External != nil || !sb.Can.InstallSandbox {
		t.Fatalf("the sandbox line never shows the computer's copy and is offered: %+v", sb)
	}
	if err := do(t, svc, "fake", ActRegister); err != nil {
		t.Fatalf("use the member's copy in rooms: %v", err)
	}
	if err := do(t, svc, "fake-sandbox", ActInstallSandbox); err != nil {
		t.Fatalf("install in the sandbox: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(svc.ACPHome, DropInDir, DropInFile))
	d, _ := readDropIn(t, svc)
	got := map[string]string{}
	for _, a := range d.Agents {
		got[a.Name] = a.Command
	}
	if len(got) != 2 || !strings.HasSuffix(got["fake"], "/fakeagent") || !strings.Contains(got["fake-sandbox"], "BNBLOC_MOUNT") {
		t.Fatalf("the member's copy under its name and the guest one under its own: %s", raw)
	}
	if _, err := os.Stat(hostCmd); err != nil {
		t.Errorf("the member's own copy was touched: %v", err)
	}
	writeInUse(t, svc, time.Now(), true) // the core has said no room runs it
	if err := do(t, svc, "fake-sandbox", ActRemove); err != nil {
		t.Fatal(err)
	}
	d, _ = readDropIn(t, svc)
	if len(d.Agents) != 1 || d.Agents[0].Name != "fake" || !strings.HasSuffix(d.Agents[0].Command, "/fakeagent") {
		t.Errorf("with the sandbox copy removed the member's copy is still registered: %+v", d.Agents)
	}
	if _, err := os.Stat(hostCmd); err != nil {
		t.Errorf("the member's own copy was touched by the remove: %v", err)
	}
}

// A sandbox install recorded under the id of an agent's computer line (how one was kept before each
// place had a line) is forgotten at start, with the folder nothing would show any more: no migration.
func TestAnOldSandboxRecordUnderTheComputerLinesIdIsForgotten(t *testing.T) {
	fakeNPM(t)
	reg := newSandboxRegistry(t)
	reg.publish(t, "2.0.0")
	svc := bothService(t, reg)
	old := svc.sandboxStore().versionDir("fake", "2.0.0")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.mutate(func(st *State) {
		st.Agents["fake"] = &AgentState{Version: "2.0.0", Method: sandboxMethod, Registered: true}
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Init(); err != nil {
		t.Fatal(err)
	}
	if v := view(svc, "fake"); v.Managed != nil || v.Registered {
		t.Errorf("the computer line shows an install that is not on this computer: %+v", v)
	}
	if _, err := os.Stat(svc.sandboxStore().agentDir("fake")); err == nil {
		t.Error("the orphan folder is left in the sandbox's store")
	}
	// A real install of the computer line is never touched.
	if err := svc.mutate(func(st *State) { st.Agents["fake"] = &AgentState{Version: "1.0.0", Method: "npm"} }); err != nil {
		t.Fatal(err)
	}
	if err := svc.Init(); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.managedVersion("fake"); got != "1.0.0" {
		t.Errorf("a computer install was forgotten: %q", got)
	}
}
