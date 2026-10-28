package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	for _, tc := range []struct {
		cur, cand string
		want      bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.9.0", "v1.10.0", true},
		{"v1.0.0-rc1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc1", false},
		{"v1.0.0-rc1", "v1.0.0-rc2", true},
		{"v1", "v1.0.1", true},
	} {
		got, e := Newer(tc.cur, tc.cand)
		if e != nil || got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, %v; want %v", tc.cur, tc.cand, got, e, tc.want)
		}
	}
	// A local build is not a release and must not be compared with one.
	for _, v := range []string{"dev", "", "1.0.0", "3dcec8b", "v1.0.0-3-g3dcec8b", "v1.0.0-3-g3dcec8b-dirty", "v1.0.0-dirty", "v1.2.3.4", "vx"} {
		if Comparable(v) {
			t.Errorf("%q passes for a release version", v)
		}
		if _, e := Newer(v, "v9.9.9"); e == nil {
			t.Errorf("Newer(%q, ...) did not fail", v)
		}
	}
	if !Comparable("v1.2.3") || !Comparable("v1.0.0-rc1") {
		t.Error("release versions refused")
	}
}

func TestParseChecksums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	in := sum + "  other\n" + strings.Repeat("CD", 32) + " *wanted\n"
	got, e := parseChecksums(strings.NewReader(in), "wanted")
	if e != nil || got != strings.Repeat("cd", 32) {
		t.Fatalf("got %q, %v", got, e)
	}
	if _, e = parseChecksums(strings.NewReader(in), "missing"); e == nil {
		t.Fatal("expected an error")
	}
	if _, e = parseChecksums(strings.NewReader("short  wanted\n"), "wanted"); e == nil {
		t.Fatal("accepted a malformed hash")
	}
}

// script is a "binary" that reports a version, so the tests run anywhere.
func script(version string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\necho 'opiz3-router %s'\n", version))
}

type fakeGitHub struct {
	*httptest.Server
	releases map[string]*Release // by tag, and "latest"
	files    map[string][]byte   // asset path -> contents
}

// newFakeGitHub serves a release for each tag with the platform's binary and
// a checksums.txt built from them. A tag in tampered gets a wrong checksum.
func newFakeGitHub(t *testing.T, binaries map[string][]byte, tampered ...string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{releases: map[string]*Release{}, files: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			data, ok := f.files[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/owner/name/releases/")
		tag = strings.TrimPrefix(tag, "tags/")
		rel, ok := f.releases[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(rel)
	}))
	t.Cleanup(f.Close)

	latest := ""
	for tag, bin := range binaries {
		sum := sha256.Sum256(bin)
		hash := hex.EncodeToString(sum[:])
		for _, bad := range tampered {
			if bad == tag {
				hash = strings.Repeat("0", 64)
			}
		}
		name := AssetName()
		sums := []byte(hash + "  " + name + "\n")
		rel := &Release{Tag: tag, URL: f.URL + "/release/" + tag, Notes: "notes for " + tag}
		add := func(file string, data []byte) {
			rel.Assets = append(rel.Assets, Asset{Name: file, URL: f.URL + "/dl/" + tag + "/" + file, Size: int64(len(data))})
			f.files["/dl/"+tag+"/"+file] = data
		}
		add(name, bin)
		add(ChecksumsAsset, sums)
		f.releases[tag] = rel
		if latest == "" || tag > latest {
			latest = tag
		}
	}
	if latest != "" {
		f.releases["latest"] = f.releases[latest]
	}
	return f
}

func (f *fakeGitHub) client() *Client { return New("owner/name", f.URL) }

func installedExe(t *testing.T, version string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "opiz3-router")
	if e := os.WriteFile(exe, script(version), 0o755); e != nil {
		t.Fatal(e)
	}
	return exe
}

func version(t *testing.T, path string) string {
	t.Helper()
	return binaryVersion(context.Background(), path)
}

