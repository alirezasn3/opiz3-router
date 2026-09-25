//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

const (
	// routingCheckInterval is how often the routing rule and routes are
	// checked. It is a cheap netlink dump, so it runs often to keep any
	// outage short.
	routingCheckInterval = 3 * time.Second
	// firewallCheckInterval is how often the iptables rules are checked.
	// Each check runs several iptables processes, so it runs less often.
	firewallCheckInterval = 15 * time.Second
	// linkSettleDelay is how long to wait after a link change before a full
	// check, giving systemd-networkd time to configure the link (and remove
	// rules it does not own, unless told not to).
	linkSettleDelay = 2 * time.Second
)

// guard keeps the router's routing and firewall setup in place. Other
// programs can remove it: systemd-networkd, for example, deletes routing
// rules and routes it did not create whenever it configures a link, such as
// when the USB modem is plugged in. The guard puts back anything missing and
// publishes the current state.
type guard struct {
	cfg       config.Config
	panelPort int
	store     *state.Store

	// Firewall hooks, replaceable in tests that must not touch iptables.
	applyFirewall func() error
	checkFirewall func() ([]string, error)

	// firewallOK is the result of the last firewall check; checking runs
	// iptables, so it is not repeated for every published update.
	firewallOK  bool
	restores    int
	lastRestore time.Time
	lastReason  string
}

func newGuard(cfg config.Config, panelPort int, store *state.Store) *guard {
	g := &guard{cfg: cfg, panelPort: panelPort, store: store}
	g.applyFirewall = func() error {
		for _, iface := range g.masqueradeInterfaces() {
			if e := masqueradeInterface(iface); e != nil {
				return fmt.Errorf("masquerade %s: %w", iface, e)
			}
		}
		return setupFirewall(cfg.Interfaces.Bridge, panelPort, cfg.Panel.AllowedInterfaces)
	}
	g.checkFirewall = func() ([]string, error) {
		var missing []string
		ok, e := firewallActive()
		if e != nil {
			return nil, e
		}
		if !ok {
			missing = append(missing, "firewall")
		}
		for _, iface := range g.masqueradeInterfaces() {
			ok, e := masqueradeActive(iface)
			if e != nil {
				return nil, e
			}
			if !ok {
				missing = append(missing, "masquerade "+iface)
			}
		}
		return missing, nil
	}
	return g
}

