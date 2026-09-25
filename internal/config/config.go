// Package config holds the router's runtime settings. Every field has a
// default matching the reference hardware, so the config file is optional and
// only needs the values that differ.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultPath = "/etc/opiz3-router/config.json"

type Config struct {
	Interfaces Interfaces `json:"interfaces"`
	Modem      Modem      `json:"modem"`
	WireGuard  WireGuard  `json:"wireguard"`
	Panel      Panel      `json:"panel"`
	Display    Display    `json:"display"`
	Network    Network    `json:"network"`
	Monitor    Monitor    `json:"monitor"`
	// DHCPLeasesFile is read to list LAN clients.
	DHCPLeasesFile string `json:"dhcp_leases_file"`
}

type Interfaces struct {
	Ethernet string `json:"ethernet"`
	USBModem string `json:"usb_modem"`
	Bridge   string `json:"bridge"`
	Wireless string `json:"wireless"`
}

type Modem struct {
	// Address is the modem's web interface, used for status polling.
	Address string `json:"address"`
}

type WireGuard struct {
	Interface string `json:"interface"`
	// Table is the routing table that LAN traffic is steered into.
	Table int `json:"table"`
	// RulePriority is the priority of the "iif <bridge> lookup <table>" rule.
	RulePriority int `json:"rule_priority"`
	// KillSwitch drops LAN traffic while the VPN is down. When false, LAN
	// traffic goes out of the WAN unencrypted until the VPN is back.
	KillSwitch bool `json:"kill_switch"`
}

type Panel struct {
	Enabled bool `json:"enabled"`
	// Listen is the address the web panel binds to. It should be the bridge
	// address so the panel is never served on a WAN interface.
	Listen string `json:"listen"`
	// AllowedInterfaces may reach the panel port; the firewall drops it on
	// every other interface.
	AllowedInterfaces []string `json:"allowed_interfaces"`
	// Username and Password enable HTTP basic auth when Password is set.
	Username string `json:"username"`
	Password string `json:"password"`
}

// Network is used to write the netplan file when the Ethernet mode is
// switched from the panel.
type Network struct {
	// NetplanFile is the netplan file the router writes. It must be the only
	// file in its directory, so no other file overrides it.
	NetplanFile string `json:"netplan_file"`
	// BridgeAddress is the address of the LAN bridge in CIDR notation.
	BridgeAddress string `json:"bridge_address"`
	// DNS servers used by the uplinks.
	DNS []string `json:"dns"`
}

// Monitor is the Wi-Fi adapter used in monitor mode to watch the channels.
type Monitor struct {
	Enabled   bool   `json:"enabled"`
	Interface string `json:"interface"`
	// HostapdConfig is read for the access point's channel.
	HostapdConfig string `json:"hostapd_config"`
	// HopIntervalMS is how long the adapter listens on each channel before
	// moving to the next, in milliseconds.
	HopIntervalMS int `json:"hop_interval_ms"`
}

type Display struct {
	SPIPort      string `json:"spi_port"`
	ResetPin     string `json:"reset_pin"`
	DCPin        string `json:"dc_pin"`
	CSPin        string `json:"cs_pin"`
	BacklightPin string `json:"backlight_pin"`
}

func Default() Config {
	return Config{
		Interfaces: Interfaces{
			Ethernet: "end0",
			USBModem: "enxd236db4ff68a",
			Bridge:   "br0",
			Wireless: "wlan0",
		},
		Modem: Modem{Address: "192.168.0.1"},
		WireGuard: WireGuard{
			Interface:    "wg0",
			Table:        200,
			RulePriority: 1000,
			KillSwitch:   true,
		},
		Panel: Panel{
			Enabled:           true,
			Listen:            "172.16.0.1:80",
			AllowedInterfaces: []string{"br0"},
			Username:          "admin",
		},
		Display: Display{
			SPIPort:      "SPI1.0",
			ResetPin:     "GPIO72",
			DCPin:        "GPIO69",
			CSPin:        "GPIO70",
			BacklightPin: "GPIO227",
		},
		Monitor: Monitor{
			Enabled:       true,
			Interface:     "wlx10feed10c013",
			HostapdConfig: "/etc/hostapd/hostapd.conf",
			HopIntervalMS: 1000,
		},
		Network: Network{
			NetplanFile:   "/etc/netplan/opiz3-router.yaml",
			BridgeAddress: "172.16.0.1/12",
			DNS:           []string{"1.1.1.1", "8.8.8.8"},
		},
		DHCPLeasesFile: "/var/lib/misc/dnsmasq.leases",
	}
}

