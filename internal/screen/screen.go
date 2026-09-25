// Package screen decides what the router's status display shows: a banner with
// the VPN protection state and one colored row per link. It only builds the
// model; drawing it is left to the display driver.
package screen

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

type Level int

const (
	// Off is for something that is absent on purpose, such as an unplugged
	// modem or an unused LAN port.
	Off Level = iota
	Good
	Warning
	Critical
	// Info is for neutral news such as a new SMS.
	Info
)

// HandshakeStale is how old the last WireGuard handshake may be before the
// tunnel is reported as possibly broken.
const HandshakeStale = 3 * time.Minute

// How long a notification stays in the footer. SMS stay longer because they
// are easy to miss.
const (
	NotificationVisible = 30 * time.Second
	SMSVisible          = 5 * time.Minute
)

type Row struct {
	Label  string
	Level  Level
	Detail string
	// HasTraffic replaces Detail with the bytes received and sent.
	HasTraffic bool
	Rx, Tx     uint64
	// HasBars adds a signal meter with Bars of MaxBars filled.
	HasBars bool
	Bars    int
}

// MaxBars is the number of bars in the 4G signal meter.
const MaxBars = 5

type Model struct {
	BannerLevel Level
	Banner      string
	// Protection describes the banner state in words, for the event log.
	Protection string
	Rows       []Row
	Footer     string
	// FooterDot marks the footer as a notification of FooterLevel.
	FooterDot   bool
	FooterLevel Level
}

// NotificationLevel maps a notification level to a screen level.
func NotificationLevel(level string) Level {
	switch level {
	case state.Good:
		return Good
	case state.Warning:
		return Warning
	case state.Critical:
		return Critical
	}
	return Info
}

func Build(snap state.Snapshot, cfg config.Config, now time.Time) Model {
	m := Model{}
	m.BannerLevel, m.Banner, m.Protection = protection(snap, now)
	byName := map[string]state.Interface{}
	for _, i := range snap.Interfaces {
		byName[i.Name] = i
	}
	online := 0
	for _, c := range snap.Clients {
		if c.Online {
			online++
		}
	}

	m.Rows = []Row{
		ethernetRow(byName[cfg.Interfaces.Ethernet]),
		modemRow(byName[cfg.Interfaces.USBModem], snap.Modem),
		vpnRow(snap.WireGuard, byName[cfg.WireGuard.Interface], now),
		bridgeRow(byName[cfg.Interfaces.Bridge], online),
	}

	if n, ok := footerNotification(snap.Notifications, now); ok {
		m.Footer, m.FooterDot, m.FooterLevel = n.Text, true, NotificationLevel(n.Level)
	} else if cfg.Panel.Enabled {
		m.Footer = "http://" + strings.TrimSuffix(cfg.Panel.Listen, ":80")
	}
	return m
}

// footerNotification returns the newest notification that is still within
// its display time. A newer event that has expired, such as a brief link
// change, must not hide an SMS that is still being shown.
func footerNotification(log []state.Notification, now time.Time) (state.Notification, bool) {
	for _, n := range slices.Backward(log) {
		age := now.Sub(n.Time)
		if age < visibleFor(n) {
			return n, true
		}
		if age >= SMSVisible {
			break // older entries have all expired
		}
	}
	return state.Notification{}, false
}

func visibleFor(n state.Notification) time.Duration {
	if n.Source == "sms" {
		return SMSVisible
	}
	return NotificationVisible
}

func protection(snap state.Snapshot, now time.Time) (Level, string, string) {
	r, wg := snap.Routing, snap.WireGuard
	switch {
	case !r.Rule:
		return Critical, "NOT ROUTED", "LAN traffic is not routed to the VPN"
	case wg.Up && r.VPNRoute && handshakeFresh(wg, now):
		return Good, "PROTECTED", "VPN protected"
	case wg.Up && r.VPNRoute:
		return Warning, "NO HANDSHAKE", "VPN has no recent handshake"
	case r.KillSwitch:
		return Warning, "VPN DOWN - BLOCKED", "VPN down, kill switch blocking LAN traffic"
	case !r.KillSwitchEnabled:
		return Critical, "UNPROTECTED", "VPN down, LAN traffic unprotected"
	default:
		return Critical, "LEAKING", "VPN down and kill switch route missing"
	}
}

