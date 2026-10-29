package manager

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A program published as a file on a release page (the "release" sandbox method): the agent is one
// compiled executable per platform, with a checksums file beside it, and no package registry has it
// (omp: its npm package needs bun, its release has the compiled builds). The catalog names the
// repository and the asset of each CPU; the manager downloads the one for the sandbox's CPU over
// https, checks it against the release's own checksums file, and places it as <version>/bin/<command>.
// Nothing of it is run here.
//
// What this does and does not defend against: the checksums file comes from the same release as the
// binary, so it catches a damaged or truncated download and a swapped asset, not a compromised release
// (a pinned version, an organization's own setting, is the answer to that). The download follows one
// redirect only to the host GitHub serves release assets from.

const (
	// DefaultReleaseHost is where release assets are fetched from (https only).
	DefaultReleaseHost = "github.com"
	// maxReleaseBytes bounds one downloaded program.
	maxReleaseBytes = 1 << 30
	maxSumsBytes    = 1 << 20
)

// ReleaseInstall names a program published on a release page.
type ReleaseInstall struct {
	// Repo is owner/name on the release host.
	Repo string `json:"repo"`
	// Assets is the file name of the program for each guest CPU ("arm64", "x64").
	Assets map[string]string `json:"assets"`
	// Sums is the release's checksums file (lines of `<sha256>  <file name>`).
	Sums string `json:"sums"`
	// TagPrefix is what the release's tag carries before the version ("v").
	TagPrefix string `json:"tagPrefix,omitempty"`
}

var (
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	assetNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	tagPrefixRE = regexp.MustCompile(`^[A-Za-z]{0,8}$`)
)

func (r *ReleaseInstall) validate() error {
	if !repoRE.MatchString(r.Repo) {
		return fmt.Errorf("sandbox.release.repo %q is not owner/name", r.Repo)
	}
	if len(r.Assets) == 0 {
		return errors.New("sandbox.release.assets names no file")
	}
	for cpu, name := range r.Assets {
		if cpu != "arm64" && cpu != "x64" {
			return fmt.Errorf("sandbox.release.assets.%s is not arm64 or x64", cpu)
		}
		if !assetNameRE.MatchString(name) {
			return fmt.Errorf("sandbox.release.assets.%s %q is not a file name", cpu, name)
		}
	}
	if !assetNameRE.MatchString(r.Sums) {
		return fmt.Errorf("sandbox.release.sums %q is not a file name", r.Sums)
	}
	if !tagPrefixRE.MatchString(r.TagPrefix) {
		return fmt.Errorf("sandbox.release.tagPrefix %q is not letters", r.TagPrefix)
	}
	return nil
}

func (r *ReleaseInstall) tagFor(version string) string { return r.TagPrefix + version }

func (r *ReleaseInstall) assetURL(host, tag, file string) string {
	return "https://" + host + "/" + r.Repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(file)
}

func (s *Service) releaseHost() string {
	if s.ReleaseHost != "" {
		return s.ReleaseHost
	}
	return DefaultReleaseHost
}

