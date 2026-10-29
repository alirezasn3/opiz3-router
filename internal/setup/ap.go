package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"opiz3-router/internal/config"
)

// Paths are the files and directories setup writes. Tests point them at a
// temporary directory.
type Paths struct {
	BootEnv       string
	Hostapd       string
	Dnsmasq       string
	SystemdDir    string
	NetplanBackup string
	WireGuardDir  string
	PolicyRC      string
	NetworkdConf  string
}

// DefaultPaths are the system's files. The backup of the image's own netplan
// files goes next to the router's binary, with the rest of its files.
func DefaultPaths(appDir string) Paths {
	return Paths{
		BootEnv:       "/boot/orangepiEnv.txt",
		Hostapd:       "/etc/hostapd/hostapd.conf",
		Dnsmasq:       "/etc/dnsmasq.d/br0.conf",
		SystemdDir:    "/etc/systemd/system",
		NetplanBackup: filepath.Join(appDir, "netplan-backup"),
		WireGuardDir:  "/etc/wireguard",
		PolicyRC:      "/usr/sbin/policy-rc.d",
		NetworkdConf:  "/etc/systemd/networkd.conf.d/opiz3-router.conf",
	}
}

// RenderHostapd returns hostapd.conf for the access point on the wireless
// interface, bridged into the LAN bridge.
func RenderHostapd(cfg config.Config) string {
	mode := "g"
	if cfg.WiFi.Channel > 14 {
		mode = "a"
	}
	return fmt.Sprintf(`# Written by opiz3-router. Change these in the panel or in config.json.
interface=%s
bridge=%s
driver=nl80211
ssid=%s
hw_mode=%s
channel=%d
wmm_enabled=1
auth_algs=1
wpa=2
wpa_passphrase=%s
wpa_key_mgmt=WPA-PSK
rsn_pairwise=CCMP
`, cfg.Interfaces.Wireless, cfg.Interfaces.Bridge, cfg.WiFi.SSID, mode, cfg.WiFi.Channel, cfg.WiFi.Passphrase)
}

// RenderDnsmasq returns the DHCP server config for the LAN bridge. DNS is
// not served (port=0); clients are pointed at the configured resolvers.
func RenderDnsmasq(cfg config.Config) (string, error) {
	_, subnet, e := net.ParseCIDR(cfg.Network.BridgeAddress)
	if e != nil {
		return "", e
	}
	mask := net.IP(subnet.Mask).String()
	return fmt.Sprintf(`# Written by opiz3-router. Change these in the panel or in config.json.
interface=%s
bind-interfaces
dhcp-range=%s,%s,%s,24h
dhcp-option=option:dns-server,%s
port=0
`, cfg.Interfaces.Bridge, cfg.Network.DHCPStart, cfg.Network.DHCPEnd, mask, strings.Join(cfg.Network.DNS, ",")), nil
}

// writeIfChanged writes data to path unless it already holds exactly that. It
// reports whether the file changed.
func writeIfChanged(path string, data []byte, perm os.FileMode) (bool, error) {
	if old, e := os.ReadFile(path); e == nil && string(old) == string(data) {
		return false, nil
	}
	if e := os.MkdirAll(filepath.Dir(path), 0o755); e != nil {
		return false, e
	}
	tmp := path + ".tmp"
	if e := os.WriteFile(tmp, data, perm); e != nil {
		return false, e
	}
	if e := os.Chmod(tmp, perm); e != nil {
		return false, e
	}
	return true, os.Rename(tmp, path)
}

// APChanges says which services need a restart after the files were written.
type APChanges struct {
	Hostapd, Dnsmasq bool
}

// WriteAP writes the hostapd and dnsmasq config. hostapd is left alone until
// the access point has a passphrase.
func WriteAP(cfg config.Config, p Paths) (APChanges, error) {
	var ch APChanges
	var e error
	if cfg.WiFi.Passphrase != "" {
		if ch.Hostapd, e = writeIfChanged(p.Hostapd, []byte(RenderHostapd(cfg)), 0o600); e != nil {
			return ch, e
		}
	}
	dnsmasq, e := RenderDnsmasq(cfg)
	if e != nil {
		return ch, e
	}
	ch.Dnsmasq, e = writeIfChanged(p.Dnsmasq, []byte(dnsmasq), 0o644)
	return ch, e
}

// Settings are the access point and DHCP values the panel can change.
type Settings struct {
	SSID string `json:"ssid"`
	// Passphrase is only sent to change it; an empty value keeps the current one.
	Passphrase    string `json:"passphrase,omitempty"`
	PassphraseSet bool   `json:"passphrase_set"`
	Channel       int    `json:"channel"`
	DHCPStart     string `json:"dhcp_start"`
	DHCPEnd       string `json:"dhcp_end"`
}

// AP changes the access point and DHCP settings: it saves them in the config
// file, rewrites the hostapd and dnsmasq files and restarts what changed.
type AP struct {
	mu    sync.Mutex
	cfg   config.Config
	path  string
	paths Paths
	run   Runner
}

func NewAP(cfg config.Config, cfgPath string) *AP {
	return &AP{cfg: cfg, path: cfgPath, paths: DefaultPaths(config.ExecutableDir()), run: ExecRunner()}
}

func (a *AP) Settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.cfg
	return Settings{
		SSID: c.WiFi.SSID, PassphraseSet: c.WiFi.Passphrase != "", Channel: c.WiFi.Channel,
		DHCPStart: c.Network.DHCPStart, DHCPEnd: c.Network.DHCPEnd,
	}
}

// Update applies new settings. When a service fails to restart with them, the
// old settings are put back.
func (a *AP) Update(ctx context.Context, s Settings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.cfg
	next.WiFi.SSID, next.WiFi.Channel = s.SSID, s.Channel
	if s.Passphrase != "" {
		next.WiFi.Passphrase = s.Passphrase
	}
	next.Network.DHCPStart, next.Network.DHCPEnd = s.DHCPStart, s.DHCPEnd
	if e := next.Validate(); e != nil {
		return e
	}
	if next.WiFi.Passphrase == "" {
		return errors.New("set a Wi-Fi passphrase first")
	}
	if e := a.apply(ctx, next); e != nil {
		if re := a.apply(ctx, a.cfg); re != nil {
			return fmt.Errorf("%w; putting the old settings back also failed: %v", e, re)
		}
		return e
	}
	a.cfg = next
	return nil
}

func (a *AP) apply(ctx context.Context, cfg config.Config) error {
	ch, e := WriteAP(cfg, a.paths)
	if e != nil {
		return e
	}
	// Change only the access point's settings in the file, so a setting
	// changed from another card meanwhile is not undone.
	if _, e = config.Modify(a.path, func(c *config.Config) {
		c.WiFi = cfg.WiFi
		c.Network.DHCPStart, c.Network.DHCPEnd = cfg.Network.DHCPStart, cfg.Network.DHCPEnd
	}); e != nil {
		return fmt.Errorf("save %s: %w", a.path, e)
	}
	// Restarting hostapd drops the Wi-Fi clients, including whoever is using
	// the panel; it comes back by itself within seconds.
	if ch.Hostapd {
		if _, e = a.run.Run(ctx, "systemctl", "restart", "--no-block", "hostapd"); e != nil {
			return e
		}
	}
	if ch.Dnsmasq {
		if _, e = a.run.Run(ctx, "systemctl", "restart", "--no-block", "dnsmasq"); e != nil {
			return e
		}
	}
	return nil
}
