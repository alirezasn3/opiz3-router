package setup

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	goSystemd "github.com/alirezasn3/go-systemd"
	"opiz3-router/internal/config"
	"opiz3-router/internal/router"
)

// ServiceName is the systemd service that runs the router.
const ServiceName = "opiz3-router"

// Packages the router needs that a stock image may lack.
var packages = []string{"wireguard-tools", "hostapd", "dnsmasq", "iw", "netplan.io", "nftables"}

// Installer makes a stock Orange Pi Ubuntu image ready to run the router.
// Every step can be run again: it only changes what is not already right.
type Installer struct {
	Cfg     config.Config
	CfgPath string
	Paths   Paths
	Run     Runner
	HTTP    *http.Client
	Out     io.Writer

	// MiniNetManBinary is a local mini-net-man binary to use instead of
	// downloading the latest release.
	MiniNetManBinary string
	// ReleaseBase is where mini-net-man's release files are downloaded from.
	ReleaseBase string

	// RebootNeeded is set when a change only takes effect after a reboot.
	RebootNeeded bool
	// Notes are things the person installing has to do or know afterwards.
	Notes []string
}

func NewInstaller(cfg config.Config, cfgPath string, out io.Writer) *Installer {
	return &Installer{
		Cfg: cfg, CfgPath: cfgPath, Paths: DefaultPaths(), Run: ExecRunner(), Out: out,
		HTTP:        &http.Client{Timeout: 5 * time.Minute},
		ReleaseBase: "https://github.com/alirezasn3/mini-net-man/releases/latest/download",
	}
}

func (in *Installer) say(format string, args ...any) {
	fmt.Fprintf(in.Out, format+"\n", args...)
}

func (in *Installer) note(format string, args ...any) {
	in.Notes = append(in.Notes, fmt.Sprintf(format, args...))
}

// Install runs every step. The network step comes last because it can
// interrupt an SSH session that runs over the Ethernet port.
func (in *Installer) Install(ctx context.Context) error {
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"config", in.ensureConfig},
		{"packages", in.installPackages},
		{"display overlay", in.enableDisplayOverlay},
		{"access point and DHCP", in.configureAP},
		{"WireGuard", in.configureWireGuard},
		{"mini-net-man", in.installMiniNetMan},
		{"network", in.configureNetwork},
		{"service", in.installService},
	}
	for _, s := range steps {
		in.say("== %s", s.name)
		if e := s.run(ctx); e != nil {
			return fmt.Errorf("%s: %w", s.name, e)
		}
	}
	return nil
}

// ensureConfig makes sure the config file exists and the access point has a
// passphrase.
func (in *Installer) ensureConfig(context.Context) error {
	_, statErr := os.Stat(in.CfgPath)
	changed := statErr != nil
	if in.Cfg.WiFi.Passphrase == "" {
		p, e := randomPassphrase(16)
		if e != nil {
			return e
		}
		in.Cfg.WiFi.Passphrase = p
		in.note("Wi-Fi network %q has the new passphrase %s (change it in the panel or in %s).", in.Cfg.WiFi.SSID, p, in.CfgPath)
		changed = true
	}
	if !changed {
		return nil
	}
	in.say("writing %s", in.CfgPath)
	return in.Cfg.Save(in.CfgPath)
}

