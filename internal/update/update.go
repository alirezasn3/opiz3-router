// Package update installs new versions of opiz3-router from GitHub Releases.
//
// A release carries the binary for each platform, named
// opiz3-router-<os>-<arch>, and a checksums.txt in `sha256sum` format. The
// binary is checked against that file, test-run (`<binary> -version` must
// name the release tag), and only then swapped in with a rename. The previous
// binary is kept as <exe>.old so an update can be undone.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRepo    = "alirezasn3/opiz3-router"
	DefaultAPIBase = "https://api.github.com"
	ChecksumsAsset = "checksums.txt"
	maxBinaryBytes = 200 << 20
	// BinaryName is the program's name; `-version` prints it before the version.
	BinaryName = "opiz3-router"
)

// Client talks to the GitHub releases API.
type Client struct {
	Repo    string // owner/name
	APIBase string
	HTTP    *http.Client
}

// New returns a client for repo ("" means DefaultRepo) and the GitHub API at
// apiBase ("" means GitHub itself; a test server otherwise).
func New(repo, apiBase string) *Client {
	if repo == "" {
		repo = DefaultRepo
	}
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	return &Client{Repo: repo, APIBase: strings.TrimRight(apiBase, "/"), HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type Release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Notes  string  `json:"body"`
	Assets []Asset `json:"assets"`
}

// AssetName is the binary name for this platform.
func AssetName() string {
	return fmt.Sprintf("%s-%s-%s", BinaryName, runtime.GOOS, runtime.GOARCH)
}

func (r *Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

func (c *Client) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "opiz3-router-updater")
	return c.HTTP.Do(req)
}

func (c *Client) release(ctx context.Context, path string) (*Release, error) {
	resp, e := c.get(ctx, c.APIBase+"/repos/"+c.Repo+"/releases/"+path, "application/vnd.github+json")
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("no such release in %s (is the repository public, and does it have a published release?)", c.Repo)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("GitHub refused the request (rate limit?); try again later")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel Release
	if e := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); e != nil {
		return nil, fmt.Errorf("reading release: %w", e)
	}
	if rel.Tag == "" {
		return nil, errors.New("release has no tag")
	}
	return &rel, nil
}

// Latest returns the newest published (non-prerelease) release.
func (c *Client) Latest(ctx context.Context) (*Release, error) { return c.release(ctx, "latest") }

// ByTag returns a specific release, such as "v1.2.3".
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	return c.release(ctx, "tags/"+tag)
}

// ---- versions --------------------------------------------------------------

type semver struct {
	nums [3]int
	pre  string
}

// describeBuild matches what `git describe --tags --dirty` adds to a tag for a
// build made after it (v1.0.0-3-gabc1234, v1.0.0-dirty).
var describeBuild = regexp.MustCompile(`^(\d+-g[0-9a-f]+)?(-?dirty)?$`)

// parseVersion accepts only "v"-prefixed release versions. Release tags always
// are, and the prefix keeps an all-digit commit hash from passing for a
// version. A local build is not a release, so it is refused too: it would
// otherwise rank below the tag it was made from.
func parseVersion(v string) (semver, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		return semver{}, false
	}
	v = v[1:]
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var s semver
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, s.pre = v[:i], v[i+1:]
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return s, false
	}
	for i, p := range parts {
		n, e := strconv.Atoi(p)
		if e != nil || n < 0 {
			return s, false
		}
		s.nums[i] = n
	}
	if s.pre != "" && describeBuild.MatchString(s.pre) {
		return s, false
	}
	return s, true
}

// Comparable reports whether v looks like a release version (v1.2.3). Local
// builds ("dev", a commit hash, "v1.0.0-3-gabc1234") are not.
func Comparable(v string) bool { _, ok := parseVersion(v); return ok }

// Newer reports whether candidate is a later release than current. A
// pre-release (v1.2.3-rc1) ranks below the final v1.2.3.
func Newer(current, candidate string) (bool, error) {
	c, ok := parseVersion(current)
	if !ok {
		return false, fmt.Errorf("current version %q is not a release version", current)
	}
	n, ok := parseVersion(candidate)
	if !ok {
		return false, fmt.Errorf("release tag %q is not a version", candidate)
	}
	for i := range c.nums {
		if n.nums[i] != c.nums[i] {
			return n.nums[i] > c.nums[i], nil
		}
	}
	switch {
	case c.pre == n.pre:
		return false, nil
	case c.pre != "" && n.pre == "":
		return true, nil
	case c.pre == "" && n.pre != "":
		return false, nil
	}
	return n.pre > c.pre, nil
}

// ---- installing ------------------------------------------------------------

// parseChecksums reads `sha256sum` output and returns the hash for name.
func parseChecksums(r io.Reader, name string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s has no checksum for %s", ChecksumsAsset, name)
}