// Load reads the config at path on top of the defaults. A missing file is not
// an error: the defaults are returned.
func Load(path string) (Config, error) {
	c := Default()
	data, e := os.ReadFile(path)
	if errors.Is(e, fs.ErrNotExist) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(data, &c); e != nil {
		return c, fmt.Errorf("parse %s: %w", path, e)
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	for name, v := range map[string]string{
		"interfaces.ethernet":   c.Interfaces.Ethernet,
		"interfaces.bridge":     c.Interfaces.Bridge,
		"wireguard.interface":   c.WireGuard.Interface,
		"modem.address":         c.Modem.Address,
		"display.spi_port":      c.Display.SPIPort,
		"display.reset_pin":     c.Display.ResetPin,
		"display.dc_pin":        c.Display.DCPin,
		"display.cs_pin":        c.Display.CSPin,
		"display.backlight_pin": c.Display.BacklightPin,
	} {
		if v == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	// Tables 253-255 are the kernel's default, main and local tables.
	if c.WireGuard.Table <= 0 || c.WireGuard.Table >= 253 {
		return fmt.Errorf("wireguard.table must be between 1 and 252")
	}
	if c.WireGuard.RulePriority <= 0 || c.WireGuard.RulePriority >= 32766 {
		return fmt.Errorf("wireguard.rule_priority must be between 1 and 32765")
	}
	if c.Panel.Enabled {
		if _, e := c.PanelPort(); e != nil {
			return e
		}
	}
	// A whole pass over the 13 channels must fit well inside the 90 s that
	// networks are remembered.
	if c.Monitor.HopIntervalMS < 100 || c.Monitor.HopIntervalMS > 5000 {
		return fmt.Errorf("monitor.hop_interval_ms must be between 100 and 5000")
	}
	if !filepath.IsAbs(c.Network.NetplanFile) || !strings.HasSuffix(c.Network.NetplanFile, ".yaml") {
		return fmt.Errorf("network.netplan_file must be an absolute path ending in .yaml")
	}
	if ip, _, e := net.ParseCIDR(c.Network.BridgeAddress); e != nil || ip.To4() == nil {
		return fmt.Errorf("network.bridge_address must be an IPv4 address in CIDR notation, such as 172.16.0.1/12")
	}
	for _, d := range c.Network.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("network.dns: %q is not an IP address", d)
		}
	}
	// Names end up in the netplan file; keep them to what interface names use.
	for name, v := range map[string]string{
		"interfaces.ethernet":  c.Interfaces.Ethernet,
		"interfaces.usb_modem": c.Interfaces.USBModem,
		"interfaces.bridge":    c.Interfaces.Bridge,
		"monitor.interface":    c.Monitor.Interface,
	} {
		if !validInterfaceName(v) {
			return fmt.Errorf("%s: %q is not a valid interface name", name, v)
		}
	}
	return nil
}

// validInterfaceName reports whether s is a Linux interface name (at most 15
// characters, no spaces, slashes or colons). An empty name is allowed.
func validInterfaceName(s string) bool {
	if len(s) > 15 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// PanelPort returns the TCP port of Panel.Listen.
func (c Config) PanelPort() (int, error) {
	_, p, e := net.SplitHostPort(c.Panel.Listen)
	if e != nil {
		return 0, fmt.Errorf("panel.listen: %w", e)
	}
	port, e := strconv.Atoi(p)
	if e != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("panel.listen: invalid port %q", p)
	}
	return port, nil
}