// masqueradeInterfaces lists every interface traffic can leave through. Rules
// for interfaces that do not exist yet, such as an unplugged modem, are
// harmless and take effect when the interface appears.
func (g *guard) masqueradeInterfaces() []string {
	var out []string
	for _, name := range []string{
		g.cfg.Interfaces.Ethernet,
		g.cfg.Interfaces.USBModem,
		g.cfg.Interfaces.Wireless,
		g.cfg.WireGuard.Interface,
		g.cfg.Interfaces.Bridge,
	} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// applyRouting sets up forwarding and the VPN routing. It is idempotent.
func (g *guard) applyRouting() error {
	wg := g.cfg.WireGuard
	if e := enableIPForwarding(); e != nil {
		return fmt.Errorf("ip forwarding: %w", e)
	}
	if e := setupVPNRouting(g.cfg.Interfaces.Bridge, wg.Table, wg.RulePriority, wg.KillSwitch); e != nil {
		return e
	}
	st, e := getVPNRoutingStatus(g.cfg.Interfaces.Bridge, wg.Interface, wg.Table)
	if e != nil {
		return e
	}
	// A route through a missing or down interface cannot be added; it is
	// added once the tunnel comes up.
	if st.vpnUp {
		if e := ensureVPNRoute(wg.Interface, wg.Table); e != nil {
			return fmt.Errorf("vpn route: %w", e)
		}
	}
	return nil
}

// Apply sets up everything from scratch.
func (g *guard) Apply() error {
	if e := g.applyRouting(); e != nil {
		return e
	}
	if e := g.applyFirewall(); e != nil {
		return fmt.Errorf("firewall: %w", e)
	}
	g.firewallOK = true
	g.publish(nil)
	return nil
}

// checkRouting returns what is missing from the routing setup.
func (g *guard) checkRouting() ([]string, error) {
	wg := g.cfg.WireGuard
	var missing []string
	forward, e := ipForwardingEnabled()
	if e != nil {
		return nil, e
	}
	if !forward {
		missing = append(missing, "ip forwarding")
	}
	st, e := getVPNRoutingStatus(g.cfg.Interfaces.Bridge, wg.Interface, wg.Table)
	if e != nil {
		return nil, e
	}
	if !st.rule {
		missing = append(missing, "policy rule")
	}
	if wg.KillSwitch && !st.killSwitch {
		missing = append(missing, "kill switch route")
	}
	if st.vpnUp && !st.vpnRoute {
		missing = append(missing, "vpn route")
	}
	return missing, nil
}

// check looks for missing pieces, restores them and publishes the state.
func (g *guard) check(firewall bool) {
	var errs []error
	missing, e := g.checkRouting()
	if e != nil {
		errs = append(errs, e)
	}
	// The kernel drops the VPN route whenever the tunnel goes down, so adding
	// it back once the tunnel is up again is routine, not a restore.
	if len(missing) == 1 && missing[0] == "vpn route" {
		log.Print("guard: tunnel is up; adding VPN route")
		if e := ensureVPNRoute(g.cfg.WireGuard.Interface, g.cfg.WireGuard.Table); e != nil {
			errs = append(errs, fmt.Errorf("vpn route: %w", e))
		}
		missing = nil
	}
	routingMissing := len(missing) > 0
	firewallMissing := false
	if firewall {
		fw, e := g.checkFirewall()
		if e != nil {
			errs = append(errs, e)
		}
		firewallMissing = len(fw) > 0
		g.firewallOK = e == nil && !firewallMissing
		missing = append(missing, fw...)
	}

	if len(missing) > 0 {
		reason := "missing " + strings.Join(missing, ", ")
		log.Printf("guard: %s; restoring", reason)
		if routingMissing {
			if e := g.applyRouting(); e != nil {
				errs = append(errs, fmt.Errorf("restore routing: %w", e))
			}
		}
		if firewallMissing {
			if e := g.applyFirewall(); e != nil {
				errs = append(errs, fmt.Errorf("restore firewall: %w", e))
			} else {
				g.firewallOK = true
			}
		}
		g.restores++
		g.lastRestore = time.Now()
		g.lastReason = reason
		g.store.Notify(state.Warning, "guard", "Restored "+strings.Join(missing, ", "))
	}
	g.publish(errors.Join(errs...))
}

func (g *guard) publish(checkErr error) {
	r := state.Routing{
		KillSwitchEnabled: g.cfg.WireGuard.KillSwitch,
		Restores:          g.restores,
		LastRestore:       g.lastRestore,
		LastRestoreReason: g.lastReason,
	}
	errs := []error{checkErr}
	var e error
	if r.IPForward, e = ipForwardingEnabled(); e != nil {
		errs = append(errs, e)
	}
	st, e := getVPNRoutingStatus(g.cfg.Interfaces.Bridge, g.cfg.WireGuard.Interface, g.cfg.WireGuard.Table)
	if e != nil {
		errs = append(errs, e)
	}
	r.Rule, r.VPNRoute, r.KillSwitch = st.rule, st.vpnRoute, st.killSwitch
	r.Firewall = g.firewallOK
	if e = errors.Join(errs...); e != nil {
		r.Error = e.Error()
	}
	g.store.Update(func(s *state.Snapshot) { s.Routing = r })
}

// Run checks the setup periodically, and shortly after every link change,
// until ctx is cancelled. linkChanged receives a value on each link event.
func (g *guard) Run(ctx context.Context, linkChanged <-chan struct{}) {
	routing := time.NewTicker(routingCheckInterval)
	defer routing.Stop()
	firewall := time.NewTicker(firewallCheckInterval)
	defer firewall.Stop()
	settle := time.NewTimer(0)
	<-settle.C
	for {
		select {
		case <-ctx.Done():
			return
		case <-linkChanged:
			// Wait for the burst of events around a change to end.
			settle.Reset(linkSettleDelay)
		case <-settle.C:
			g.check(true)
		case <-firewall.C:
			g.check(true)
		case <-routing.C:
			g.check(false)
		}
	}
}
