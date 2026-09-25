//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// masquerade the outgoing traffic from the interface
func masqueradeInterface(name string) error {
	// create new table
	ipt, e := iptables.New()
	if e != nil {
		return e
	}

	// append the rule if it doesn't exist already
	e = ipt.AppendUnique("nat", "POSTROUTING", "-o", name, "-j", "MASQUERADE")
	if e != nil {
		return e
	}

	return nil
}

// get cidr notation from of an interface by it's name
func getCIDRFromInterfaceName(name string) (*net.IP, *net.IPNet, error) {
	// get interface
	iface, e := net.InterfaceByName(name)
	if e != nil {
		return nil, nil, e
	}

	// get the assigned addresses of the interface
	addresses, e := iface.Addrs()
	if e != nil {
		return nil, nil, e
	}

	if len(addresses) == 0 {
		return nil, nil, fmt.Errorf("%s has no address", name)
	}

	// Prefer an IPv4 address because policy routing below is IPv4-only.
	for _, address := range addresses {
		ip, ipnet, e := net.ParseCIDR(address.String())
		if e == nil && ip.To4() != nil {
			return &ip, ipnet, nil
		}
	}

	return nil, nil, fmt.Errorf("%s has no IPv4 address", name)
}

// killSwitchMetric is the metric of the blackhole default route in the VPN
// table. It is higher than the VPN route, so it only takes effect when the VPN
// route is gone.
const killSwitchMetric = 4096

var defaultIPv4 = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}

func isDefaultRoute(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0
}

// setupVPNRouting steers traffic that arrives on the bridge into table, whose
// default route (added by ensureVPNRoute) is the VPN interface. Matching on the
// incoming interface rather than the LAN subnet keeps the router's own replies
// to LAN clients (SSH, the web panel) in the main table.
//
// With killSwitch, a blackhole route in the same table drops LAN traffic while
// the VPN route is missing. Without it, LAN traffic falls through to the main
// table and goes out of the WAN unencrypted until the VPN is back.
func setupVPNRouting(bridge string, table, priority int, killSwitch bool) error {
	if e := ensureBridgeRule(bridge, table, priority); e != nil {
		return fmt.Errorf("policy rule: %w", e)
	}
	if killSwitch {
		if e := netlink.RouteReplace(killSwitchRoute(table)); e != nil {
			return fmt.Errorf("kill switch route: %w", e)
		}
		return nil
	}
	// Routes outlive the service, so drop one left by an earlier run.
	if e := netlink.RouteDel(killSwitchRoute(table)); e != nil && !errors.Is(e, syscall.ESRCH) {
		return fmt.Errorf("remove kill switch route: %w", e)
	}
	return nil
}

func findBridgeRule(bridge string, table int) (bool, error) {
	rules, e := netlink.RuleList(netlink.FAMILY_V4)
	if e != nil {
		return false, e
	}
	for _, r := range rules {
		if r.Table == table && r.IifName == bridge {
			return true, nil
		}
	}
	return false, nil
}

func ensureBridgeRule(bridge string, table, priority int) error {
	// The kernel only reports duplicates when every attribute matches, so look
	// the rule up instead of relying on EEXIST.
	found, e := findBridgeRule(bridge, table)
	if e != nil || found {
		return e
	}
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.IifName = bridge
	rule.Table = table
	rule.Priority = priority
	if e = netlink.RuleAdd(rule); e != nil && !errors.Is(e, syscall.EEXIST) {
		return e
	}
	return nil
}

func killSwitchRoute(table int) *netlink.Route {
	return &netlink.Route{
		Dst:      defaultIPv4,
		Table:    table,
		Type:     unix.RTN_BLACKHOLE,
		Priority: killSwitchMetric,
	}
}

func ensureVPNRoute(vpn string, table int) error {
	link, e := netlink.LinkByName(vpn)
	if e != nil {
		return e
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       defaultIPv4,
		Table:     table,
	})
}

type vpnRoutingStatus struct {
	rule, vpnRoute, killSwitch bool
	// vpnUp is whether the VPN interface exists and is up, i.e. whether the
	// VPN route can exist at all.
	vpnUp bool
}

func getVPNRoutingStatus(bridge, vpn string, table int) (vpnRoutingStatus, error) {
	var s vpnRoutingStatus
	var e error
	if s.rule, e = findBridgeRule(bridge, table); e != nil {
		return s, e
	}
	routes, e := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if e != nil {
		return s, e
	}
	vpnIndex := -1
	if link, e := netlink.LinkByName(vpn); e == nil {
		vpnIndex = link.Attrs().Index
		s.vpnUp = link.Attrs().Flags&net.FlagUp != 0
	}
	for _, r := range routes {
		if !isDefaultRoute(r.Dst) {
			continue
		}
		if r.Type == unix.RTN_BLACKHOLE {
			s.killSwitch = true
		} else if r.LinkIndex == vpnIndex {
			s.vpnRoute = true
		}
	}
	return s, nil
}

const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

func enableIPForwarding() error {
	return os.WriteFile(ipForwardPath, []byte("1"), 0o644)
}

func ipForwardingEnabled() (bool, error) {
	data, e := os.ReadFile(ipForwardPath)
	if e != nil {
		return false, e
	}
	return strings.TrimSpace(string(data)) == "1", nil
}
