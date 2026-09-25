package config

import (
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
	if c.Monitor.Interface != "wlx10feed10c013" {
		t.Fatalf("monitor interface = %q", c.Monitor.Interface)
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
		"hop too fast":     func(c *Config) { c.Monitor.HopIntervalMS = 10 },
		"hop too slow":     func(c *Config) { c.Monitor.HopIntervalMS = 60000 },
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