func randomPassphrase(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	for i := range b {
		v, e := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if e != nil {
			return "", e
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b), nil
}

func (in *Installer) installPackages(ctx context.Context) error {
	var missing []string
	for _, p := range packages {
		out, e := in.Run.Run(ctx, "dpkg-query", "-W", "-f=${db:Status-Status}", p)
		if e != nil || strings.TrimSpace(out) != "installed" {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		in.say("all installed")
		return nil
	}
	in.say("installing %s", strings.Join(missing, " "))
	// Packages start their services when they are installed, before there is
	// a bridge or any config for them. Ask the init system not to.
	if _, e := os.Stat(in.Paths.PolicyRC); os.IsNotExist(e) {
		if e := os.WriteFile(in.Paths.PolicyRC, []byte("#!/bin/sh\nexit 101\n"), 0o755); e == nil {
			defer os.Remove(in.Paths.PolicyRC)
		}
	}
	if _, e := in.Run.Run(ctx, "apt-get", "update"); e != nil {
		in.say("apt-get update failed (%v); trying to install anyway", e)
	}
	_, e := in.Run.Run(ctx, "env", append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y"}, missing...)...)
	return e
}

func (in *Installer) enableDisplayOverlay(context.Context) error {
	data, e := os.ReadFile(in.Paths.BootEnv)
	if e != nil {
		return fmt.Errorf("read %s: %w (is this an Orange Pi image?)", in.Paths.BootEnv, e)
	}
	updated, changed := UpdateBootEnv(string(data))
	if !changed {
		in.say("already enabled")
		return nil
	}
	if _, e = writeIfChanged(in.Paths.BootEnv, []byte(updated), 0o644); e != nil {
		return e
	}
	in.say("added %s to %s", displayOverlay, in.Paths.BootEnv)
	in.RebootNeeded = true
	return nil
}

// dropIn writes a systemd drop-in for unit and reports whether it changed.
func (in *Installer) dropIn(unit, content string) (bool, error) {
	return writeIfChanged(filepath.Join(in.Paths.SystemdDir, unit+".d", "opiz3-router.conf"), []byte(content), 0o644)
}

// waitForBridge makes a service wait for the LAN bridge and its address, which
// mini-net-man creates after boot.
func waitForBridge(bridge string) string {
	return fmt.Sprintf(`[Unit]
After=mini-net-man.service

[Service]
ExecStartPre=/bin/sh -c 'for i in $$(seq 1 60); do ip -4 -o addr show dev %s 2>/dev/null | grep -q . && exit 0; sleep 1; done; exit 1'
Restart=on-failure
RestartSec=5
`, bridge)
}

func (in *Installer) configureAP(ctx context.Context) error {
	ch, e := WriteAP(in.Cfg, in.Paths)
	if e != nil {
		return e
	}
	for _, f := range []struct {
		changed bool
		path    string
	}{{ch.Hostapd, in.Paths.Hostapd}, {ch.Dnsmasq, in.Paths.Dnsmasq}} {
		if f.changed {
			in.say("wrote %s", f.path)
		}
	}
	reload := false
	for _, unit := range []string{"hostapd.service", "dnsmasq.service"} {
		changed, e := in.dropIn(unit, waitForBridge(in.Cfg.Interfaces.Bridge))
		if e != nil {
			return e
		}
		reload = reload || changed
	}
	if reload {
		if _, e = in.Run.Run(ctx, "systemctl", "daemon-reload"); e != nil {
			return e
		}
	}
	// Ubuntu masks hostapd until it is configured.
	if _, e = in.Run.Run(ctx, "systemctl", "unmask", "hostapd"); e != nil {
		return e
	}
	if _, e = in.Run.Run(ctx, "systemctl", "enable", "hostapd", "dnsmasq"); e != nil {
		return e
	}
	// The services need the bridge, which exists once the router runs; start
	// them without waiting for it.
	for svc, changed := range map[string]bool{"hostapd": ch.Hostapd, "dnsmasq": ch.Dnsmasq} {
		if changed {
			if _, e = in.Run.Run(ctx, "systemctl", "restart", "--no-block", svc); e != nil {
				return e
			}
		}
	}
	return nil
}

const networkdConf = `# Written by opiz3-router. systemd-networkd must leave the routing rules and
# routes that mini-net-man keeps alone.
[Network]
ManageForeignRoutingPolicyRules=no
ManageForeignRoutes=no
`

var tableOff = regexp.MustCompile(`(?im)^\s*Table\s*=\s*off\s*$`)

func (in *Installer) configureWireGuard(ctx context.Context) error {
	wg := in.Cfg.WireGuard.Interface
	unit := "wg-quick@" + wg + ".service"
	// Systemd retries the tunnel if it fails at boot, for example because the
	// uplink is not up yet.
	changed, e := in.dropIn(unit, "[Service]\nRestart=on-failure\nRestartSec=10\n")
	if e != nil {
		return e
	}
	if changed {
		if _, e = in.Run.Run(ctx, "systemctl", "daemon-reload"); e != nil {
			return e
		}
	}
	conf := filepath.Join(in.Paths.WireGuardDir, wg+".conf")
	data, e := os.ReadFile(conf)
	if os.IsNotExist(e) {
		in.note("Create %s, then run: systemctl enable --now wg-quick@%s", conf, wg)
		return nil
	}
	if e != nil {
		return e
	}
	if !tableOff.Match(data) {
		in.note("Add \"Table = off\" to the [Interface] section of %s: the router sets up the routes for LAN traffic itself.", conf)
	}
	_, e = in.Run.Run(ctx, "systemctl", "enable", "--now", "wg-quick@"+wg)
	return e
}

// configureNetwork writes the fallback netplan file, turns NetworkManager off
// and lets systemd-networkd take over. The file only brings the Ethernet port
// and the modem up with DHCP, so the device stays reachable if the router or
// mini-net-man fail to start; the bridge itself belongs to mini-net-man.
func (in *Installer) configureNetwork(ctx context.Context) error {
	file := in.Cfg.Network.NetplanFile
	dir := filepath.Dir(file)
	changed := false

	entries, e := os.ReadDir(dir)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	var moved [][2]string
	for _, en := range entries {
		ext := filepath.Ext(en.Name())
		from := filepath.Join(dir, en.Name())
		if en.IsDir() || (ext != ".yaml" && ext != ".yml") || from == file {
			continue
		}
		to := filepath.Join(in.Paths.NetplanBackup, en.Name())
		if e = os.MkdirAll(in.Paths.NetplanBackup, 0o755); e != nil {
			return e
		}
		if e = os.Rename(from, to); e != nil {
			return e
		}
		in.say("moved %s to %s", from, to)
		moved = append(moved, [2]string{from, to})
		changed = true
	}
	undo := func() {
		for _, m := range moved {
			_ = os.Rename(m[1], m[0])
		}
	}

	if _, e = os.Stat(file); os.IsNotExist(e) {
		content, e := router.RenderNetplan(router.ModeUplink, in.Cfg)
		if e != nil {
			undo()
			return e
		}
		if _, e = writeIfChanged(file, content, 0o600); e != nil {
			undo()
			return e
		}
		in.say("wrote %s", file)
		changed = true
	}
	if _, e = in.Run.Run(ctx, "netplan", "generate"); e != nil {
		undo()
		return e
	}

	// By default networkd deletes the routing rules and routes it did not
	// make whenever it configures a link, such as when the modem is plugged
	// in. That would remove the VPN routing mini-net-man keeps.
	if c, e := writeIfChanged(in.Paths.NetworkdConf, []byte(networkdConf), 0o644); e != nil {
		undo()
		return e
	} else if c {
		in.say("wrote %s", in.Paths.NetworkdConf)
		changed = true
	}

	nm, e := in.disableNetworkManager(ctx)
	if e != nil {
		return e
	}
	if _, e = in.Run.Run(ctx, "systemctl", "enable", "systemd-networkd"); e != nil {
		return e
	}
	if !changed && !nm {
		in.say("already set up")
		return nil
	}
	in.say("applying the network config; an SSH session over Ethernet may drop for a moment")
	_, e = in.Run.Run(ctx, "netplan", "apply")
	return e
}

// disableNetworkManager stops and masks NetworkManager, and reports whether
// it was still active.
func (in *Installer) disableNetworkManager(ctx context.Context) (bool, error) {
	out, e := in.Run.Run(ctx, "systemctl", "list-unit-files", "NetworkManager.service", "--no-legend")
	if e != nil || strings.TrimSpace(out) == "" || strings.Contains(out, "masked") {
		return false, nil
	}
	in.say("disabling NetworkManager")
	for _, args := range [][]string{
		{"disable", "--now", "NetworkManager.service"},
		{"mask", "NetworkManager.service", "NetworkManager-wait-online.service", "NetworkManager-dispatcher.service"},
	} {
		if _, e := in.Run.Run(ctx, "systemctl", args...); e != nil {
			return true, e
		}
	}
	return true, nil
}

// installService creates and starts the router's service. It starts after
// mini-net-man, which holds the bridge the router's panel listens on.
func (in *Installer) installService(ctx context.Context) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	execStart := exe
	if in.CfgPath != config.DefaultPath {
		execStart += " -config " + in.CfgPath
	}
	changed, e := in.dropIn(ServiceName+".service", "[Unit]\nAfter=mini-net-man.service\nWants=mini-net-man.service\n")
	if e != nil {
		return e
	}
	if changed {
		if _, e = in.Run.Run(ctx, "systemctl", "daemon-reload"); e != nil {
			return e
		}
	}
	return goSystemd.CreateService(&goSystemd.Service{
		Name:        ServiceName,
		Description: "Orangepi Zero 3 Router",
		ExecStart:   execStart,
		Restart:     "on-failure",
		RestartSec:  "10s",
		After:       "network-online.target",
		Wants:       "network-online.target",
		WantedBy:    "multi-user.target",
	})
}
