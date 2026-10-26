package manager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
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
	if err := do(t, svc, "sbx", ActInstall); err != nil {
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
	if v.Can.Install || !strings.Contains(v.Reason, "sandbox") {
		t.Errorf("without the sandbox's folders: can install %v, reason %q", v.Can.Install, v.Reason)
	}
	if err := do(t, svc, "sbx", ActInstall); err == nil {
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
	err := do(t, svc, "sbx", ActInstall)
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
	if err := do(t, svc, "sbx", ActInstall); err == nil || !strings.Contains(err.Error(), "sha512") {
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
		if err := do(t, svc, "sbx", ActInstall); err == nil {
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
	if err := do(t, svc, "sbx", ActInstall); err != nil {
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
	if err := do(t, svc, "sbx", ActInstall); err != nil {
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

func TestACatalogEntryForTheSandboxCannotAlsoRunOnThisComputer(t *testing.T) {
	bad := map[string]string{
		"an install too":    `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"install":{"method":"npm","package":"p","bin":"x"},"latest":{"from":"npm","package":"p"},"register":{"name":"a"}}`,
		"a detect":          `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"latest":{"from":"npm","package":"p"},"detect":{"command":"a"},"register":{"name":"a"}}`,
		"no register":       `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"latest":{"from":"npm","package":"p"}}`,
		"an environment":    `{"id":"a","name":"A","sandbox":{"package":"p-{cpu}","bin":"bin/x"},"latest":{"from":"npm","package":"p"},"register":{"name":"a","env":{"K":"v"}}}`,
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
