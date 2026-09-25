//go:build linux

package main

import (
	"context"
	"image/color"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/screen"
	"opiz3-router/internal/state"
)

var (
	colorBackground = color.RGBA{0, 0, 0, 255}
	colorText       = color.RGBA{255, 255, 255, 255}
	colorDetail     = color.RGBA{195, 194, 183, 255}
	colorMuted      = color.RGBA{137, 135, 129, 255}
	colorDivider    = color.RGBA{44, 44, 42, 255}
	colorBarEmpty   = color.RGBA{64, 63, 60, 255}

	levelColors = map[screen.Level]color.RGBA{
		screen.Off:      {90, 89, 85, 255},
		screen.Good:     {12, 163, 12, 255},
		screen.Warning:  {250, 178, 25, 255},
		screen.Critical: {208, 59, 59, 255},
		screen.Info:     {57, 135, 229, 255},
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
	// footerPadding keeps the footer clear of the panel's rounded corners,
	// which cut deeper into the bottom line than into the rows above.
	footerPadding = marginX + 8
	footerDot     = 4
	// protectionSettle is how long a VPN state must last before it is
	// logged, so a brief flap between states does not flood the log.
	protectionSettle = 10 * time.Second
)

// startStatusScreen replaces the boot log with the status screen, or the
// channels page when that is chosen, and redraws it every second until ctx is
// cancelled. It also logs changes of the VPN
// state once they have lasted protectionSettle.
func startStatusScreen(ctx context.Context, d *Display, store *state.Store, cfg config.Config) {
	var logged, pending string
	var pendingSince time.Time
	go every(ctx, time.Second, func() {
		now := time.Now()
		snap := store.Snapshot()
		m := screen.Build(snap, cfg, now)
		if snap.Monitor.Page == state.PageChannels {
			ch := screen.BuildChannels(snap, cfg, now)
			d.Draw(func(c *Canvas) { drawChannels(c, ch) })
		} else if snap.Monitor.Page == state.PageNetwork {
			n := screen.BuildNetwork(snap, cfg, now)
			d.Draw(func(c *Canvas) { drawNetwork(c, n) })
		} else {
			d.Draw(func(c *Canvas) { drawStatus(c, m) })
		}

		if m.Protection != pending {
			pending, pendingSince = m.Protection, now
		}
		if pending != logged && now.Sub(pendingSince) >= protectionSettle {
			logged = pending
			store.Notify(stateLevel(m.BannerLevel), "vpn", m.Protection)
		}
	})
}

// stateLevel maps a screen level back to a notification level.
func stateLevel(l screen.Level) string {
	switch l {
	case screen.Good:
		return state.Good
	case screen.Warning:
		return state.Warning
	case screen.Critical:
		return state.Critical
	}
	return state.Info
}

func drawStatus(c *Canvas, m screen.Model) {
	c.Clear(colorBackground)
	w := c.Width()

	// Banner: the overall VPN state on a full-width colored bar. Yellow is
	// too light for white text, so it gets black text.
	c.FillRect(0, 0, w, bannerHeight, levelColors[m.BannerLevel])
	bannerText := colorText
	if m.BannerLevel == screen.Warning {
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

// trafficLayout is where drawTraffic puts the arrows and numbers. Each number
// sits in a slot as wide as the longest FormatBytes output, so nothing moves
// when the numbers change.
type trafficLayout struct {
	rxArrow, rxText, txArrow, txText int // x positions
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

// drawTraffic draws "▼ rx  ▲ tx" (bytes received and sent) ending at right.
// The font has no arrow glyphs, so the arrows are drawn as triangles, in the
// same color as the numbers.
func drawTraffic(c *Canvas, right, mid, baseline int, rx, tx uint64) {
	l := layoutTraffic(c, right, mid)
	c.Triangle(l.rxArrow, l.arrowY, arrowSize, false, colorDetail)
	c.Text(l.rxText, baseline, screen.FormatBytes(rx), false, colorDetail)
	c.Triangle(l.txArrow, l.arrowY, arrowSize, true, colorDetail)
	c.Text(l.txText, baseline, screen.FormatBytes(tx), false, colorDetail)
}

// barsWidth is the width of the signal meter.
const barsWidth = screen.MaxBars*barWidth + (screen.MaxBars-1)*barGap

// drawBars draws a signal meter ending at right, with filled of its bars lit,
// and returns its left edge.
func drawBars(c *Canvas, right, mid, filled int) int {
	left := right - barsWidth
	tallest := barMinHeight + (screen.MaxBars-1)*barStep
	bottom := mid + tallest/2
	for i := range screen.MaxBars {
		h := barMinHeight + i*barStep
		col := colorBarEmpty
		if i < filled {
			col = colorDetail
		}
		c.FillRect(left+i*(barWidth+barGap), bottom-h, barWidth, h, col)
	}
	return left
}

// fit shortens s with ".." until it is at most width pixels wide.
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
