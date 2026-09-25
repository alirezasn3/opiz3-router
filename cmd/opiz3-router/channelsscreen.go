//go:build linux

package main

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

	"opiz3-router/internal/screen"
)

// Layout of the channels page.
const (
	chHeaderHeight = 34
	chChartTop     = 42
	chBaseline     = 172 // the 0 line of the spectrum
	chChartLeft    = 30  // room for the dBm labels
	chMarkerTop    = chBaseline + 3
	chMarkerSize   = 7
	chLabelY       = 194 // baseline of the channel numbers
	chFooterTop    = 200
	// Signals are drawn from chSignalFloor (flat) to chSignalCeil (full).
	chSignalFloor = -95
	chSignalCeil  = -30
	chFillAlpha   = 56
	chLabelChars  = 10
)

var (
	// Series colors: dark-surface steps of a validated categorical palette,
	// in fixed order; everything not singled out stays gray.
	seriesColors = map[int]color.RGBA{
		screen.SeriesOther:   {150, 149, 142, 255},
		screen.SeriesOwn:     {57, 135, 229, 255},
		screen.SeriesTop:     {217, 89, 38, 255},
		screen.SeriesTop + 1: {25, 158, 112, 255},
		screen.SeriesTop + 2: {144, 133, 233, 255},
	}
	colorScanMarker = colorText
)

func drawChannels(c *Canvas, m screen.ChannelsModel) {
	c.Clear(colorBackground)
	w := c.Width()

	mid := chHeaderHeight / 2
	baseline := mid + c.Ascent(false)/2 - 1
	c.Text(marginX, baseline, m.Title, false, colorText)
	if m.Summary != "" {
		c.Text(w-marginX-c.TextWidth(m.Summary, false), baseline, m.Summary, false, colorMuted)
	}
	c.FillRect(0, chHeaderHeight, w, 1, colorDivider)

	if m.Message != "" {
		lines := wrap(c, m.Message, w-2*footerPadding, 3)
		lineHeight := c.Ascent(false) + 8
		y := (chChartTop+chBaseline)/2 - (len(lines)-1)*lineHeight/2 + c.Ascent(false)/2
		for i, l := range lines {
			c.Text((w-c.TextWidth(l, false))/2, y+i*lineHeight, l, false, colorDetail)
		}
	} else {
		drawSpectrum(c, m)
	}

	c.FillRect(0, chFooterTop, w, 1, colorDivider)
	footerMid := chFooterTop + (c.Height()-chFooterTop)/2
	x := footerPadding
	if m.FooterDot {
		c.Circle(x+footerDot, footerMid, footerDot, levelColors[m.FooterLevel])
		x += 2*footerDot + 8
	}
	footer := fit(c, m.Footer, w-footerPadding-x, false)
	c.Text(x, footerMid+c.Ascent(false)/2-1, footer, false, colorMuted)
}

// spectrumX maps a (fractional) channel to x. The axis runs from channel -1
// to 15 so the humps of channels 1 and 13 fit.
func spectrumX(c *Canvas, ch float64) int {
	right := c.Width() - marginX
	return chChartLeft + int(math.Round((ch+1)/16*float64(right-chChartLeft)))
}

// signalY maps a signal in dBm to the height of a hump's top.
func signalY(dbm int) int {
	frac := float64(dbm-chSignalFloor) / float64(chSignalCeil-chSignalFloor)
	frac = min(1, max(0.04, frac))
	// Leave room above the strongest hump for its label.
	return chBaseline - int(math.Round(frac*float64(chBaseline-chChartTop-14)))
}