func (c *Client) download(ctx context.Context, a Asset) (*http.Response, error) {
	resp, e := c.get(ctx, a.URL, "application/octet-stream")
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: %s", a.Name, resp.Status)
	}
	return resp, nil
}

// Installed describes a completed swap.
type Installed struct {
	Tag      string
	Previous string // path of the backup <exe>.old; empty when the same version was reinstalled
}

// Install downloads the release's binary for this platform, verifies it and
// replaces the file at exe with it.
func (c *Client) Install(ctx context.Context, rel *Release, exe string) (*Installed, error) {
	name := AssetName()
	bin, ok := rel.asset(name)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s binary", rel.Tag, name)
	}
	sums, ok := rel.asset(ChecksumsAsset)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s; refusing to install an unverified binary", rel.Tag, ChecksumsAsset)
	}

	resp, e := c.download(ctx, sums)
	if e != nil {
		return nil, e
	}
	want, e := parseChecksums(io.LimitReader(resp.Body, 1<<20), name)
	resp.Body.Close()
	if e != nil {
		return nil, e
	}

	// Download next to the target so the final rename stays on one filesystem.
	tmp := exe + ".new"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if e != nil {
		return nil, fmt.Errorf("cannot write next to %s (run as root?): %w", exe, e)
	}
	cleanup := func() { f.Close(); os.Remove(tmp) }

	resp, e = c.download(ctx, bin)
	if e != nil {
		cleanup()
		return nil, e
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinaryBytes+1))
	if e != nil {
		cleanup()
		return nil, fmt.Errorf("downloading %s: %w", name, e)
	}
	if n > maxBinaryBytes {
		cleanup()
		return nil, errors.New("download is unreasonably large; refusing it")
	}
	if e = f.Close(); e != nil {
		os.Remove(tmp)
		return nil, e
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return nil, fmt.Errorf("checksum mismatch for %s: download is %s, release says %s", name, got, want)
	}

	// The binary must run on this machine and name the release.
	if e = os.Chmod(tmp, 0o755); e != nil {
		os.Remove(tmp)
		return nil, e
	}
	if got := binaryVersion(ctx, tmp); got != rel.Tag {
		os.Remove(tmp)
		if got == "" {
			return nil, errors.New("the downloaded binary does not run here")
		}
		return nil, fmt.Errorf("the downloaded binary reports version %q, expected %q", got, rel.Tag)
	}

	// Reinstalling the version that is already there must not overwrite the
	// backup of the version before it.
	if binaryVersion(ctx, exe) == rel.Tag {
		if e = os.Rename(tmp, exe); e != nil {
			os.Remove(tmp)
			return nil, e
		}
		return &Installed{Tag: rel.Tag}, nil
	}
	old := exe + ".old"
	if e = os.Rename(exe, old); e != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("keeping the previous binary: %w", e)
	}
	if e = os.Rename(tmp, exe); e != nil {
		_ = os.Rename(old, exe) // put it back
		os.Remove(tmp)
		return nil, e
	}
	return &Installed{Tag: rel.Tag, Previous: old}, nil
}

// binaryVersion asks a binary for its version ("" if it can't say).
func binaryVersion(ctx context.Context, path string) string {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, e := exec.CommandContext(cctx, path, "-version").Output()
	if e != nil {
		return ""
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), BinaryName+" ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// Rollback swaps the previous binary (<exe>.old) back in.
func Rollback(exe string) error {
	old := exe + ".old"
	if _, e := os.Stat(old); e != nil {
		return fmt.Errorf("no previous version to go back to (%s is missing)", old)
	}
	swap := exe + ".swap"
	if e := os.Rename(exe, swap); e != nil {
		return e
	}
	if e := os.Rename(old, exe); e != nil {
		_ = os.Rename(swap, exe)
		return e
	}
	return os.Rename(swap, old) // so rolling back twice returns to the newer one
}

// Executable is the real path of the running binary, symlinks resolved.
func Executable() (string, error) {
	exe, e := os.Executable()
	if e != nil {
		return "", e
	}
	if real, e := filepath.EvalSymlinks(exe); e == nil {
		exe = real
	}
	return exe, nil
}

// UnderSystemd reports whether this process was started by systemd.
func UnderSystemd() bool { return os.Getenv("INVOCATION_ID") != "" }

// RestartService restarts the systemd unit. With noBlock it only queues the
// restart and returns, which is what a service restarting itself needs.
func RestartService(name string, noBlock bool) error {
	args := []string{"restart"}
	if noBlock {
		args = append(args, "--no-block")
	}
	out, e := exec.Command("systemctl", append(args, name)...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("systemctl restart %s: %v: %s", name, e, strings.TrimSpace(string(out)))
	}
	return nil
}