// latestFromRelease reads the newest published version of a program from the release page's own feed
// (releases.atom: no key, and not the API's 60 an hour shared by every program on one address, which
// answered 403 on the first real call), with the organization's hold applied as for a package. The
// feed does not mark a pre-release, so only a tag that is a plain version (the tag prefix and x.y.z)
// counts: 1.2.3-rc.1 is never the newest.
func (s *Service) latestFromRelease(ctx context.Context, r *ReleaseInstall, holdDays int) (string, error) {
	host := s.releaseHost()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/"+r.Repo+"/releases.atom", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.releaseClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("the release page did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the release page answered %d for %s", resp.StatusCode, r.Repo)
	}
	var feed struct {
		Entries []struct {
			Title   string    `xml:"title"`
			Updated time.Time `xml:"updated"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&feed); err != nil {
		return "", fmt.Errorf("the release feed of %s could not be read: %w", r.Repo, err)
	}
	cutoff := s.now().Add(-time.Duration(holdDays) * 24 * time.Hour)
	best := ""
	for _, e := range feed.Entries {
		v, ok := strings.CutPrefix(strings.TrimSpace(e.Title), r.TagPrefix)
		if !ok || !ValidVersion(v) || strings.Contains(v, "-") || (holdDays > 0 && e.Updated.After(cutoff)) {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no release of %s with a version this component installs was found", r.Repo)
	}
	return best, nil
}

// releaseClient follows a redirect only to https, and only to the host the release host serves its
// assets from: a redirect anywhere else is a refusal, never a download.
func (s *Service) releaseClient() *http.Client {
	base := s.httpClient()
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("a redirect to %s is refused: not https", req.URL.Redacted())
		}
		if !s.releaseAssetHostOK(req.URL.Hostname()) {
			return fmt.Errorf("a redirect to %s is refused: not where %s serves its files from", req.URL.Hostname(), s.releaseHost())
		}
		return nil
	}
	return &c
}

// releaseAssetHostOK says whether a redirect target may serve a release's files. The release host
// itself and its asset host (githubusercontent.com) are the places; a deployment's mirror is its own
// host, set with ReleaseHost, and serves from itself.
func (s *Service) releaseAssetHostOK(host string) bool {
	if s.releaseAssetOK != nil {
		return s.releaseAssetOK(host)
	}
	host = strings.ToLower(host)
	own := strings.ToLower(s.releaseHost())
	if host == own || strings.HasSuffix(host, "."+own) {
		return true
	}
	return own == DefaultReleaseHost && strings.HasSuffix(host, ".githubusercontent.com")
}

// sandboxReleaseInstall puts the program in a new version folder of the sandbox's store, whole or
// not at all.
func (s *Service) sandboxReleaseInstall(ctx context.Context, op *operation, e *Entry, version string) error {
	in := e.Sandbox.Release
	asset, ok := in.Assets[guestCPU()]
	if !ok {
		return fmt.Errorf("%s publishes no build for the sandbox's CPU (%s)", e.baseName(), guestCPU())
	}
	tag := in.tagFor(version)
	host := s.releaseHost()
	op.logf("reading %s for %s", in.Sums, tag)
	sums, err := s.fetchSmall(ctx, in.assetURL(host, tag, in.Sums), maxSumsBytes)
	if err != nil {
		return fmt.Errorf("the checksums of %s %s could not be read: %w", e.baseName(), version, err)
	}
	want, err := checksumOf(sums, asset)
	if err != nil {
		return err
	}
	st := s.sandboxStore()
	final := st.versionDir(e.ID, version)
	partial := final + partialSuffix
	_ = os.RemoveAll(partial)
	bin := filepath.Join(partial, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	ok = false
	defer func() {
		if !ok {
			_ = os.RemoveAll(partial)
		}
	}()
	op.logf("downloading %s", asset)
	dest := filepath.Join(bin, e.Sandbox.command(e))
	if err := s.downloadChecked(ctx, op, in.assetURL(host, tag, asset), dest, want); err != nil {
		return err
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return err
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

// fetchSmall reads one small file over https (the checksums), bounded.
func (s *Service) fetchSmall(ctx context.Context, u string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.releaseClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("the file is larger than this component reads")
	}
	return raw, nil
}

// checksumOf finds the sha256 of file in a checksums file (`<hex>  <name>`, a leading `*` on the
// name for binary mode allowed).
func checksumOf(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			if len(f[0]) != 64 {
				return "", fmt.Errorf("the checksum of %s is not a sha256", file)
			}
			if _, err := hex.DecodeString(f[0]); err != nil {
				return "", fmt.Errorf("the checksum of %s is not a sha256", file)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("the release's checksums have no entry for %s, so the download could not be checked", file)
}

// downloadChecked downloads u to dest, hashing as it goes, and refuses a file whose sha256 is not want.
// Every chunk counts as activity.
func (s *Service) downloadChecked(ctx context.Context, op *operation, u, dest, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.releaseClient().Do(req)
	if err != nil {
		return fmt.Errorf("the download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the download answered %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxReleaseBytes {
				_ = f.Close()
				return errors.New("the download is larger than this component accepts")
			}
			h.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				_ = f.Close()
				return err
			}
			op.touch()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			return fmt.Errorf("the download failed: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return errors.New("the download does not match the release's checksum; nothing was installed")
	}
	return nil
}
