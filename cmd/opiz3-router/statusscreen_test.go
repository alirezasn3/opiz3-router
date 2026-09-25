//go:build linux

package main

import (
	"image/color"
	"image/png"
	"os"
	"slices"
	"testing"
	"time"

	"opiz3-router/internal/assets"
	"opiz3-router/internal/config"
	"opiz3-router/internal/screen"
	"opiz3-router/internal/state"
)

// Set OPIZ3_SCREEN_PNG=/path/prefix to save the rendered frames for a look.
func TestDrawStatus(t *testing.T) {
	d := newTestDisplay(t)

	now := time.Now()
	snap := state.Snapshot{
		Interfaces: []state.Interface{
			{Name: "end0", Role: "wan-ethernet", Present: true, Up: true, Addresses: []string{"192.168.1.44/24"}},
			{Name: "enxd236db4ff68a", Role: "wan-4g", Present: true, Up: true},
			{Name: "wlan0", Role: "wifi", Present: true, Up: true},
			{Name: "wg0", Role: "vpn", Present: true, Up: true, RxBytes: 6 << 30, TxBytes: 900 << 20},
			{Name: "br0", Role: "lan", Present: true, Up: true},
		},
		WireGuard: state.WireGuard{Up: true, Peers: []state.Peer{{LastHandshake: now.Add(-time.Minute), RxRate: 400000, TxRate: 36000}}},
		Modem:     state.Modem{Reachable: true, NetworkType: "LTE", SignalStrength: "-71 dBm", SignalLevel: 3},
		Clients:   []state.Client{{Online: true, Wireless: true}, {Online: true, Wireless: true}, {Online: true}},
		Routing:   state.Routing{Rule: true, VPNRoute: true, KillSwitch: true, KillSwitchEnabled: true},
	}
	down := snap
	down.Interfaces = append([]state.Interface(nil), snap.Interfaces...)
	down.Interfaces[0].Up = false
	down.Interfaces[3].Up = false
	down.WireGuard = state.WireGuard{Error: "no such device"}
	down.Routing.VPNRoute = false
	down.Modem = state.Modem{}
	down.Notifications = []state.Notification{{Time: now, Level: state.Info, Source: "sms", Text: "SMS from +989121234567"}}

	for name, tc := range map[string]struct {
		snap   state.Snapshot
		banner screen.Level
	}{
		"ok":   {snap, screen.Good},
		"down": {down, screen.Warning},
	} {
		m := screen.Build(tc.snap, config.Default(), now)
		d.Draw(func(c *Canvas) { drawStatus(c, m) })

		if got := d.image.RGBAAt(4, 4); got != levelColors[tc.banner] {
			t.Errorf("%s: banner color = %v, want %v", name, got, levelColors[tc.banner])
		}
		// Centre of each row's status dot.
		for i, r := range m.Rows {
			y := rowsTop + i*rowHeight + rowHeight/2
			if got := d.image.RGBAAt(marginX+dotRadius-1, y); got != levelColors[r.Level] {
				t.Errorf("%s: %s dot = %v, want %v", name, r.Label, got, levelColors[r.Level])
			}
		}

		// Nothing may be drawn in the footer's corner padding.
		footerTop := rowsTop + len(m.Rows)*rowHeight
		for y := footerTop + 1; y < d.image.Rect.Dy(); y++ {
			for x := range footerPadding {
				if got := d.image.RGBAAt(x, y); got != colorBackground {
					t.Fatalf("%s: footer pixel (%d,%d) = %v inside the padding", name, x, y, got)
				}
				if got := d.image.RGBAAt(d.image.Rect.Dx()-1-x, y); got != colorBackground {
					t.Fatalf("%s: footer pixel (%d,%d) = %v inside the padding", name, d.image.Rect.Dx()-1-x, y, got)
				}
			}
		}

		if prefix := os.Getenv("OPIZ3_SCREEN_PNG"); prefix != "" {
			f, e := os.Create(prefix + "-" + name + ".png")
			if e != nil {
				t.Fatal(e)
			}
			if e = png.Encode(f, d.image); e != nil {
				t.Fatal(e)
			}
			f.Close()
		}
	}
}

func newTestDisplay(t *testing.T) *Display {
	t.Helper()
	font, e := assets.Files.ReadFile(assets.FontPath)
	if e != nil {
		t.Fatal(e)
	}
	d := &Display{}
	if e := d.initFrame(font); e != nil {
		t.Fatal(e)
	}
	return d
}