func drawSpectrum(c *Canvas, m screen.ChannelsModel) {
	right := c.Width() - marginX

	// dBm grid.
	for _, dbm := range []int{-40, -60, -80} {
		y := signalY(dbm)
		c.FillRect(chChartLeft, y, right-chChartLeft, 1, colorDivider)
		c.SmallTextCentered(chChartLeft/2+2, y+4, strconv.Itoa(dbm), colorMuted)
	}

	for _, h := range m.Humps {
		drawHump(c, h)
	}
	c.FillRect(chChartLeft, chBaseline, right-chChartLeft, 1, colorBarEmpty)

	// Labels over the peaks, the own access point and strongest first. A
	// label that collides with one already placed tries just inside its
	// hump, and is left out if that collides too.
	var placed []image.Rectangle
	for i := len(m.Humps) - 1; i >= 0; i-- {
		h := m.Humps[i]
		if h.Label == "" {
			continue
		}
		label := h.Label
		if r := []rune(label); len(r) > chLabelChars {
			label = string(r[:chLabelChars-1]) + "."
		}
		half := c.SmallTextWidth(label)/2 + 1
		cx := min(max(spectrumX(c, float64(h.Channel)), chChartLeft+half+4), right-half)
		top := signalY(h.Signal)
		for _, y := range []int{max(top-4, chChartTop+10), top + 15} {
			box := image.Rect(cx-half-2, y-11, cx+half+2, y+2)
			if y > chBaseline-2 || anyOverlap(placed, box) {
				continue
			}
			placed = append(placed, box)
			c.SmallTextCentered(cx, y, label, colorText)
			break
		}
	}

	// Channel numbers: the best one green, your own blue, and a marker under
	// the channel being listened to right now. Those three are placed first;
	// the other numbers fill in where they do not collide.
	type axisLabel struct {
		ch  int
		col color.RGBA
	}
	var labels []axisLabel
	if m.Best != 0 {
		labels = append(labels, axisLabel{m.Best, levelColors[screen.Good]})
	}
	if m.Own != 0 && m.Own != m.Best {
		labels = append(labels, axisLabel{m.Own, seriesColors[screen.SeriesOwn]})
	}
	if m.Scan != 0 && m.Scan != m.Best && m.Scan != m.Own {
		labels = append(labels, axisLabel{m.Scan, colorText})
	}
	for ch := 1; ch <= 13; ch++ {
		if ch != m.Best && ch != m.Own && ch != m.Scan {
			labels = append(labels, axisLabel{ch, colorMuted})
		}
	}
	var taken []image.Rectangle
	for _, l := range labels {
		x := spectrumX(c, float64(l.ch))
		text := strconv.Itoa(l.ch)
		half := c.SmallTextWidth(text)/2 + 1
		box := image.Rect(x-half, chLabelY-11, x+half, chLabelY+2)
		if anyOverlap(taken, box) {
			continue
		}
		taken = append(taken, box)
		c.SmallTextCentered(x, chLabelY, text, l.col)
	}
	if m.Scan != 0 {
		x := spectrumX(c, float64(m.Scan))
		c.Triangle(x-chMarkerSize/2, chMarkerTop, chMarkerSize, true, colorScanMarker)
	}
}

// drawHump draws a network as a flat-topped hump over its channel and two on
// either side (22 MHz), filled faintly so overlapping networks show through.
func drawHump(c *Canvas, h screen.Hump) {
	col := seriesColors[h.Series]
	if _, ok := seriesColors[h.Series]; !ok {
		col = seriesColors[screen.SeriesOther]
	}
	cx := spectrumX(c, float64(h.Channel))
	half := spectrumX(c, float64(h.Channel)+2) - cx
	height := chBaseline - signalY(h.Signal)
	prevY := chBaseline
	for x := cx - half; x <= cx+half; x++ {
		u := float64(x-cx) / float64(half)
		y := chBaseline - int(math.Round((1-math.Pow(u, 4))*float64(height)))
		c.Blend(x, y, 1, chBaseline-y, col, chFillAlpha)
		// Outline: join to the previous column so steep sides have no gaps.
		top, bottom := min(y, prevY), max(y, prevY)
		c.FillRect(x, top, 1, max(1, bottom-top+1), col)
		prevY = y
	}
}

func anyOverlap(rs []image.Rectangle, r image.Rectangle) bool {
	for _, o := range rs {
		if o.Overlaps(r) {
			return true
		}
	}
	return false
}

// wrap splits s into at most maxLines lines no wider than width, shortening
// the last line if the text does not fit.
func wrap(c *Canvas, s string, width, maxLines int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		next := word
		if line != "" {
			next = line + " " + word
		}
		if line == "" || c.TextWidth(next, false) <= width {
			line = next
			continue
		}
		lines = append(lines, line)
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) > maxLines {
		lines = append(lines[:maxLines-1], strings.Join(lines[maxLines-1:], " "))
	}
	for i := range lines {
		lines[i] = fit(c, lines[i], width, false)
	}
	return lines
}
