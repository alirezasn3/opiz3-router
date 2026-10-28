//go:build linux

package router

import (
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/config"
)

// LinkWatcher turns netlink updates into up/down changes of the router's own interfaces.
type LinkWatcher struct {
	cfg    config.Config
	status map[string]string
}

func NewLinkWatcher(cfg config.Config) *LinkWatcher {
	w := &LinkWatcher{cfg: cfg, status: map[string]string{}}
	links, _ := netlink.LinkList()
	for _, l := range links {
		w.status[l.Attrs().Name] = linkStatus(l.Attrs(), false)
	}
	return w
}

// Update returns the link's label and new status, and whether it changed.
func (w *LinkWatcher) Update(u netlink.LinkUpdate) (string, string, bool) {
	a := u.Attrs()
	label := LinkLabel(w.cfg, a.Name)
	if label == a.Name {
		return "", "", false // not one of the router's interfaces
	}
	status := linkStatus(a, u.Header.Type == unix.RTM_DELLINK)
	if w.status[a.Name] == status {
		return "", "", false
	}
	w.status[a.Name] = status
	return label, status, true
}

func linkStatus(a *netlink.LinkAttrs, removed bool) string {
	switch {
	case removed:
		return "removed"
	case a.Flags&net.FlagUp != 0 && (a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown):
		return "up"
	}
	return "down"
}

// LinkLabel names an interface the way the status screen does.
func LinkLabel(cfg config.Config, name string) string {
	switch name {
	case cfg.Interfaces.Ethernet:
		return "ETH"
	case cfg.Interfaces.USBModem:
		return "4G"
	case cfg.Interfaces.Wireless:
		return "WIFI"
	case cfg.WireGuard.Interface:
		return "VPN"
	case cfg.Interfaces.Bridge:
		return "BRIDGE"
	}
	return name
}
