//go:build linux

package main

import (
	"fmt"

	"opiz3-router/internal/screen"
)

const (
	networkHeaderHeight = 34
	networkRowsTop      = 43
	networkRowHeight    = 21
	networkFooterTop    = 220
)

func drawNetwork(c *Canvas, m screen.NetworkModel) {
	c.Clear(colorBackground)
	w := c.Width()
	baseline := networkHeaderHeight/2 + c.Ascent(false)/2 - 1
	c.Text(marginX, baseline, m.Title, false, colorText)
	if m.Summary != "" {
		c.Text(w-marginX-c.TextWidth(m.Summary, false), baseline, m.Summary, false, colorMuted)
	}
	c.FillRect(0, networkHeaderHeight, w, 1, colorDivider)

	if m.Message != "" {
		lines := wrap(c, m.Message, w-2*footerPadding, 4)
		lineHeight := c.Ascent(false) + 8
		y := 105 - (len(lines)-1)*lineHeight/2
		for i, line := range lines {
			c.Text((w-c.TextWidth(line, false))/2, y+i*lineHeight, line, false, colorDetail)
		}
	} else {
		visible := (networkFooterTop - networkRowsTop) / networkRowHeight
		for i, d := range m.Devices {
			if i >= visible {
				break
			}
			top := networkRowsTop + i*networkRowHeight
			if i > 0 {
				c.FillRect(marginX, top-5, w-2*marginX, 1, colorDivider)
			}
			name := d.Name
			if name == "" {
				name = d.Manufacturer
			}
			if name == "" {
				name = "Unknown"
			}
			name = fitSmall(c, name, w-2*marginX-c.SmallTextWidth(d.IP)-12)
			baseline := top + c.SmallAscent()
			c.SmallText(marginX, baseline, name, colorText)
			c.SmallText(w-marginX-c.SmallTextWidth(d.IP), baseline, d.IP, colorDetail)
		}
		if len(m.Devices) == 0 {
			c.SmallText(marginX, 105, "No devices found", colorDetail)
		}
		if extra := len(m.Devices) - visible; extra > 0 {
			m.Footer = fmt.Sprintf("%s +%d more", m.Footer, extra)
		}
	}

	footerTop := networkFooterTop
	c.FillRect(0, footerTop, w, 1, colorDivider)
	footerMid := footerTop + (c.Height()-footerTop)/2
	x := footerPadding
	if m.FooterDot {
		c.Circle(x+footerDot, footerMid, footerDot, levelColors[m.FooterLevel])
		x += 2*footerDot + 8
	}
	c.Text(x, footerMid+c.Ascent(false)/2-1, fit(c, m.Footer, w-footerPadding-x, false), false, colorMuted)
}

func fitSmall(c *Canvas, s string, width int) string {
	if c.SmallTextWidth(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && c.SmallTextWidth(string(r)+"..") > width {
		r = r[:len(r)-1]
	}
	return string(r) + ".."
}
