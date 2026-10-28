package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"opiz3-router/internal/config"
)

type testInstall struct {
	*Installer
	run *fakeRunner
	dir string
}

func newTestInstaller(t *testing.T) *testInstall {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Network.NetplanFile = filepath.Join(dir, "netplan", "opiz3-router.yaml")
	cfg.MiniNetMan.Dir = filepath.Join(dir, "mnm")
	p := testPaths(t)
	run := &fakeRunner{replies: map[string]string{}}
	in := NewInstaller(cfg, filepath.Join(dir, "config.json"), &bytes.Buffer{})
	in.Paths, in.Run = p, run
	return &testInstall{in, run, dir}
}

func TestInstallPackagesInstallsOnlyWhatIsMissing(t *testing.T) {
	ti := newTestInstaller(t)
	ti.run.replies["dpkg-query -W -f=${db:Status-Status} iw"] = "installed"
	ti.run.replies["dpkg-query -W -f=${db:Status-Status} nftables"] = "installed"
	// While apt runs, services must be kept from starting.
	ti.run.replies["env DEBIAN_FRONTEND"] = ""
	if e := ti.installPackages(context.Background()); e != nil {
		t.Fatal(e)
	}
	var install string
	for _, c := range ti.run.calls {
		if strings.HasPrefix(c, "env ") {
			install = c
		}
	}
	if install != "env DEBIAN_FRONTEND=noninteractive apt-get install -y wireguard-tools hostapd dnsmasq netplan.io" {
		t.Fatalf("install command = %q", install)
	}
	if _, e := os.Stat(ti.Paths.PolicyRC); !os.IsNotExist(e) {
		t.Fatal("policy-rc.d was left behind and would stop every service from starting")
	}
}

func TestInstallPackagesDoesNothingWhenInstalled(t *testing.T) {
	ti := newTestInstaller(t)
	for _, p := range packages {
		ti.run.replies["dpkg-query -W -f=${db:Status-Status} "+p] = "installed"
	}
	if e := ti.installPackages(context.Background()); e != nil {
		t.Fatal(e)
	}
	if ti.run.ran("env ") || ti.run.ran("apt-get") {
		t.Fatalf("ran apt: %v", ti.run.calls)
	}
}

