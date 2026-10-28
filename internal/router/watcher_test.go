//go:build linux

package router

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"opiz3-router/internal/config"
)

func linkUpdate(name string, up bool, msgType uint16) netlink.LinkUpdate {
	attrs := netlink.LinkAttrs{Name: name, OperState: netlink.OperDown}
	if up {
		attrs.Flags, attrs.OperState = net.FlagUp, netlink.OperUp
	}
	u := netlink.LinkUpdate{Link: &netlink.Device{LinkAttrs: attrs}}
	u.Header.Type = msgType
	return u
}

func TestLinkWatcherReportsOnlyChanges(t *testing.T) {
	cfg := config.Default()
	w := &LinkWatcher{cfg: cfg, status: map[string]string{}}
	modem := cfg.Interfaces.USBModem

	steps := []struct {
		update      netlink.LinkUpdate
		wantChanged bool
		wantText    string
	}{
		{linkUpdate(modem, true, 0x10), true, "4G up"},
		{linkUpdate(modem, true, 0x10), false, ""}, // same state again
		{linkUpdate(modem, false, 0x10), true, "4G down"},
		{linkUpdate(modem, false, 0x11), true, "4G removed"},
		{linkUpdate("veth123", true, 0x10), false, ""}, // not ours
		{linkUpdate(cfg.Interfaces.Bridge, true, 0x10), true, "BRIDGE up"},
	}
	for i, s := range steps {
		label, status, changed := w.Update(s.update)
		if changed != s.wantChanged || (changed && label+" "+status != s.wantText) {
			t.Errorf("step %d: got %q %q changed=%v, want %q changed=%v", i, label, status, changed, s.wantText, s.wantChanged)
		}
	}
}
