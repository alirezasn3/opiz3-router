package screen

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

// Series of a hump on the channels page, which picks its color.
const (
	SeriesOther = iota // every network not singled out
	SeriesOwn          // the router's own access point
	// SeriesTop is the strongest neighbour; the next ones follow it.
	SeriesTop
)

// TopNetworks is how many of the strongest neighbours get a color and label.
const TopNetworks = 3

// Hump is one network on the channels page: a 2.4 GHz network occupies about
// 22 MHz, so it is drawn as a hump over its channel and two on either side,
// as high as its signal is strong.
type Hump struct {
	Channel int
	Signal  int // dBm
	Label   string
	Series  int
}

// ChannelsModel is the display page that shows the 2.4 GHz spectrum.
type ChannelsModel struct {
	Title   string
	Summary string
	// Humps are in drawing order: weakest first, the own access point last.
	Humps []Hump
	// Own, Best and Scan are channels: the router's, the recommended one and
	// the one being listened to now; 0 if unknown.
	Own, Best, Scan int
	// Message replaces the chart while the adapter is not capturing.
	Message     string
	Footer      string
	FooterDot   bool
	FooterLevel Level
}

// unknownSignal stands in for networks whose signal the adapter did not report.
const unknownSignal = -90

func BuildChannels(snap state.Snapshot, cfg config.Config, now time.Time) ChannelsModel {
	mon := snap.Monitor
	m := ChannelsModel{Title: "Wi-Fi channels", Own: mon.OwnChannel, Best: mon.Recommended, Scan: mon.ScanChannel}

	switch {
	case !mon.Present:
		m.Message = fmt.Sprintf("Plug in the Wi-Fi adapter (%s)", mon.Interface)
	case !mon.Active && mon.Error != "":
		m.Message = "Adapter error: " + mon.Error
	case !mon.Active:
		m.Message = "Starting the adapter..."
	default:
		m.Summary = plural(len(mon.Networks), "network")
		m.Humps = humps(mon)
	}

	// Notifications such as a new SMS still show here.
	if n, ok := footerNotification(snap.Notifications, now); ok {
		m.Footer, m.FooterDot, m.FooterLevel = n.Text, true, NotificationLevel(n.Level)
		return m
	}
	switch {
	case !mon.Active || mon.Recommended == 0:
	case mon.OwnChannel == 0:
		m.Footer = fmt.Sprintf("Best channel: %d", mon.Recommended)
	case mon.OwnChannel == mon.Recommended:
		m.Footer = fmt.Sprintf("Your channel %d is best", mon.OwnChannel)
	default:
		m.Footer = fmt.Sprintf("Best: %d  Yours: %d", mon.Recommended, mon.OwnChannel)
	}
	return m
}

func humps(mon state.Monitor) []Hump {
	var others, top []Hump
	nets := slices.Clone(mon.Networks)
	for i := range nets {
		if nets[i].Signal == 0 {
			nets[i].Signal = unknownSignal
		}
	}
	slices.SortStableFunc(nets, func(a, b state.MonitorNetwork) int { return cmp.Compare(b.Signal, a.Signal) })
	for i, n := range nets {
		h := Hump{Channel: n.Channel, Signal: n.Signal}
		if i < TopNetworks {
			h.Series, h.Label = SeriesTop+i, n.SSID
			if h.Label == "" {
				h.Label = "hidden"
			}
			top = append(top, h)
		} else {
			others = append(others, h)
		}
	}
	// Weakest first, so stronger humps are drawn over weaker ones.
	slices.Reverse(others)
	slices.Reverse(top)
	out := append(others, top...)
	if mon.OwnChannel != 0 && mon.OwnSignal != 0 {
		out = append(out, Hump{Channel: mon.OwnChannel, Signal: mon.OwnSignal, Label: "you", Series: SeriesOwn})
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