// The traffic arrows must not move or change when the numbers change width.
func TestTrafficArrowsStayPut(t *testing.T) {
	d := newTestDisplay(t)
	const mid, baseline = 100, 106
	var l trafficLayout
	arrows := func(rx, tx uint64) []color.RGBA {
		d.Draw(func(c *Canvas) {
			c.Clear(colorBackground)
			l = layoutTraffic(c, c.Width()-marginX, mid)
			drawTraffic(c, c.Width()-marginX, mid, baseline, rx, tx)
		})
		var px []color.RGBA
		for _, x0 := range []int{l.rxArrow, l.txArrow} {
			for y := l.arrowY; y < l.arrowY+arrowSize; y++ {
				for x := x0; x < x0+arrowSize; x++ {
					px = append(px, d.image.RGBAAt(x, y))
				}
			}
		}
		return px
	}
	want := arrows(0, 0)
	if !slices.Contains(want, colorDetail) {
		t.Fatal("no arrow pixels drawn")
	}
	if slices.ContainsFunc(want, func(p color.RGBA) bool { return p != colorDetail && p != colorBackground }) {
		t.Fatal("arrows are not drawn in the text color")
	}
	for _, r := range [][2]uint64{{125, 12}, {400000, 36000}, {6 << 30, 900 << 20}, {3 << 40, 1}} {
		if got := arrows(r[0], r[1]); !slices.Equal(got, want) {
			t.Fatalf("arrows changed for %v", r)
		}
	}
	// The numbers must stay clear of the arrows.
	if l.rxText <= l.rxArrow+arrowSize || l.txText <= l.txArrow+arrowSize || l.txArrow <= l.rxText {
		t.Fatalf("overlapping layout: %+v", l)
	}
}

func TestSignalBars(t *testing.T) {
	d := newTestDisplay(t)
	const right, mid = 268, 100
	for filled := range screen.MaxBars + 1 {
		var left int
		d.Draw(func(c *Canvas) {
			c.Clear(colorBackground)
			left = drawBars(c, right, mid, filled)
		})
		if left != right-barsWidth {
			t.Fatalf("left = %d, want %d", left, right-barsWidth)
		}
		// Sample the bottom row of each bar, which every bar reaches.
		bottom := mid + (barMinHeight+(screen.MaxBars-1)*barStep)/2 - 1
		for i := range screen.MaxBars {
			want := colorBarEmpty
			if i < filled {
				want = colorDetail
			}
			if got := d.image.RGBAAt(left+i*(barWidth+barGap), bottom); got != want {
				t.Errorf("%d filled: bar %d = %v, want %v", filled, i, got, want)
			}
		}
	}
}

func TestDrawChannels(t *testing.T) {
	d := newTestDisplay(t)
	now := time.Now()
	snap := state.Snapshot{
		Monitor: state.Monitor{Page: state.PageChannels, Interface: "wlx10feed10c013", Present: true, Active: true,
			OwnChannel: 6, OwnSignal: -38, Recommended: 11, ScanChannel: 3,
			Networks: []state.MonitorNetwork{
				{SSID: "Neighbour-5G", Channel: 1, Signal: -45},
				{SSID: "MTN-Irancell-Home", Channel: 6, Signal: -52},
				{SSID: "", Channel: 1, Signal: -60},
				{SSID: "Cafe Guest", Channel: 3, Signal: -71},
				{SSID: "Office", Channel: 4, Signal: -78},
				{SSID: "Far away", Channel: 13, Signal: -84},
				{SSID: "Another", Channel: 9, Signal: -88},
			}},
	}
	absent := snap
	absent.Monitor = state.Monitor{Page: state.PageChannels, Interface: "wlx10feed10c013"}
	absent.Notifications = []state.Notification{{Time: now, Level: state.Info, Source: "sms", Text: "SMS from +989121234567"}}

	for name, s := range map[string]state.Snapshot{"channels": snap, "channels-absent": absent} {
		m := screen.BuildChannels(s, config.Default(), now)
		var c *Canvas
		d.Draw(func(cv *Canvas) { c = cv; drawChannels(cv, m) })
		if prefix := os.Getenv("OPIZ3_SCREEN_PNG"); prefix != "" {
			f, e := os.Create(prefix + "-" + name + ".png")
			if e != nil {
				t.Fatal(e)
			}
			if e = png.Encode(f, d.image); e != nil {
				t.Fatal(e)
			}
			f.Close()
		}
		// No VPN status dot in the header any more: none of its colors
		// (green, yellow, red) appear there.
		for y := range chHeaderHeight {
			for x := range d.image.Rect.Dx() {
				p := d.image.RGBAAt(x, y)
				for _, l := range []screen.Level{screen.Good, screen.Warning, screen.Critical} {
					if lc := levelColors[l]; p == lc {
						t.Fatalf("%s: status color %v in the header at (%d,%d)", name, p, x, y)
					}
				}
			}
		}
		if name != "channels" {
			continue
		}
		own := seriesColors[screen.SeriesOwn]
		if got := d.image.RGBAAt(spectrumX(c, 6), signalY(-38)); got != own {
			t.Errorf("own access point outline = %v, want %v", got, own)
		}
		// Inside the own hump, below its outline, the fill is blended over
		// the humps underneath, so it is neither background nor solid.
		if got := d.image.RGBAAt(spectrumX(c, 6), chBaseline-4); got == colorBackground || got == own {
			t.Errorf("hump fill = %v", got)
		}
		if got := d.image.RGBAAt(spectrumX(c, 3), chMarkerTop+chMarkerSize-1); got != colorScanMarker {
			t.Errorf("scan marker = %v", got)
		}
		green := levelColors[screen.Good]
		found := false
		x11 := spectrumX(c, 11)
		for y := chLabelY - 12; y <= chLabelY && !found; y++ {
			for x := x11 - 8; x <= x11+8; x++ {
				if d.image.RGBAAt(x, y) == green {
					found = true
					break
				}
			}
		}
		if !found {
			t.Error("best channel number not green")
		}
	}
}
