package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opiz3-router/internal/config"
)

// fakeRunner records the commands and answers those with a canned reply: the
// first key that is a prefix of the command line wins.
type fakeRunner struct {
	calls   []string
	replies map[string]string
	fail    map[string]error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	for prefix, e := range f.fail {
		if strings.HasPrefix(line, prefix) {
			return "", e
		}
	}
	for prefix, out := range f.replies {
		if strings.HasPrefix(line, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeRunner) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	d := t.TempDir()
	return Paths{
		BootEnv:       filepath.Join(d, "boot", "orangepiEnv.txt"),
		Hostapd:       filepath.Join(d, "hostapd", "hostapd.conf"),
		Dnsmasq:       filepath.Join(d, "dnsmasq.d", "br0.conf"),
		SystemdDir:    filepath.Join(d, "systemd"),
		NetplanBackup: filepath.Join(d, "backup"),
		WireGuardDir:  filepath.Join(d, "wireguard"),
		PolicyRC:      filepath.Join(d, "policy-rc.d"),
		NetworkdConf:  filepath.Join(d, "networkd.conf.d", "opiz3-router.conf"),
	}
}

func TestRenderHostapd(t *testing.T) {
	cfg := config.Default()
	cfg.WiFi.Passphrase = "correct horse"
	got := RenderHostapd(cfg)
	for _, want := range []string{"interface=wlan0\n", "bridge=br0\n", "ssid=OrangePiZero3\n", "hw_mode=g\n", "channel=6\n", "wpa_passphrase=correct horse\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	cfg.WiFi.Channel = 36
	if got = RenderHostapd(cfg); !strings.Contains(got, "hw_mode=a\n") {
		t.Errorf("5 GHz channel should use hw_mode a:\n%s", got)
	}
}

func TestRenderDnsmasq(t *testing.T) {
	got, e := RenderDnsmasq(config.Default())
	if e != nil {
		t.Fatal(e)
	}
	want := "interface=br0\nbind-interfaces\ndhcp-range=172.16.0.2,172.16.0.99,255.240.0.0,24h\ndhcp-option=option:dns-server,1.1.1.1,8.8.8.8\nport=0\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("got:\n%s\nwant ending:\n%s", got, want)
	}
}

func TestWriteAPOnlyReportsChanges(t *testing.T) {
	p := testPaths(t)
	cfg := config.Default()
	ch, e := WriteAP(cfg, p)
	if e != nil {
		t.Fatal(e)
	}
	if ch.Hostapd {
		t.Error("hostapd was configured without a passphrase")
	}
	if !ch.Dnsmasq {
		t.Error("dnsmasq was not written")
	}
	cfg.WiFi.Passphrase = "correct horse"
	ch, _ = WriteAP(cfg, p)
	if !ch.Hostapd || ch.Dnsmasq {
		t.Fatalf("changes = %+v", ch)
	}
	if info, _ := os.Stat(p.Hostapd); info.Mode().Perm() != 0o600 {
		t.Errorf("hostapd.conf mode = %v; it holds the passphrase", info.Mode().Perm())
	}
	if ch, _ = WriteAP(cfg, p); ch.Hostapd || ch.Dnsmasq {
		t.Fatalf("rewriting identical files reported %+v", ch)
	}
}

func TestAPUpdate(t *testing.T) {
	p := testPaths(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.WiFi.Passphrase = "correct horse"
	ap := NewAP(cfg, cfgPath)
	ap.paths = p
	run := &fakeRunner{}
	ap.run = run

	s := ap.Settings()
	if !s.PassphraseSet || s.Passphrase != "" {
		t.Fatalf("settings leak or lack the passphrase: %+v", s)
	}
	s.SSID, s.Channel = "Home", 11
	if e := ap.Update(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if got, _ := os.ReadFile(p.Hostapd); !strings.Contains(string(got), "ssid=Home\n") || !strings.Contains(string(got), "wpa_passphrase=correct horse\n") {
		t.Fatalf("hostapd.conf:\n%s", got)
	}
	if len(run.calls) != 2 || !strings.Contains(run.calls[0], "restart --no-block hostapd") || !strings.Contains(run.calls[1], "dnsmasq") {
		t.Fatalf("first write should restart both: %v", run.calls)
	}
	run.calls = nil
	s.SSID = "Home2"
	if e := ap.Update(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if len(run.calls) != 1 || !strings.Contains(run.calls[0], "hostapd") {
		t.Fatalf("an SSID change should only restart hostapd: %v", run.calls)
	}
	saved, e := config.Load(cfgPath)
	if e != nil || saved.WiFi.SSID != "Home2" || saved.WiFi.Passphrase != "correct horse" {
		t.Fatalf("saved config: %+v, %v", saved.WiFi, e)
	}

	s.Passphrase = "short"
	if e := ap.Update(context.Background(), s); e == nil {
		t.Fatal("accepted a 5 character passphrase")
	}
	s.Passphrase = ""
	s.DHCPEnd = "10.0.0.5"
	if e := ap.Update(context.Background(), s); e == nil {
		t.Fatal("accepted a DHCP range outside the bridge subnet")
	}
	if got := ap.Settings(); got.SSID != "Home2" || got.DHCPEnd != "172.16.0.99" {
		t.Fatalf("a rejected update changed the settings: %+v", got)
	}
}

func TestAPUpdateKeepsOtherSettingsChangedMeanwhile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.WiFi.Passphrase = "correct horse"
	if e := cfg.Save(cfgPath); e != nil {
		t.Fatal(e)
	}
	ap := NewAP(cfg, cfgPath)
	ap.paths, ap.run = testPaths(t), &fakeRunner{}

	// Another card changes the file after the router started.
	if _, e := config.Modify(cfgPath, func(c *config.Config) { c.MiniNetMan.Enforce = false; c.Update.AutoCheck = false }); e != nil {
		t.Fatal(e)
	}
	s := ap.Settings()
	s.SSID = "Home"
	if e := ap.Update(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	saved, e := config.Load(cfgPath)
	if e != nil {
		t.Fatal(e)
	}
	if saved.WiFi.SSID != "Home" || saved.MiniNetMan.Enforce || saved.Update.AutoCheck {
		t.Fatalf("saved = ssid %q, enforce %v, auto check %v", saved.WiFi.SSID, saved.MiniNetMan.Enforce, saved.Update.AutoCheck)
	}
}
