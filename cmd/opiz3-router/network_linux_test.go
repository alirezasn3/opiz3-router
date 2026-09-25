//go:build linux

package main

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

// inNewNetns runs fn in a fresh network namespace on a locked OS thread, so
// the test never touches the host's routing. It skips without privileges.
func inNewNetns(t *testing.T, fn func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for network namespaces")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, e := netns.Get()
	if e != nil {
		t.Skipf("netns unavailable: %v", e)
	}
	defer orig.Close()
	ns, e := netns.New()
	if e != nil {
		t.Skipf("cannot create netns: %v", e)
	}
	defer ns.Close()
	defer netns.Set(orig)
	fn()
}

// addLink creates one end of a veth pair (both ends up, so it has carrier).
func addLink(t *testing.T, name, cidr string) {
	t.Helper()
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: name + "-peer"}
	if e := netlink.LinkAdd(link); e != nil {
		t.Skipf("cannot create veth links: %v", e)
	}
	if peer, e := netlink.LinkByName(name + "-peer"); e == nil {
		_ = netlink.LinkSetUp(peer)
	}
	if cidr != "" {
		addr, _ := netlink.ParseAddr(cidr)
		if e := netlink.AddrAdd(link, addr); e != nil {
			t.Fatalf("addr %s: %v", name, e)
		}
	}
	if e := netlink.LinkSetUp(link); e != nil {
		t.Fatalf("up %s: %v", name, e)
	}
}

// lookupFromBridge resolves dst as if a packet arrived on the bridge from src.
func lookupFromBridge(t *testing.T, bridge string, src, dst net.IP) (netlink.Route, error) {
	t.Helper()
	routes, e := netlink.RouteGetWithOptions(dst, &netlink.RouteGetOptions{Iif: bridge, SrcAddr: src})
	if e != nil {
		return netlink.Route{}, e
	}
	return routes[0], nil
}

func TestVPNRouting(t *testing.T) {
	inNewNetns(t, func() {
		const table, priority = 200, 1000
		lo, _ := netlink.LinkByName("lo")
		_ = netlink.LinkSetUp(lo)
		addLink(t, "br0", "172.16.0.1/12")
		addLink(t, "end0", "192.168.1.44/24")
		addLink(t, "wg0", "10.8.0.2/32")
		// The WAN default route in the main table, where leaked traffic would go.
		end0, _ := netlink.LinkByName("end0")
		gw := net.ParseIP("192.168.1.1")
		if e := netlink.RouteAdd(&netlink.Route{LinkIndex: end0.Attrs().Index, Dst: defaultIPv4, Gw: gw}); e != nil {
			t.Fatal(e)
		}
		if e := enableIPForwarding(); e != nil {
			t.Skipf("cannot enable forwarding in netns: %v", e)
		}

		for range 2 { // idempotent
			if e := setupVPNRouting("br0", table, priority, true); e != nil {
				t.Fatal(e)
			}
			if e := ensureVPNRoute("wg0", table); e != nil {
				t.Fatal(e)
			}
		}
		rules, _ := netlink.RuleList(netlink.FAMILY_V4)
		var bridgeRules int
		for _, r := range rules {
			if r.Table == table && r.IifName == "br0" {
				bridgeRules++
			}
		}
		if bridgeRules != 1 {
			t.Fatalf("got %d bridge rules, want 1", bridgeRules)
		}
		st, e := getVPNRoutingStatus("br0", "wg0", table)
		if e != nil || !st.rule || !st.vpnRoute || !st.killSwitch {
			t.Fatalf("status = %+v, %v", st, e)
		}

		client, internet := net.ParseIP("172.16.0.23"), net.ParseIP("1.1.1.1")
		wg0, _ := netlink.LinkByName("wg0")
		r, e := lookupFromBridge(t, "br0", client, internet)
		if e != nil || r.LinkIndex != wg0.Attrs().Index {
			t.Fatalf("LAN traffic not routed via wg0: %+v, %v", r, e)
		}

		// The router's own replies to a LAN client must stay on the bridge.
		br0, _ := netlink.LinkByName("br0")
		routes, e := netlink.RouteGetWithOptions(client, &netlink.RouteGetOptions{SrcAddr: net.ParseIP("172.16.0.1")})
		if e != nil || routes[0].LinkIndex != br0.Attrs().Index {
			t.Fatalf("reply to LAN client not sent via br0: %+v, %v", routes, e)
		}

		// With the tunnel gone, LAN traffic must hit the kill switch rather
		// than fall through to the WAN default route.
		if e := netlink.LinkDel(wg0); e != nil {
			t.Fatal(e)
		}
		st, _ = getVPNRoutingStatus("br0", "wg0", table)
		if st.vpnRoute || !st.killSwitch {
			t.Fatalf("status after wg0 removal = %+v", st)
		}
		// The kernel rejects a lookup that ends in a blackhole route.
		r, e = lookupFromBridge(t, "br0", client, internet)
		if e == nil && r.Type != unix.RTN_BLACKHOLE {
			t.Fatalf("LAN traffic not blackholed when wg0 is gone: %+v", r)
		}

		// Turning the kill switch off removes the blackhole, so LAN traffic
		// falls through to the WAN while the tunnel is gone.
		for range 2 { // idempotent
			if e := setupVPNRouting("br0", table, priority, false); e != nil {
				t.Fatal(e)
			}
		}
		st, _ = getVPNRoutingStatus("br0", "wg0", table)
		if !st.rule || st.killSwitch {
			t.Fatalf("status with kill switch off = %+v", st)
		}
		r, e = lookupFromBridge(t, "br0", client, internet)
		if e != nil || r.LinkIndex != end0.Attrs().Index {
			t.Fatalf("LAN traffic not sent via end0 with kill switch off: %+v, %v", r, e)
		}
	})
}

