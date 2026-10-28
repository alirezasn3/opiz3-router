package mnm

import (
	"strconv"

	"opiz3-router/internal/config"
)

const (
	keyBridge      = "bridge"
	keyBridgeAddr  = "bridge-address"
	keyVPNCheck    = "vpn-health"
	keyVPNRoute    = "vpn-route"
	keyKillSwitch  = "vpn-kill-switch"
	keyVPNRule     = "vpn-rule"
	killSwitchMetr = 4096
)

// Desired is everything the router wants mini-net-man to hold.
type Desired struct {
	Bridges  []Bridge
	Addrs    []Address
	Checks   []Check
	Routes   []Route
	Rules    []Rule
	Firewall []FirewallRule
	// RuleChecks maps a rule's key to the key of the health check that gates it.
	RuleChecks map[string]string
}

func tagged(key string) Meta { return Meta{Comment: Tag(key)} }

// Build describes the router's setup for cfg. lan says whether the Ethernet
// port is a LAN port (a bridge port) instead of an uplink.
func Build(cfg config.Config, lan bool) Desired {
	ifc, wg := cfg.Interfaces, cfg.WireGuard
	table := strconv.Itoa(wg.Table)
	d := Desired{RuleChecks: map[string]string{}}

	// The Wi-Fi interface is not listed: hostapd joins it to the bridge.
	ports := []string{}
	if lan {
		ports = append(ports, ifc.Ethernet)
	}
	d.Bridges = []Bridge{{Meta: tagged(keyBridge), Name: ifc.Bridge, Ports: ports}}
	d.Addrs = []Address{{Meta: tagged(keyBridgeAddr), Address: cfg.Network.BridgeAddress, Interface: ifc.Bridge}}

	d.Checks = []Check{{Meta: tagged(keyVPNCheck), Name: "opiz3-router " + wg.Interface, Target: wg.HealthTarget, Interface: wg.Interface}}
	d.Routes = []Route{{Meta: tagged(keyVPNRoute), Dst: "default", Dev: wg.Interface, Table: table}}
	rule := Rule{Meta: tagged(keyVPNRule), Priority: wg.RulePriority, Iif: ifc.Bridge, Family: "ipv4", Table: table}
	if wg.KillSwitch {
		// The blackhole route has a worse metric than the tunnel route, so it
		// only carries traffic while the tunnel is missing.
		d.Routes = append(d.Routes, Route{Meta: tagged(keyKillSwitch), Dst: "default", Type: "blackhole", Table: table, Metric: killSwitchMetr})
	} else {
		// Without a kill switch the rule steps aside while the tunnel fails
		// its health check, and LAN traffic uses the uplink directly.
		d.RuleChecks[keyVPNRule] = keyVPNCheck
	}
	d.Rules = []Rule{rule}

	d.Firewall = firewallRules(cfg)
	return d
}

func firewallRules(cfg config.Config) []FirewallRule {
	ifc := cfg.Interfaces
	var out []FirewallRule
	add := func(key string, r FirewallRule) {
		r.Meta = tagged(key)
		out = append(out, r)
	}

	// Keep the panels off the uplinks.
	ports := []int{}
	if cfg.Panel.Enabled {
		if p, e := cfg.PanelPort(); e == nil {
			ports = append(ports, p)
		}
	}
	if p := cfg.MiniNetManPort(); p > 0 && (len(ports) == 0 || p != ports[0]) {
		ports = append(ports, p)
	}
	for _, p := range ports {
		port := strconv.Itoa(p)
		for _, iface := range cfg.Panel.AllowedInterfaces {
			add("panel-"+port+"-"+iface, FirewallRule{Chain: "input", Action: "accept", InIface: iface, Proto: "tcp", DstPort: port})
		}
		add("panel-"+port+"-lo", FirewallRule{Chain: "input", Action: "accept", InIface: "lo", Proto: "tcp", DstPort: port})
		add("panel-"+port+"-drop", FirewallRule{Chain: "input", Action: "drop", Proto: "tcp", DstPort: port})
	}

	// Nothing from outside may open a connection into the LAN.
	add("forward-established", FirewallRule{Chain: "forward", Action: "accept", ConnState: "established,related"})
	add("forward-from-lan", FirewallRule{Chain: "forward", Action: "accept", InIface: ifc.Bridge})
	add("forward-to-lan", FirewallRule{Chain: "forward", Action: "drop", OutIface: ifc.Bridge})

	for _, iface := range []string{ifc.Ethernet, ifc.USBModem, cfg.WireGuard.Interface} {
		if iface != "" {
			add("masquerade-"+iface, FirewallRule{Chain: "srcnat", Action: "masquerade", OutIface: iface, Family: "ipv4"})
		}
	}
	add("clamp-mss", FirewallRule{Chain: "mangle", Action: "clamp-mss", Proto: "tcp", Family: "ipv4"})
	return out
}
