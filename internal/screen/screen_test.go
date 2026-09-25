package screen

import (
	"testing"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func healthy() state.Snapshot {
	return state.Snapshot{
		Interfaces: []state.Interface{
			{Name: "end0", Role: "wan-ethernet", Present: true, Up: true, Addresses: []string{"fe80::1/64", "192.168.1.44/24"}},
			{Name: "enxd236db4ff68a", Role: "wan-4g", Present: true, Up: true},
			{Name: "wlan0", Role: "wifi", Present: true, Up: true},
			{Name: "wg0", Role: "vpn", Present: true, Up: true, RxBytes: 6 << 30, TxBytes: 900 << 20},
			{Name: "br0", Role: "lan", Present: true, Up: true, Addresses: []string{"172.16.0.1/12"}},
		},
		WireGuard: state.WireGuard{Up: true, Peers: []state.Peer{{LastHandshake: now.Add(-time.Minute), RxRate: 400000, TxRate: 36000}}},
		Modem:     state.Modem{Reachable: true, NetworkType: " LTE\n", SignalStrength: "-71 \t dBm", SignalLevel: 3},
		Clients:   []state.Client{{Online: true, Wireless: true}, {Online: true}, {Online: false, Wireless: true}},
		Routing:   state.Routing{Rule: true, VPNRoute: true, KillSwitch: true, KillSwitchEnabled: true},
	}
}

func row(m Model, label string) Row {
	for _, r := range m.Rows {
		if r.Label == label {
			return r
		}
	}
	return Row{}
}

func TestHealthy(t *testing.T) {
	m := Build(healthy(), config.Default(), now)
	if m.BannerLevel != Good || m.Banner != "PROTECTED" {
		t.Fatalf("banner = %v %q", m.BannerLevel, m.Banner)
	}
	for label, want := range map[string]Row{
		"ETH":    {Label: "ETH", Level: Good, Detail: "192.168.1.44"},
		"4G":     {Label: "4G", Level: Good, Detail: "LTE -71dBm", HasBars: true, Bars: 3},
		"BRIDGE": {Label: "BRIDGE", Level: Good, Detail: "2 online"},
		"VPN":    {Label: "VPN", Level: Good, HasTraffic: true, Rx: 6 << 30, Tx: 900 << 20},
	} {
		if got := row(m, label); got != want {
			t.Errorf("%s = %+v, want %+v", label, got, want)
		}
	}
	if len(m.Rows) != 4 {
		t.Errorf("rows = %+v", m.Rows)
	}
	if m.Footer != "http://172.16.0.1" || m.FooterDot {
		t.Errorf("footer = %q dot=%v", m.Footer, m.FooterDot)
	}
}

func TestProblems(t *testing.T) {
	s := healthy()
	s.Interfaces[0].Up = false                                 // cable pulled
	s.Interfaces[1] = state.Interface{Name: "enxd236db4ff68a"} // modem unplugged
	s.WireGuard.Peers[0].LastHandshake = now.Add(-10 * time.Minute)
	s.Notifications = []state.Notification{{Time: now.Add(-5 * time.Second), Level: state.Warning, Source: "link", Text: "ETH down"}}
	m := Build(s, config.Default(), now)

	if r := row(m, "ETH"); r.Level != Critical || r.Detail != "no cable" {
		t.Errorf("ETH = %+v", r)
	}
	if r := row(m, "4G"); r.Level != Off || r.Detail != "unplugged" {
		t.Errorf("4G = %+v", r)
	}
	if r := row(m, "VPN"); r.Level != Warning || r.Detail != "no handshake" {
		t.Errorf("VPN = %+v", r)
	}
	if m.BannerLevel != Warning || m.Banner != "NO HANDSHAKE" {
		t.Errorf("banner = %v %q", m.BannerLevel, m.Banner)
	}
	if m.Footer != "ETH down" || !m.FooterDot || m.FooterLevel != Warning {
		t.Errorf("footer = %q dot=%v level=%v", m.Footer, m.FooterDot, m.FooterLevel)
	}
}

func TestBannerWhenVPNDown(t *testing.T) {
	s := healthy()
	s.WireGuard.Up = false
	s.Routing.VPNRoute = false
	if m := Build(s, config.Default(), now); m.BannerLevel != Warning || m.Banner != "VPN DOWN - BLOCKED" {
		t.Errorf("kill switch on: %v %q", m.BannerLevel, m.Banner)
	}
	s.Routing.KillSwitch, s.Routing.KillSwitchEnabled = false, false
	if m := Build(s, config.Default(), now); m.BannerLevel != Critical || m.Banner != "UNPROTECTED" {
		t.Errorf("kill switch off: %v %q", m.BannerLevel, m.Banner)
	}
	s.Routing.Rule = false
	if m := Build(s, config.Default(), now); m.Banner != "NOT ROUTED" {
		t.Errorf("no rule: %q", m.Banner)
	}
}

func TestFooterNotificationExpiry(t *testing.T) {
	s := healthy()
	s.Notifications = []state.Notification{{Time: now.Add(-time.Minute), Level: state.Warning, Source: "link", Text: "ETH down"}}
	if m := Build(s, config.Default(), now); m.FooterDot {
		t.Errorf("a minute-old link event is still shown: %q", m.Footer)
	}
	s.Notifications = append(s.Notifications, state.Notification{Time: now.Add(-2 * time.Minute), Level: state.Info, Source: "sms", Text: "SMS from 1000"})
	if m := Build(s, config.Default(), now); m.Footer != "SMS from 1000" || m.FooterLevel != Info {
		t.Errorf("SMS not shown for longer: %q %v", m.Footer, m.FooterLevel)
	}

	// A newer event takes the footer while it is fresh, then the SMS returns.
	s.Notifications = append(s.Notifications, state.Notification{Time: now.Add(-10 * time.Second), Level: state.Good, Source: "link", Text: "4G up"})
	if m := Build(s, config.Default(), now); m.Footer != "4G up" {
		t.Errorf("fresh link event not shown: %q", m.Footer)
	}
	if m := Build(s, config.Default(), now.Add(time.Minute)); m.Footer != "SMS from 1000" {
		t.Errorf("expired link event hides the SMS: %q", m.Footer)
	}
	if m := Build(s, config.Default(), now.Add(4*time.Minute)); m.FooterDot {
		t.Errorf("SMS shown after its time: %q", m.Footer)
	}
}

func TestServerModeEthernetIsLANPort(t *testing.T) {
	s := healthy()
	s.Interfaces[0] = state.Interface{Name: "end0", Role: "lan-port", Present: true}
	if r := row(Build(s, config.Default(), now), "ETH"); r.Level != Off {
		t.Errorf("idle LAN port = %+v", r)
	}
}

func TestModemSignalBarsAreClamped(t *testing.T) {
	s := healthy()
	for level, want := range map[int64]int{-1: 0, 0: 0, 4: 4, 5: 5, 9: MaxBars} {
		s.Modem.SignalLevel = level
		if r := row(Build(s, config.Default(), now), "4G"); r.Bars != want {
			t.Errorf("level %d: bars = %d, want %d", level, r.Bars, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0B", 512: "512B", 999: "999B", 1000: "1K", 3500: "3.4K", 12 << 20: "12M", 6 << 30: "6G", 900 << 20: "900M", 1023 << 20: "1G", 3 << 40: "3T"} {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
	// Never wider than the four characters the screen reserves.
	for b := 1.0; b < 1.8e19; b *= 1.07 {
		if got := FormatBytes(uint64(b)); len(got) > 4 {
			t.Fatalf("FormatBytes(%v) = %q is longer than 4", uint64(b), got)
		}
	}
}
