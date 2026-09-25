package screen

import (
	"strings"
	"testing"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func monitorSnapshot() state.Snapshot {
	s := healthy()
	s.Monitor = state.Monitor{Page: state.PageChannels, Interface: "wlx10feed10c013", Present: true, Active: true,
		OwnChannel: 6, OwnSignal: -35, Recommended: 11, ScanChannel: 3,
		Networks: []state.MonitorNetwork{
			{SSID: "Neighbour", Channel: 1, Signal: -45},
			{SSID: "", Channel: 1, Signal: -60},
			{SSID: "Cafe", Channel: 3, Signal: -71},
			{SSID: "Far", Channel: 13, Signal: -88},
			{SSID: "Mystery", Channel: 6, Signal: 0}, // signal not reported
		}}
	return s
}

func TestChannelsPage(t *testing.T) {
	m := BuildChannels(monitorSnapshot(), config.Default(), now)
	if m.Message != "" || m.Summary != "5 networks" || m.Own != 6 || m.Best != 11 || m.Scan != 3 {
		t.Fatalf("model = %+v", m)
	}
	want := []Hump{
		{Channel: 6, Signal: unknownSignal},                             // weakest other first
		{Channel: 13, Signal: -88},                                      // then the other one
		{Channel: 3, Signal: -71, Label: "Cafe", Series: SeriesTop + 2}, // top three, weakest first
		{Channel: 1, Signal: -60, Label: "hidden", Series: SeriesTop + 1},
		{Channel: 1, Signal: -45, Label: "Neighbour", Series: SeriesTop},
		{Channel: 6, Signal: -35, Label: "you", Series: SeriesOwn}, // own access point on top
	}
	if len(m.Humps) != len(want) {
		t.Fatalf("humps = %+v", m.Humps)
	}
	for i := range want {
		if m.Humps[i] != want[i] {
			t.Errorf("hump %d = %+v, want %+v", i, m.Humps[i], want[i])
		}
	}
	if m.Footer != "Best: 11  Yours: 6" || m.FooterDot {
		t.Fatalf("footer = %q", m.Footer)
	}
}

func TestChannelsPageWithoutOwnSignal(t *testing.T) {
	s := monitorSnapshot()
	s.Monitor.OwnSignal = 0 // own access point not heard
	for _, h := range BuildChannels(s, config.Default(), now).Humps {
		if h.Series == SeriesOwn {
			t.Fatal("own access point drawn without a signal")
		}
	}
}

func TestChannelsPageFooter(t *testing.T) {
	s := monitorSnapshot()
	s.Monitor.Recommended = 6
	if m := BuildChannels(s, config.Default(), now); m.Footer != "Your channel 6 is best" {
		t.Errorf("footer = %q", m.Footer)
	}
	s.Monitor.OwnChannel = 0
	if m := BuildChannels(s, config.Default(), now); m.Footer != "Best channel: 6" {
		t.Errorf("footer = %q", m.Footer)
	}
	s.Notifications = []state.Notification{{Time: now, Level: state.Info, Source: "sms", Text: "SMS from 1000"}}
	if m := BuildChannels(s, config.Default(), now); m.Footer != "SMS from 1000" || !m.FooterDot {
		t.Errorf("notification not shown: %q", m.Footer)
	}
}

func TestChannelsPageMessages(t *testing.T) {
	s := monitorSnapshot()
	s.Monitor.Present, s.Monitor.Active = false, false
	if m := BuildChannels(s, config.Default(), now); !strings.Contains(m.Message, "wlx10feed10c013") || m.Humps != nil {
		t.Errorf("absent adapter: %+v", m)
	}
	s.Monitor.Present = true
	if m := BuildChannels(s, config.Default(), now); m.Message != "Starting the adapter..." {
		t.Errorf("starting: %q", m.Message)
	}
	s.Monitor.Error = "iw: device busy"
	if m := BuildChannels(s, config.Default(), now); !strings.Contains(m.Message, "device busy") {
		t.Errorf("error: %q", m.Message)
	}
}
