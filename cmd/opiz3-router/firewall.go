//go:build linux

package main

import (
	"strconv"

	"github.com/coreos/go-iptables/iptables"
)

// The router's filter rules live in their own chains, which are rebuilt on
// every start and jumped to from the top of INPUT and FORWARD. Other rules in
// the built-in chains are left alone.
const (
	inputChain   = "OPIZ3-INPUT"
	forwardChain = "OPIZ3-FORWARD"
)

// setupFirewall restricts the panel port to allowedInterfaces and drops new
// connections forwarded into the LAN from anywhere but the LAN itself, so the
// VPN peer and WAN side cannot reach clients unless the connection was started
// from the LAN or explicitly port forwarded (DNAT).
func setupFirewall(bridge string, panelPort int, allowedInterfaces []string) error {
	ipt, e := iptables.New()
	if e != nil {
		return e
	}

	var input [][]string
	if panelPort > 0 {
		port := strconv.Itoa(panelPort)
		for _, iface := range allowedInterfaces {
			input = append(input, []string{"-i", iface, "-p", "tcp", "--dport", port, "-j", "ACCEPT"})
		}
		// Loopback stays allowed for local health checks.
		input = append(input,
			[]string{"-i", "lo", "-p", "tcp", "--dport", port, "-j", "ACCEPT"},
			[]string{"-p", "tcp", "--dport", port, "-j", "DROP"},
		)
	}
	forward := [][]string{
		{"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED,DNAT", "-j", "RETURN"},
		{"!", "-i", bridge, "-o", bridge, "-j", "DROP"},
	}

	for chain, rules := range map[string][][]string{inputChain: input, forwardChain: forward} {
		// ClearChain creates the chain, or flushes it if it already exists.
		if e = ipt.ClearChain("filter", chain); e != nil {
			return e
		}
		for _, rule := range rules {
			if e = ipt.Append("filter", chain, rule...); e != nil {
				return e
			}
		}
	}
	if e = ipt.InsertUnique("filter", "INPUT", 1, "-j", inputChain); e != nil {
		return e
	}
	return ipt.InsertUnique("filter", "FORWARD", 1, "-j", forwardChain)
}

// firewallActive reports whether both chains are hooked into the filter table.
func firewallActive() (bool, error) {
	ipt, e := iptables.New()
	if e != nil {
		return false, e
	}
	for builtin, chain := range map[string]string{"INPUT": inputChain, "FORWARD": forwardChain} {
		ok, e := ipt.Exists("filter", builtin, "-j", chain)
		if e != nil || !ok {
			return false, e
		}
	}
	return true, nil
}

// masqueradeActive reports whether traffic leaving iface is masqueraded.
func masqueradeActive(iface string) (bool, error) {
	ipt, e := iptables.New()
	if e != nil {
		return false, e
	}
	return ipt.Exists("nat", "POSTROUTING", "-o", iface, "-j", "MASQUERADE")
}
