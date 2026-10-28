package display

import (
	"image/color"
	"slices"
	"testing"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func newTestDisplay(t *testing.T) *Display {
	t.Helper()
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

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
		Routing:   state.Routing{Synced: true, VPN: "up", KillSwitch: true},
	}
	down := snap
	down.Interfaces = append([]state.Interface(nil), snap.Interfaces...)
	down.Interfaces[0].Up = false
	down.Interfaces[3].Up = false
	down.WireGuard = state.WireGuard{Error: "no such device"}
	down.Routing.VPN = "down"
	down.Modem = state.Modem{}
	down.Notifications = []state.Notification{{Time: now, Level: state.Info, Source: "sms", Text: "SMS from +989121234567"}}

	for name, tc := range map[string]struct {
		snap   state.Snapshot
		banner Level
	}{
		"ok":   {snap, Good},
		"down": {down, Warning},
	} {
		m := Build(tc.snap, config.Default(), now)
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
	}
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
	for filled := range MaxBars + 1 {
		var left int
		d.Draw(func(c *Canvas) {
			c.Clear(colorBackground)
			left = drawBars(c, right, mid, filled)
		})
		if left != right-barsWidth {
			t.Fatalf("left = %d, want %d", left, right-barsWidth)
		}
		// Sample the bottom row of each bar, which every bar reaches.
		bottom := mid + (barMinHeight+(MaxBars-1)*barStep)/2 - 1
		for i := range MaxBars {
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

func TestFit(t *testing.T) {
	d := newTestDisplay(t)
	d.Draw(func(c *Canvas) {
		s := "this is a very long string that will not fit"
		fitted := fit(c, s, 50, false)
		if len(fitted) >= len(s) {
			t.Errorf("fit did not shorten string: %q", fitted)
		}
		if c.TextWidth(fitted, false) > 50 {
			t.Errorf("fitted text width %d > 50", c.TextWidth(fitted, false))
		}
	})
}