func handshakeFresh(wg state.WireGuard, now time.Time) bool {
	for _, p := range wg.Peers {
		if !p.LastHandshake.IsZero() && now.Sub(p.LastHandshake) < HandshakeStale {
			return true
		}
	}
	return false
}

// firstIPv4 returns the first IPv4 address without its prefix length.
func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if ip, _, e := net.ParseCIDR(a); e == nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

// uplinkRow describes an interface that should have an address of its own.
func uplinkRow(label string, i state.Interface) Row {
	switch {
	case !i.Present:
		return Row{Label: label, Level: Critical, Detail: "missing"}
	case !i.Up:
		return Row{Label: label, Level: Critical, Detail: "down"}
	}
	if ip := firstIPv4(i.Addresses); ip != "" {
		return Row{Label: label, Level: Good, Detail: ip}
	}
	return Row{Label: label, Level: Warning, Detail: "no address"}
}

func ethernetRow(i state.Interface) Row {
	if i.Role == "lan-port" {
		// A LAN port without a cable is normal, not a fault.
		if i.Up {
			return Row{Label: "ETH", Level: Good, Detail: "LAN port"}
		}
		return Row{Label: "ETH", Level: Off, Detail: "LAN port idle"}
	}
	r := uplinkRow("ETH", i)
	if i.Present && !i.Up {
		r.Detail = "no cable"
	}
	return r
}

func modemRow(i state.Interface, m state.Modem) Row {
	switch {
	case !i.Present:
		return Row{Label: "4G", Level: Off, Detail: "unplugged"}
	case !i.Up:
		return Row{Label: "4G", Level: Critical, Detail: "down"}
	case !m.Reachable:
		return Row{Label: "4G", Level: Warning, Detail: "no response"}
	}
	var parts []string
	if t := strings.Join(strings.Fields(m.NetworkType), " "); t != "" {
		parts = append(parts, t)
	}
	// "-71 dBm" reads fine as "-71dBm" and saves space.
	if sig := strings.Join(strings.Fields(m.SignalStrength), ""); sig != "" {
		parts = append(parts, sig)
	}
	detail := strings.Join(parts, " ")
	if detail == "" {
		detail = "connected"
	}
	return Row{Label: "4G", Level: Good, Detail: detail, HasBars: true, Bars: min(max(int(m.SignalLevel), 0), MaxBars)}
}

// vpnRow shows the tunnel state and, while it works, the bytes received and
// sent through the VPN interface.
func vpnRow(wg state.WireGuard, i state.Interface, now time.Time) Row {
	switch {
	case wg.Error != "" || !wg.Up:
		return Row{Label: "VPN", Level: Critical, Detail: "down"}
	case len(wg.Peers) == 0:
		return Row{Label: "VPN", Level: Critical, Detail: "no peer"}
	case !handshakeFresh(wg, now):
		return Row{Label: "VPN", Level: Warning, Detail: "no handshake"}
	}
	return Row{Label: "VPN", Level: Good, HasTraffic: true, Rx: i.RxBytes, Tx: i.TxBytes}
}

// bridgeRow describes the LAN bridge that the Wi-Fi access point (and the
// Ethernet port in server mode) belong to.
func bridgeRow(i state.Interface, online int) Row {
	if !i.Present || !i.Up {
		return Row{Label: "BRIDGE", Level: Critical, Detail: "down"}
	}
	return Row{Label: "BRIDGE", Level: Good, Detail: fmt.Sprintf("%d online", online)}
}

// FormatBytes formats a byte count briefly, such as "512B", "3.4M" or "12G".
// The result is at most 4 characters, so it fits a fixed-width slot.
func FormatBytes(b uint64) string {
	v := float64(b)
	units := []string{"B", "K", "M", "G", "T", "P", "E"}
	i := 0
	// Step up before rounding would print four digits, e.g. 999.7K -> 1M.
	for v >= 999.5 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i > 0 && v < 9.95 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + units[i]
	}
	return fmt.Sprintf("%.0f", v) + units[i]
}