func TestEnableDisplayOverlayAsksForReboot(t *testing.T) {
	ti := newTestInstaller(t)
	if e := os.MkdirAll(filepath.Dir(ti.Paths.BootEnv), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(ti.Paths.BootEnv, []byte("rootdev=UUID=abc\n"), 0o644); e != nil {
		t.Fatal(e)
	}
	if e := ti.enableDisplayOverlay(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !ti.RebootNeeded {
		t.Fatal("no reboot asked for after changing the boot file")
	}
	got, _ := os.ReadFile(ti.Paths.BootEnv)
	if !strings.Contains(string(got), "rootdev=UUID=abc\n") || !strings.Contains(string(got), "overlays=spi1-cs0-spidev\n") {
		t.Fatalf("boot file:\n%s", got)
	}
	ti.RebootNeeded = false
	if e := ti.enableDisplayOverlay(context.Background()); e != nil || ti.RebootNeeded {
		t.Fatalf("second run: reboot=%v err=%v", ti.RebootNeeded, e)
	}
}

func TestEnsureConfigGeneratesPassphraseOnce(t *testing.T) {
	ti := newTestInstaller(t)
	if e := ti.ensureConfig(context.Background()); e != nil {
		t.Fatal(e)
	}
	saved, e := config.Load(ti.CfgPath)
	if e != nil {
		t.Fatal(e)
	}
	if len(saved.WiFi.Passphrase) != 16 || saved.WiFi.Passphrase != ti.Cfg.WiFi.Passphrase {
		t.Fatalf("passphrase = %q", saved.WiFi.Passphrase)
	}
	if info, _ := os.Stat(ti.CfgPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
	first := saved.WiFi.Passphrase
	ti.Notes = nil
	if e = ti.ensureConfig(context.Background()); e != nil || len(ti.Notes) != 0 || ti.Cfg.WiFi.Passphrase != first {
		t.Fatalf("second run changed things: %v %v", ti.Notes, e)
	}
}

func TestConfigureAP(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Cfg.WiFi.Passphrase = "correct horse"
	if e := ti.configureAP(context.Background()); e != nil {
		t.Fatal(e)
	}
	drop, e := os.ReadFile(filepath.Join(ti.Paths.SystemdDir, "hostapd.service.d", "opiz3-router.conf"))
	if e != nil {
		t.Fatal(e)
	}
	// systemd would expand a single $ itself.
	if !strings.Contains(string(drop), "$$(seq 1 60)") || !strings.Contains(string(drop), "dev br0") || !strings.Contains(string(drop), "After=mini-net-man.service") {
		t.Fatalf("drop-in:\n%s", drop)
	}
	for _, want := range []string{"systemctl unmask hostapd", "systemctl enable hostapd dnsmasq", "systemctl restart --no-block hostapd"} {
		if !ti.run.ran(want) {
			t.Errorf("did not run %q: %v", want, ti.run.calls)
		}
	}
	ti.run.calls = nil
	if e = ti.configureAP(context.Background()); e != nil {
		t.Fatal(e)
	}
	if ti.run.ran("systemctl restart") || ti.run.ran("systemctl daemon-reload") {
		t.Fatalf("second run restarted things: %v", ti.run.calls)
	}
}

func TestConfigureWireGuard(t *testing.T) {
	ti := newTestInstaller(t)
	if e := ti.configureWireGuard(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(ti.Notes) != 1 || !strings.Contains(ti.Notes[0], "wg0.conf") || ti.run.ran("systemctl enable") {
		t.Fatalf("without a config: notes %v, calls %v", ti.Notes, ti.run.calls)
	}

	ti.Notes = nil
	if e := os.MkdirAll(ti.Paths.WireGuardDir, 0o755); e != nil {
		t.Fatal(e)
	}
	conf := filepath.Join(ti.Paths.WireGuardDir, "wg0.conf")
	if e := os.WriteFile(conf, []byte("[Interface]\nPrivateKey = x\nAddress = 10.0.0.2/32\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e := ti.configureWireGuard(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(ti.Notes) != 1 || !strings.Contains(ti.Notes[0], "Table = off") || !ti.run.ran("systemctl enable --now wg-quick@wg0") {
		t.Fatalf("with a config: notes %v, calls %v", ti.Notes, ti.run.calls)
	}

	ti.Notes = nil
	if e := os.WriteFile(conf, []byte("[Interface]\nTable = off\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e := ti.configureWireGuard(context.Background()); e != nil || len(ti.Notes) != 0 {
		t.Fatalf("notes %v, err %v", ti.Notes, e)
	}
}

func TestConfigureNetwork(t *testing.T) {
	ti := newTestInstaller(t)
	netplanDir := filepath.Dir(ti.Cfg.Network.NetplanFile)
	if e := os.MkdirAll(netplanDir, 0o755); e != nil {
		t.Fatal(e)
	}
	old := filepath.Join(netplanDir, "10-orangepi.yaml")
	if e := os.WriteFile(old, []byte("network: {renderer: NetworkManager}\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	ti.run.replies["systemctl list-unit-files NetworkManager.service"] = "NetworkManager.service enabled enabled"

	if e := ti.configureNetwork(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(old); !os.IsNotExist(e) {
		t.Fatal("the image's netplan file is still there and would override the router's")
	}
	if _, e := os.Stat(filepath.Join(ti.Paths.NetplanBackup, "10-orangepi.yaml")); e != nil {
		t.Fatal("the image's netplan file was not kept:", e)
	}
	got, _ := os.ReadFile(ti.Cfg.Network.NetplanFile)
	if !strings.Contains(string(got), "dhcp4: true") || strings.Contains(string(got), "bridges:") {
		t.Fatalf("fallback netplan:\n%s", got)
	}
	if conf, _ := os.ReadFile(ti.Paths.NetworkdConf); !strings.Contains(string(conf), "ManageForeignRoutingPolicyRules=no") {
		t.Fatalf("networkd drop-in:\n%s", conf)
	}
	// Generate to check the file, stop NetworkManager, and only then apply.
	order := map[string]int{}
	for i, c := range ti.run.calls {
		order[c] = i
	}
	gen, nm, apply := order["netplan generate"], order["systemctl disable --now NetworkManager.service"], order["netplan apply"]
	if !(gen < nm && nm < apply) || !ti.run.ran("systemctl mask NetworkManager.service") || !ti.run.ran("systemctl enable systemd-networkd") {
		t.Fatalf("calls: %v", ti.run.calls)
	}

	// Done once, left alone afterwards, including a LAN-mode file.
	if e := os.WriteFile(ti.Cfg.Network.NetplanFile, []byte("# lan mode\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	ti.run.calls = nil
	ti.run.replies["systemctl list-unit-files NetworkManager.service"] = "NetworkManager.service masked masked"
	if e := ti.configureNetwork(context.Background()); e != nil {
		t.Fatal(e)
	}
	if again, _ := os.ReadFile(ti.Cfg.Network.NetplanFile); string(again) != "# lan mode\n" {
		t.Fatalf("second run overwrote the netplan file: %s", again)
	}
	if ti.run.ran("netplan apply") {
		t.Fatalf("second run applied the network config again: %v", ti.run.calls)
	}
}

func TestConfigureNetworkPutsFilesBackWhenNetplanRejectsTheNewOne(t *testing.T) {
	ti := newTestInstaller(t)
	netplanDir := filepath.Dir(ti.Cfg.Network.NetplanFile)
	if e := os.MkdirAll(netplanDir, 0o755); e != nil {
		t.Fatal(e)
	}
	old := filepath.Join(netplanDir, "10-orangepi.yaml")
	if e := os.WriteFile(old, []byte("network: {}\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	ti.run.fail = map[string]error{"netplan generate": errors.New("invalid")}
	if e := ti.configureNetwork(context.Background()); e == nil {
		t.Fatal("expected an error")
	}
	if _, e := os.Stat(old); e != nil {
		t.Fatal("the image's netplan file was not put back:", e)
	}
	if ti.run.ran("systemctl disable") {
		t.Fatal("NetworkManager was disabled although the new network config is invalid")
	}
}

// fakeMiniNetMan answers the API calls the installer makes.
func fakeMiniNetMan(t *testing.T, password string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/login":
			if body["password"] != password {
				http.Error(w, `{"error":"wrong"}`, http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s"})
			fmt.Fprint(w, `{"user":"admin"}`)
		case "/api/tokens":
			if _, e := r.Cookie("session"); e != nil || body["password"] != password || body["name"] != "opiz3-router" || body["read_only"] != false {
				http.Error(w, `{"error":"bad request"}`, http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"token":"mnm_secret","id":"1"}`)
		case "/api/me":
			if r.Header.Get("Authorization") != "Bearer mnm_secret" {
				http.Error(w, `{"error":"invalid API token"}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"user":"token:opiz3-router"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallMiniNetMan(t *testing.T) {
	ti := newTestInstaller(t)
	srv := fakeMiniNetMan(t, "pw123456")
	ti.Cfg.MiniNetMan.URL = srv.URL
	local := filepath.Join(ti.dir, "mnm-local")
	if e := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); e != nil {
		t.Fatal(e)
	}
	ti.MiniNetManBinary = local
	bin := filepath.Join(ti.Cfg.MiniNetMan.Dir, "mini-net-man")
	ti.run.replies[bin+" install"] = "\nCreated config.yaml\n  login:    admin / pw123456   (change it)\n"

	if e := ti.installMiniNetMan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !ti.run.ran(bin + " install -allow 172.16.0.0/12 -port ") {
		t.Fatalf("calls = %v", ti.run.calls)
	}
	saved, e := config.Load(ti.CfgPath)
	if e != nil || saved.MiniNetMan.Token != "mnm_secret" {
		t.Fatalf("token not saved: %+v %v", saved.MiniNetMan, e)
	}

	// With a working token nothing more is needed, and the login is not asked for.
	// (The real install has made config.yaml by now.)
	if e = os.WriteFile(filepath.Join(ti.Cfg.MiniNetMan.Dir, "config.yaml"), nil, 0o600); e != nil {
		t.Fatal(e)
	}
	ti.run.calls = nil
	ti.run.replies[bin+" install"] = "\nUsing existing config\n"
	ti.Notes = nil
	if e = ti.installMiniNetMan(context.Background()); e != nil || len(ti.Notes) != 0 {
		t.Fatalf("second run: %v, notes %v", e, ti.Notes)
	}
	if ti.run.ran(bin + " install -allow") {
		t.Fatalf("second run passed first-install flags: %v", ti.run.calls)
	}
}

func TestInstallMiniNetManWithoutAToken(t *testing.T) {
	ti := newTestInstaller(t)
	srv := fakeMiniNetMan(t, "pw123456")
	ti.Cfg.MiniNetMan.URL = srv.URL
	bin := filepath.Join(ti.Cfg.MiniNetMan.Dir, "mini-net-man")
	if e := os.MkdirAll(ti.Cfg.MiniNetMan.Dir, 0o755); e != nil {
		t.Fatal(e)
	}
	for _, f := range []string{bin, filepath.Join(ti.Cfg.MiniNetMan.Dir, "config.yaml")} {
		if e := os.WriteFile(f, nil, 0o755); e != nil {
			t.Fatal(e)
		}
	}
	// Already installed earlier: the login is gone, so the person is told what to do.
	ti.run.replies[bin+" install"] = "\nUsing existing config\n"
	if e := ti.installMiniNetMan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(ti.Notes) != 1 || !strings.Contains(ti.Notes[0], "mini_net_man.token") {
		t.Fatalf("notes = %v", ti.Notes)
	}
}

func TestDownloadMiniNetManChecksArchiveAgainstChecksums(t *testing.T) {
	asset := "mini-net-man-linux-" + runtime.GOARCH
	binary := []byte("binary contents")
	sum := sha256.Sum256(binary)
	good := hex.EncodeToString(sum[:])
	for name, tc := range map[string]struct {
		checksum string
		wantErr  string
	}{
		"matches":  {good, ""},
		"mismatch": {strings.Repeat("0", 64), "checksum"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/checksums.txt":
				fmt.Fprintf(w, "%s  other-file\n%s  %s\n", strings.Repeat("1", 64), tc.checksum, asset)
			case "/" + asset:
				w.Write(binary)
			default:
				http.NotFound(w, r)
			}
		}))
		ti := newTestInstaller(t)
		ti.ReleaseBase = srv.URL
		bin := filepath.Join(ti.dir, "mini-net-man")
		e := ti.downloadMiniNetMan(context.Background(), bin)
		srv.Close()
		if tc.wantErr == "" {
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			if got, _ := os.ReadFile(bin); string(got) != string(binary) {
				t.Fatalf("%s: binary = %q", name, got)
			}
			if info, _ := os.Stat(bin); info.Mode().Perm()&0o100 == 0 {
				t.Fatalf("%s: binary is not executable", name)
			}
			continue
		}
		if e == nil || !strings.Contains(e.Error(), tc.wantErr) {
			t.Fatalf("%s: error = %v", name, e)
		}
		if _, e := os.Stat(bin); !os.IsNotExist(e) {
			t.Fatalf("%s: a binary that failed its checksum was installed", name)
		}
	}
}
