package display

import (
	"context"
	"fmt"
	"image/color"
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

// MaxBars is the number of bars in the 4G signal meter.
const MaxBars = 5

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

// Colors for the status display.
var (
	colorBackground = color.RGBA{0, 0, 0, 255}
	colorText       = color.RGBA{255, 255, 255, 255}
	colorDetail     = color.RGBA{195, 194, 183, 255}
	colorMuted      = color.RGBA{137, 135, 129, 255}
	colorDivider    = color.RGBA{44, 44, 42, 255}
	colorBarEmpty   = color.RGBA{64, 63, 60, 255}

	levelColors = map[Level]color.RGBA{
		Off:      {90, 89, 85, 255},
		Good:     {12, 163, 12, 255},
		Warning:  {250, 178, 25, 255},
		Critical: {208, 59, 59, 255},
		Info:     {57, 135, 229, 255},
	}
)

// Layout of the 280x240 landscape frame.
const (
	bannerHeight = 46
	rowsTop      = 52
	rowHeight    = 36
	marginX      = 12
	dotRadius    = 7
	labelX       = 34
	arrowSize    = 9
	// The 4G signal meter: MaxBars bars growing from barMinHeight.
	barWidth     = 4
	barGap       = 2
	barMinHeight = 4
	barStep      = 3
	// footerPadding keeps the footer clear of the panel's rounded corners.
	footerPadding = marginX + 8
	footerDot     = 4
	// protectionSettle is how long a VPN state must last before it is logged.
	protectionSettle = 10 * time.Second
)

// StartStatusScreen redraws the status screen on the display every second until ctx is cancelled.
// It also logs changes of the VPN state once they have lasted protectionSettle.
func StartStatusScreen(ctx context.Context, d *Display, store *state.Store, cfg config.Config) {
	var logged, pending string
	var pendingSince time.Time
	ticker := time.NewTicker(time.Second)
	go func() {
		defer ticker.Stop()
		for {
			now := time.Now()
			snap := store.Snapshot()
			m := Build(snap, cfg, now)

			if d.IsPowered() {
				d.Draw(func(c *Canvas) { drawStatus(c, m) })
			}

			if m.Protection != pending {
				pending, pendingSince = m.Protection, now
			}
			if pending != logged && now.Sub(pendingSince) >= protectionSettle {
				logged = pending
				store.Notify(stateLevel(m.BannerLevel), "vpn", m.Protection)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func stateLevel(l Level) string {
	switch l {
	case Good:
		return state.Good
	case Warning:
		return state.Warning
	case Critical:
		return state.Critical
	}
	return state.Info
}

func drawStatus(c *Canvas, m Model) {
	c.Clear(colorBackground)
	w := c.Width()

	// Banner: the overall VPN state on a full-width colored bar.
	c.FillRect(0, 0, w, bannerHeight, levelColors[m.BannerLevel])
	bannerText := colorText
	if m.BannerLevel == Warning {
		bannerText = colorBackground
	}
	banner := fit(c, m.Banner, w-2*marginX, true)
	c.Text((w-c.TextWidth(banner, true))/2, (bannerHeight+c.Ascent(true))/2-2, banner, true, bannerText)

	// One row per link: a status dot, the link name and a detail on the right.
	for i, r := range m.Rows {
		top := rowsTop + i*rowHeight
		mid := top + rowHeight/2
		baseline := mid + c.Ascent(false)/2 - 1
		if i > 0 {
			c.FillRect(marginX, top, w-2*marginX, 1, colorDivider)
		}
		c.Circle(marginX+dotRadius-1, mid, dotRadius, levelColors[r.Level])
		labelEnd := c.Text(labelX, baseline, r.Label, false, colorText)

		right := w - marginX
		if r.HasTraffic {
			drawTraffic(c, right, mid, baseline, r.Rx, r.Tx)
			continue
		}
		if r.HasBars {
			right = drawBars(c, right, mid, r.Bars) - 10
		}
		detail := fit(c, r.Detail, right-labelEnd-16, false)
		c.Text(right-c.TextWidth(detail, false), baseline, detail, false, colorDetail)
	}

	footerTop := rowsTop + len(m.Rows)*rowHeight
	c.FillRect(0, footerTop, w, 1, colorDivider)
	footerMid := footerTop + (c.Height()-footerTop)/2
	x := footerPadding
	if m.FooterDot {
		c.Circle(x+footerDot, footerMid, footerDot, levelColors[m.FooterLevel])
		x += 2*footerDot + 8
	}
	footer := fit(c, m.Footer, w-footerPadding-x, false)
	c.Text(x, footerMid+c.Ascent(false)/2-1, footer, false, colorMuted)
}

type trafficLayout struct {
	rxArrow, rxText, txArrow, txText int
	arrowY                           int
}

func layoutTraffic(c *Canvas, right, mid int) trafficLayout {
	slot := c.TextWidth("0000", false)
	var l trafficLayout
	l.arrowY = mid - arrowSize/2
	l.txText = right - slot
	l.txArrow = l.txText - arrowSize - 4
	l.rxText = l.txArrow - 12 - slot
	l.rxArrow = l.rxText - arrowSize - 4
	return l
}

func drawTraffic(c *Canvas, right, mid, baseline int, rx, tx uint64) {
	l := layoutTraffic(c, right, mid)
	c.Triangle(l.rxArrow, l.arrowY, arrowSize, false, colorDetail)
	c.Text(l.rxText, baseline, FormatBytes(rx), false, colorDetail)
	c.Triangle(l.txArrow, l.arrowY, arrowSize, true, colorDetail)
	c.Text(l.txText, baseline, FormatBytes(tx), false, colorDetail)
}

const barsWidth = MaxBars*barWidth + (MaxBars-1)*barGap

func drawBars(c *Canvas, right, mid, filled int) int {
	left := right - barsWidth
	tallest := barMinHeight + (MaxBars-1)*barStep
	bottom := mid + tallest/2
	for i := range MaxBars {
		h := barMinHeight + i*barStep
		col := colorBarEmpty
		if i < filled {
			col = colorDetail
		}
		c.FillRect(left+i*(barWidth+barGap), bottom-h, barWidth, h, col)
	}
	return left
}

func fit(c *Canvas, s string, width int, large bool) string {
	if c.TextWidth(s, large) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && c.TextWidth(string(r)+"..", large) > width {
		r = r[:len(r)-1]
	}
	return string(r) + ".."
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

func footerNotification(log []state.Notification, now time.Time) (state.Notification, bool) {
	for _, n := range slices.Backward(log) {
		age := now.Sub(n.Time)
		if age < visibleFor(n) {
			return n, true
		}
		if age >= SMSVisible {
			break
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
	case !r.Synced:
		return Critical, "NOT ENFORCED", "mini-net-man is not holding the routing"
	case r.VPN == "down" || !wg.Up:
		if r.KillSwitch {
			return Warning, "VPN DOWN - BLOCKED", "VPN down, kill switch blocking LAN traffic"
		}
		return Critical, "UNPROTECTED", "VPN down, LAN traffic unprotected"
	case handshakeFresh(wg, now):
		return Good, "PROTECTED", "VPN protected"
	default:
		return Warning, "NO HANDSHAKE", "VPN has no recent handshake"
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

func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if ip, _, e := net.ParseCIDR(a); e == nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

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
	if sig := strings.Join(strings.Fields(m.SignalStrength), ""); sig != "" {
		parts = append(parts, sig)
	}
	detail := strings.Join(parts, " ")
	if detail == "" {
		detail = "connected"
	}
	return Row{Label: "4G", Level: Good, Detail: detail, HasBars: true, Bars: min(max(int(m.SignalLevel), 0), MaxBars)}
}

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

func bridgeRow(i state.Interface, online int) Row {
	if !i.Present || !i.Up {
		return Row{Label: "BRIDGE", Level: Critical, Detail: "down"}
	}
	return Row{Label: "BRIDGE", Level: Good, Detail: fmt.Sprintf("%d online", online)}
}

func FormatBytes(b uint64) string {
	v := float64(b)
	units := []string{"B", "K", "M", "G", "T", "P", "E"}
	i := 0
	for v >= 999.5 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i > 0 && v < 9.95 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + units[i]
	}
	return fmt.Sprintf("%.0f", v) + units[i]
}
