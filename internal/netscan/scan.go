package netscan

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

const arpScanTimeout = 3 * time.Second

// Scanner discovers hosts on the IPv4 subnet assigned to the Ethernet uplink when triggered.
type Scanner struct {
	cfg   config.Config
	store *state.Store
	mu    sync.Mutex
	busy  bool
}

// NewScanner creates a new network scanner.
func NewScanner(cfg config.Config, store *state.Store) *Scanner {
	return &Scanner{cfg: cfg, store: store}
}

// TriggerScan starts an asynchronous network discovery scan if one is not already running.
func (s *Scanner) TriggerScan() error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errors.New("scan already in progress")
	}
	s.busy = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.busy = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		scanNetwork(ctx, s.cfg, s.store)
	}()
	return nil
}

func subnetHosts(n *net.IPNet) []net.IP {
	ip := n.IP.To4()
	if ip == nil {
		return nil
	}
	var hosts []net.IP
	for candidate := append(net.IP(nil), ip...); n.Contains(candidate); candidate = nextIPv4(candidate) {
		if candidate.Equal(ip) || candidate.Equal(lastIPv4(n)) {
			continue
		}
		hosts = append(hosts, append(net.IP(nil), candidate...))
	}
	return hosts
}

func nextIPv4(ip net.IP) net.IP {
	out := append(net.IP(nil), ip.To4()...)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

func lastIPv4(n *net.IPNet) net.IP {
	out := append(net.IP(nil), n.IP.To4()...)
	for i := range out {
		out[i] |= ^n.Mask[i+len(n.Mask)-4]
	}
	return out
}

func publishNetworkScan(store *state.Store, iface, subnet string, devices []state.NetworkDevice, e error) {
	store.Update(func(s *state.Snapshot) {
		s.NetworkScan.Interface, s.NetworkScan.Subnet = iface, subnet
		s.NetworkScan.Active, s.NetworkScan.Error = false, ""
		if e != nil {
			s.NetworkScan.Error = e.Error()
			return
		}
		s.NetworkScan.Devices, s.NetworkScan.UpdatedAt = devices, time.Now()
	})
}
