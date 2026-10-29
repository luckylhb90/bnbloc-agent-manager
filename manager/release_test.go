package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const releaseCatalog = `{
  "version": "r1",
  "agents": [
    {"id": "rel", "name": "Rel Agent",
     "detect": {"command": "relagent", "args": ["--version"]},
     "sandbox": {"method": "release", "bin": "relagent", "approxMB": 1, "release": {
        "repo": "acme/rel", "tagPrefix": "v", "assets": {"arm64": "rel-linux-arm64", "x64": "rel-linux-x64"}, "sums": "SHA256SUMS.txt"}},
     "register": {"name": "rel", "args": ["acp"]}}
  ]
}`

// releaseHost is a release page: a feed of tags, a checksums file and the program, the program served
// through a redirect to another host the way GitHub serves its assets.
type releaseHost struct {
	srv, assets *httptest.Server
	tags        []string
	body        string // what the asset host serves for the program
	sums        func(program string) string
}

func newReleaseHost(t *testing.T) *releaseHost {
	t.Helper()
	r := &releaseHost{body: "#!/bin/sh\necho relagent 1.2.0\n"}
	r.sums = func(program string) string {
		sum := sha256.Sum256([]byte(program))
		return hex.EncodeToString(sum[:]) + "  rel-linux-" + npmCPU() + "\n"
	}
	r.assets = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = fmt.Fprint(w, r.body)
	}))
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/releases.atom"):
			fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom">`)
			for _, tag := range r.tags {
				fmt.Fprintf(w, `<entry><title>%s</title><updated>2026-01-01T00:00:00Z</updated></entry>`, tag)
			}
			fmt.Fprint(w, `</feed>`)
		case strings.HasSuffix(req.URL.Path, "/SHA256SUMS.txt"):
			fmt.Fprint(w, r.sums(r.body))
		case strings.Contains(req.URL.Path, "/releases/download/") && strings.Contains(req.URL.Path, "/rel-linux-"):
			http.Redirect(w, req, r.assets.URL+"/asset", http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(func() { r.srv.Close(); r.assets.Close() })
	return r
}

func releaseService(t *testing.T, r *releaseHost) *Service {
	t.Helper()
	svc := sandboxService(t, newSandboxRegistry(t))
	cat, err := ParseCatalog([]byte(releaseCatalog))
	if err != nil {
		t.Fatal(err)
	}
	svc.Catalog = cat
	svc.eff.Store(nil)
	u, _ := url.Parse(r.srv.URL)
	svc.ReleaseHost = u.Host
	// The test hosts are https on loopback names; the client trusts their certificates and the
	// asset host is accepted as the release host's (a deployment's mirror serves from itself).
	svc.HTTP = r.srv.Client()
	tr := svc.HTTP.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.RootCAs.AddCert(r.assets.Certificate())
	svc.HTTP = &http.Client{Transport: tr}
	svc.releaseAssetOK = func(host string) bool { return true }
	return svc
}

// A program published as a file on a release page is installed into its own folder as bin/<command>,
// after its checksum from the release's own file matched; nothing of it runs here.
func TestAProgramOnAReleasePageIsInstalledAfterItsChecksumMatches(t *testing.T) {
	r := newReleaseHost(t)
	r.tags = []string{"v1.1.0", "v1.2.0", "v1.2.1-rc.1", "v0.9.0"}
	svc := releaseService(t, r)

	if got, err := svc.target(t.Context(), mustEntry(t, svc, "rel-sandbox")); err != nil || got != "1.2.0" {
		t.Fatalf("newest = %q, %v; want 1.2.0 (a release candidate is never the newest)", got, err)
	}
	if err := do(t, svc, "rel-sandbox", ActInstallSandbox); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(svc.sandboxStore().versionDir("rel-sandbox", "1.2.0"), "bin", "relagent")
	if fi, err := os.Stat(prog); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the program must be runnable by the guest's user (0755): %v %v", fi, err)
	}
	if got, _ := os.ReadFile(prog); string(got) != r.body {
		t.Errorf("the program is not what the release serves: %q", got)
	}
	_ = svc.WriteRegistration()
	d, _ := readDropIn(t, svc)
	if len(d.Agents) != 1 || d.Agents[0].Name != "rel-sandbox" || d.Agents[0].Command != "$BNBLOC_MOUNT_AGENT_REL_SANDBOX/bin/relagent" {
		t.Errorf("rooms launch it by the variable of its mount: %+v", d)
	}
	if _, err := os.Stat(svc.Store.agentDir("rel-sandbox")); err == nil {
		t.Error("nothing is put in this computer's own store")
	}
}