func TestInstallReplacesTheBinaryAndKeepsTheOldOne(t *testing.T) {
	gh := newFakeGitHub(t, map[string][]byte{"v1.1.0": script("v1.1.0")})
	exe := installedExe(t, "v1.0.0")
	c := gh.client()
	rel, e := c.Latest(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	done, e := c.Install(context.Background(), rel, exe)
	if e != nil {
		t.Fatal(e)
	}
	if done.Tag != "v1.1.0" || done.Previous != exe+".old" {
		t.Fatalf("installed = %+v", done)
	}
	if version(t, exe) != "v1.1.0" || version(t, exe+".old") != "v1.0.0" {
		t.Fatalf("versions: new %q, old %q", version(t, exe), version(t, exe+".old"))
	}
	if _, e = os.Stat(exe + ".new"); !os.IsNotExist(e) {
		t.Fatal("the download was left behind")
	}
	if info, _ := os.Stat(exe); info.Mode().Perm()&0o111 == 0 {
		t.Fatal("the new binary is not executable")
	}

	// Installing the same version again keeps the backup of the one before.
	if done, e = c.Install(context.Background(), rel, exe); e != nil || done.Previous != "" {
		t.Fatalf("reinstall = %+v, %v", done, e)
	}
	if version(t, exe+".old") != "v1.0.0" {
		t.Fatalf("the backup was overwritten by a reinstall: %q", version(t, exe+".old"))
	}

	if e = Rollback(exe); e != nil {
		t.Fatal(e)
	}
	if version(t, exe) != "v1.0.0" || version(t, exe+".old") != "v1.1.0" {
		t.Fatalf("after rollback: %q, old %q", version(t, exe), version(t, exe+".old"))
	}
	if e = Rollback(exe); e != nil || version(t, exe) != "v1.1.0" {
		t.Fatalf("second rollback: %v, %q", e, version(t, exe))
	}
	if e = Rollback(filepath.Join(t.TempDir(), "none")); e == nil {
		t.Fatal("rolled back with nothing to go back to")
	}
}

func TestInstallRefusesWhatItCannotTrust(t *testing.T) {
	exe := installedExe(t, "v1.0.0")
	for name, tc := range map[string]struct {
		binaries map[string][]byte
		tampered []string
		mutate   func(*fakeGitHub)
		wantErr  string
	}{
		"checksum mismatch": {map[string][]byte{"v1.1.0": script("v1.1.0")}, []string{"v1.1.0"}, nil, "checksum mismatch"},
		"wrong version":     {map[string][]byte{"v1.1.0": script("v1.0.5")}, nil, nil, `reports version "v1.0.5"`},
		"does not run":      {map[string][]byte{"v1.1.0": []byte("not a program")}, nil, nil, "does not run"},
		"no checksums": {map[string][]byte{"v1.1.0": script("v1.1.0")}, nil, func(f *fakeGitHub) {
			r := f.releases["v1.1.0"]
			r.Assets = r.Assets[:1]
		}, "refusing to install an unverified binary"},
		"no binary for this platform": {map[string][]byte{"v1.1.0": script("v1.1.0")}, nil, func(f *fakeGitHub) {
			r := f.releases["v1.1.0"]
			r.Assets = r.Assets[1:]
		}, "has no " + AssetName()},
	} {
		gh := newFakeGitHub(t, tc.binaries, tc.tampered...)
		if tc.mutate != nil {
			tc.mutate(gh)
		}
		c := gh.client()
		rel, _ := c.Latest(context.Background())
		_, e := c.Install(context.Background(), rel, exe)
		if e == nil || !strings.Contains(e.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, e, tc.wantErr)
		}
		if version(t, exe) != "v1.0.0" {
			t.Errorf("%s: the installed binary was replaced by one that failed a check", name)
		}
		for _, leftover := range []string{exe + ".new", exe + ".old"} {
			if _, e := os.Stat(leftover); !os.IsNotExist(e) {
				t.Errorf("%s: %s left behind", name, leftover)
			}
		}
	}
}

func TestMissingRelease(t *testing.T) {
	gh := newFakeGitHub(t, nil)
	if _, e := gh.client().Latest(context.Background()); e == nil || !strings.Contains(e.Error(), "is the repository public") {
		t.Fatalf("error = %v", e)
	}
}

func run(t *testing.T, gh *fakeGitHub, current string, mutate func(*Options)) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o := Options{Current: current, Repo: "owner/name", APIBase: gh.URL, Service: "opiz3-router", NoRestart: true, Out: &out, Exe: installedExe(t, current)}
	if mutate != nil {
		mutate(&o)
	}
	e := Run(context.Background(), o)
	return out.String(), e
}

func TestRun(t *testing.T) {
	gh := newFakeGitHub(t, map[string][]byte{"v1.0.0": script("v1.0.0"), "v1.1.0": script("v1.1.0")})

	out, e := run(t, gh, "v1.1.0", nil)
	if e != nil || !strings.Contains(out, "Up to date (v1.1.0)") {
		t.Fatalf("up to date: %q, %v", out, e)
	}

	var exe string
	out, e = run(t, gh, "v1.0.0", func(o *Options) { exe = o.Exe })
	if e != nil || !strings.Contains(out, "Installed v1.1.0") || !strings.Contains(out, "-rollback") || !strings.Contains(out, "-install") {
		t.Fatalf("update: %q, %v", out, e)
	}
	if version(t, exe) != "v1.1.0" {
		t.Fatalf("version after update = %q", version(t, exe))
	}
	if !strings.Contains(out, "systemctl restart opiz3-router") {
		t.Fatalf("no restart hint: %q", out)
	}

	out, e = run(t, gh, "v1.0.0", func(o *Options) { o.Check = true; exe = o.Exe })
	if e != nil || !strings.Contains(out, "Update available: v1.0.0 -> v1.1.0") || !strings.Contains(out, "notes for v1.1.0") || version(t, exe) != "v1.0.0" {
		t.Fatalf("check: %q, %v", out, e)
	}

	// A local build can't tell, unless forced.
	out, e = run(t, gh, "v1.1.0-2-gabc1234", nil)
	if e == nil || !strings.Contains(e.Error(), "-force") {
		t.Fatalf("dev build: %q, %v", out, e)
	}
	out, e = run(t, gh, "dev", func(o *Options) { o.Check = true })
	if e != nil || !strings.Contains(out, "no release version") {
		t.Fatalf("dev check: %q, %v", out, e)
	}
	out, e = run(t, gh, "dev", func(o *Options) { o.Force = true; exe = o.Exe })
	if e != nil || version(t, exe) != "v1.1.0" {
		t.Fatalf("forced: %q, %v", out, e)
	}

	// A specific version, which may be older.
	out, e = run(t, gh, "v1.1.0", func(o *Options) { o.Version = "v1.0.0"; exe = o.Exe })
	if e != nil || version(t, exe) != "v1.0.0" {
		t.Fatalf("downgrade: %q, %v", out, e)
	}
	if _, e = run(t, gh, "v1.1.0", func(o *Options) { o.Version = "v9.9.9" }); e == nil {
		t.Fatal("installed a release that does not exist")
	}

	out, e = run(t, gh, "v1.1.0", func(o *Options) { o.Rollback = true })
	if e == nil || !strings.Contains(e.Error(), "no previous version") {
		t.Fatalf("rollback without a backup: %q, %v", out, e)
	}
}