// TestGuardRestoresRouting simulates systemd-networkd configuring a new link
// (such as a USB modem being plugged in) and removing every rule and route it
// did not create. The guard must put them back.
func TestGuardRestoresRouting(t *testing.T) {
	inNewNetns(t, func() {
		lo, _ := netlink.LinkByName("lo")
		_ = netlink.LinkSetUp(lo)
		addLink(t, "br0", "172.16.0.1/12")
		addLink(t, "wg0", "10.8.0.2/32")

		cfg := config.Default()
		store := state.NewStore()
		g := newGuard(cfg, 80, store)
		// Keep iptables out of the test.
		g.applyFirewall = func() error { return nil }
		g.checkFirewall = func() ([]string, error) { return nil, nil }
		if e := g.Apply(); e != nil {
			t.Skipf("cannot set up routing in netns: %v", e)
		}
		table := cfg.WireGuard.Table

		// What networkd does to foreign configuration.
		rules, _ := netlink.RuleList(netlink.FAMILY_V4)
		for _, r := range rules {
			if r.Table == table {
				if e := netlink.RuleDel(&r); e != nil {
					t.Fatal(e)
				}
			}
		}
		routes, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		for _, r := range routes {
			if e := netlink.RouteDel(&r); e != nil {
				t.Fatal(e)
			}
		}
		st, _ := getVPNRoutingStatus("br0", "wg0", table)
		if st.rule || st.vpnRoute || st.killSwitch {
			t.Fatalf("setup not removed: %+v", st)
		}

		g.check(false)

		st, _ = getVPNRoutingStatus("br0", "wg0", table)
		if !st.rule || !st.vpnRoute || !st.killSwitch {
			t.Fatalf("not restored: %+v", st)
		}
		r := store.Snapshot().Routing
		if r.Restores != 1 || !r.Rule || !r.VPNRoute || !r.KillSwitch || r.Error != "" {
			t.Fatalf("published routing = %+v", r)
		}
		if !strings.Contains(r.LastRestoreReason, "policy rule") {
			t.Fatalf("reason = %q", r.LastRestoreReason)
		}

		// Nothing missing: no further restores.
		g.check(false)
		if n := store.Snapshot().Routing.Restores; n != 1 {
			t.Fatalf("restores = %d after a clean check", n)
		}
	})
}

// Re-adding the VPN route after the tunnel restarts is routine and must not
// be reported as a restore.
func TestGuardReaddsVPNRouteQuietly(t *testing.T) {
	inNewNetns(t, func() {
		addLink(t, "br0", "172.16.0.1/12")
		addLink(t, "wg0", "10.8.0.2/32")
		store := state.NewStore()
		g := newGuard(config.Default(), 80, store)
		g.applyFirewall = func() error { return nil }
		g.checkFirewall = func() ([]string, error) { return nil, nil }
		if e := g.Apply(); e != nil {
			t.Skipf("cannot set up routing in netns: %v", e)
		}
		wg0, _ := netlink.LinkByName("wg0")
		_ = netlink.LinkSetDown(wg0) // the kernel removes the route
		_ = netlink.LinkSetUp(wg0)
		g.check(false)
		r := store.Snapshot().Routing
		if !r.VPNRoute || r.Restores != 0 {
			t.Fatalf("routing = %+v", r)
		}
	})
}