// A download that is not what the release's checksums say, a checksums file with no entry for the
// program, and a redirect to a host that does not serve the release's files each install nothing and
// leave nothing behind.
func TestAReleaseDownloadThatIsNotWhatTheReleaseSaysInstallsNothing(t *testing.T) {
	cases := map[string]func(*releaseHost, *Service){
		"a different file": func(r *releaseHost, _ *Service) {
			r.sums = func(string) string { return strings.Repeat("0", 64) + "  rel-linux-" + npmCPU() + "\n" }
		},
		"no entry for the program": func(r *releaseHost, _ *Service) {
			r.sums = func(string) string { return strings.Repeat("0", 64) + "  other-file\n" }
		},
		"a checksum that is not a sha256": func(r *releaseHost, _ *Service) {
			r.sums = func(string) string { return "abc  rel-linux-" + npmCPU() + "\n" }
		},
		"an asset host the release host does not serve from": func(_ *releaseHost, svc *Service) {
			svc.releaseAssetOK = nil // the real rule: only the release host and its asset host
		},
	}
	for name, mutate := range cases {
		r := newReleaseHost(t)
		r.tags = []string{"v1.2.0"}
		svc := releaseService(t, r)
		mutate(r, svc)
		if err := do(t, svc, "rel-sandbox", ActInstallSandbox); err == nil {
			t.Errorf("%s: installed", name)
		}
		if entries, _ := os.ReadDir(svc.sandboxStore().agentDir("rel-sandbox")); len(entries) != 0 {
			t.Errorf("%s: left %v behind", name, entries)
		}
		if view(svc, "rel-sandbox").Managed != nil {
			t.Errorf("%s: recorded an install", name)
		}
	}
}

// The organization's hold applies to a release as to a package: a release published more recently
// than the hold is not the newest.
func TestAReleaseNewerThanTheHoldIsNotOffered(t *testing.T) {
	r := newReleaseHost(t)
	r.tags = []string{"v1.2.0"} // the feed says it was published 2026-01-01
	svc := releaseService(t, r)
	svc.Now = func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) }
	svc.Settings.HoldDays = 7
	if got, err := svc.target(t.Context(), mustEntry(t, svc, "rel-sandbox")); err == nil {
		t.Errorf("a two-day-old release inside a seven-day hold is offered: %q", got)
	}
	svc.Settings.HoldDays = 1
	if got, err := svc.target(t.Context(), mustEntry(t, svc, "rel-sandbox")); err != nil || got != "1.2.0" {
		t.Errorf("outside the hold: %q %v", got, err)
	}
}

func TestAReleaseEntryIsValidated(t *testing.T) {
	bad := map[string]string{
		"a repository that is not owner/name": `"repo":"acme"`,
		"an asset path":                       `"repo":"acme/rel","assets":{"arm64":"../x"}`,
		"a cpu that is not one":               `"repo":"acme/rel","assets":{"riscv":"rel"}`,
		"no assets":                           `"repo":"acme/rel","assets":{}`,
	}
	for name, release := range bad {
		raw := `{"version":"x","agents":[{"id":"a","name":"A","detect":{"command":"a"},"sandbox":{"method":"release","bin":"a","release":{` + release + `,"sums":"S.txt"}},"register":{"name":"a"}}]}`
		if _, err := ParseCatalog([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseCatalog([]byte(`{"version":"x","agents":[{"id":"a","name":"A","detect":{"command":"a"},"sandbox":{"method":"release","bin":"a"},"register":{"name":"a"}}]}`)); err == nil {
		t.Error("a release install with no release block was accepted")
	}
}

func mustEntry(t *testing.T, svc *Service, id string) *Entry {
	t.Helper()
	e, ok := svc.cat().Entry(id)
	if !ok {
		t.Fatalf("no agent %s", id)
	}
	return e
}
