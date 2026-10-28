package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, e := Load(filepath.Join(t.TempDir(), "missing.json"))
	if e != nil {
		t.Fatal(e)
	}
	if c.Interfaces.Bridge != "br0" || c.WireGuard.Table != 200 || !c.WireGuard.KillSwitch {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if e = Default().Validate(); e != nil {
		t.Fatalf("defaults do not validate: %v", e)
	}
}

func TestLoadOverridesOnlyGivenFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if e := os.WriteFile(p, []byte(`{"interfaces":{"usb_modem":"usb0"},"wireguard":{"kill_switch":false},"panel":{"listen":"172.16.0.1:8080"}}`), 0o600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if c.Interfaces.USBModem != "usb0" || c.Interfaces.Ethernet != "end0" {
		t.Fatalf("interfaces not merged: %+v", c.Interfaces)
	}
	if c.WireGuard.KillSwitch || c.WireGuard.Table != 200 {
		t.Fatalf("wireguard not merged: %+v", c.WireGuard)
	}
	if port, _ := c.PanelPort(); port != 8080 {
		t.Fatalf("port = %d, want 8080", port)
	}
	if len(c.Panel.AllowedInterfaces) != 1 || c.Panel.AllowedInterfaces[0] != "br0" {
		t.Fatalf("allowed interfaces lost: %v", c.Panel.AllowedInterfaces)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"reserved table":   func(c *Config) { c.WireGuard.Table = 254 },
		"empty bridge":     func(c *Config) { c.Interfaces.Bridge = "" },
		"bad listen":       func(c *Config) { c.Panel.Listen = "172.16.0.1" },
		"bad port":         func(c *Config) { c.Panel.Listen = "172.16.0.1:99999" },
		"relative netplan": func(c *Config) { c.Network.NetplanFile = "opiz3.yaml" },
		"bad bridge addr":  func(c *Config) { c.Network.BridgeAddress = "172.16.0.1" },
		"bad dns":          func(c *Config) { c.Network.DNS = []string{"one.one"} },
		"odd iface name":   func(c *Config) { c.Interfaces.Ethernet = "end0\n  evil: yes" },
	} {
		c := Default()
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestExampleConfigIsValidAndMatchesDefaults(t *testing.T) {
	c, e := Load("../../deploy/config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	// The example lists every setting at its default, so a field missing from
	// it (or a changed default) shows up here.
	if got, want := fmt.Sprintf("%+v", c), fmt.Sprintf("%+v", Default()); got != want {
		t.Fatalf("example differs from the defaults:\n got %s\nwant %s", got, want)
	}
}

func TestSaveRoundTripsAndKeepsSecretsPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "etc", "config.json")
	c := Default()
	c.WiFi.Passphrase = "correct horse"
	c.MiniNetMan.Token = "mnm_secret"
	if e := c.Save(p); e != nil {
		t.Fatal(e)
	}
	got, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if got.WiFi.Passphrase != "correct horse" || got.MiniNetMan.Token != "mnm_secret" {
		t.Fatalf("lost values: %+v %+v", got.WiFi, got.MiniNetMan)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestValidateRejectsBadLANAndWiFi(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"dhcp outside subnet": func(c *Config) { c.Network.DHCPEnd = "10.0.0.5" },
		"dhcp reversed":       func(c *Config) { c.Network.DHCPStart, c.Network.DHCPEnd = "172.16.0.99", "172.16.0.2" },
		"empty ssid":          func(c *Config) { c.WiFi.SSID = "" },
		"newline in ssid":     func(c *Config) { c.WiFi.SSID = "a\nwpa=0" },
		"short passphrase":    func(c *Config) { c.WiFi.Passphrase = "short" },
		"bad channel":         func(c *Config) { c.WiFi.Channel = 0 },
		"bad health target":   func(c *Config) { c.WireGuard.HealthTarget = "one.one" },
		"bad mnm url":         func(c *Config) { c.MiniNetMan.URL = "127.0.0.1:8080" },
		"relative mnm dir":    func(c *Config) { c.MiniNetMan.Dir = "mini-net-man" },
	} {
		c := Default()
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestMiniNetManPort(t *testing.T) {
	for url, want := range map[string]int{
		"http://127.0.0.1:8080": 8080,
		"http://localhost":      80,
		"https://mnm.lan":       443,
	} {
		c := Default()
		c.MiniNetMan.URL = url
		if got := c.MiniNetManPort(); got != want {
			t.Errorf("%s: port %d, want %d", url, got, want)
		}
	}
}

func TestConfigLivesNextToTheBinary(t *testing.T) {
	if got, want := filepath.Dir(DefaultPath()), ExecutableDir(); got != want {
		t.Fatalf("config in %q, binary in %q", got, want)
	}
	if filepath.Base(DefaultPath()) != "config.json" {
		t.Fatalf("path = %q", DefaultPath())
	}
}

func TestMiniNetManDirDefaultsToNextToTheAppFolder(t *testing.T) {
	c := Default()
	if got := c.MiniNetManDir("/home/pi/opiz3-router"); got != "/home/pi/mini-net-man" {
		t.Fatalf("dir = %q", got)
	}
	c.MiniNetMan.Dir = "/srv/mnm"
	if got := c.MiniNetManDir("/home/pi/opiz3-router"); got != "/srv/mnm" {
		t.Fatalf("dir = %q", got)
	}
}
