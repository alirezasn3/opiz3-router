//go:build linux

package collector

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/sysinfo"
)

func newWGClient() (*wgctrl.Client, error) {
	return wgctrl.New()
}

func (c *linkCollector) collect(store *state.Store) {
	bridgeIndex := 0
	if br, e := netlink.LinkByName(c.cfg.Interfaces.Bridge); e == nil {
		bridgeIndex = br.Attrs().Index
	}

	var out []state.Interface
	for _, item := range []struct{ name, role string }{
		{c.cfg.Interfaces.Ethernet, "wan-ethernet"},
		{c.cfg.Interfaces.USBModem, "wan-4g"},
		{c.cfg.Interfaces.Wireless, "wan-wifi"},
		{c.cfg.WireGuard.Interface, "vpn"},
		{c.cfg.Interfaces.Bridge, "lan"},
	} {
		if item.name == "" {
			continue
		}
		iface := state.Interface{Name: item.name, Role: item.role, Addresses: []string{}}
		link, e := netlink.LinkByName(item.name)
		if e != nil {
			delete(c.prev, item.name)
			out = append(out, iface)
			continue
		}
		a := link.Attrs()
		if bridgeIndex != 0 && a.MasterIndex == bridgeIndex {
			iface.Role = "lan-port"
			if item.role == "wan-wifi" {
				iface.Role = "wifi"
			}
		}
		iface.Present = true
		iface.OperState = a.OperState.String()
		iface.Up = a.Flags&net.FlagUp != 0 && (a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown)
		if len(a.HardwareAddr) > 0 {
			iface.MAC = a.HardwareAddr.String()
		}
		if addrs, e := netlink.AddrList(link, netlink.FAMILY_ALL); e == nil {
			for _, addr := range addrs {
				if addr.IP.IsLinkLocalUnicast() {
					continue
				}
				iface.Addresses = append(iface.Addresses, addr.IPNet.String())
			}
		}
		if st := a.Statistics; st != nil {
			cur := counterSample{at: time.Now(), rx: st.RxBytes, tx: st.TxBytes}
			iface.RxBytes, iface.TxBytes = cur.rx, cur.tx
			iface.RxRate, iface.TxRate = rates(c.prev[item.name], cur)
			c.prev[item.name] = cur
		}
		out = append(out, iface)
	}
	store.Update(func(s *state.Snapshot) { s.Interfaces = out })
}

func (c *wireguardCollector) collect(store *state.Store) {
	wg := state.WireGuard{Interface: c.name, Peers: []state.Peer{}}
	dev, e := c.client.Device(c.name)
	if e != nil {
		wg.Error = e.Error()
		clear(c.prev)
		store.Update(func(s *state.Snapshot) { s.WireGuard = wg })
		return
	}
	if link, e := netlink.LinkByName(c.name); e == nil {
		wg.Up = link.Attrs().Flags&net.FlagUp != 0
	}
	wg.PublicKey = dev.PublicKey.String()
	wg.ListenPort = dev.ListenPort

	now := time.Now()
	sample := state.TrafficSample{Time: now}
	seen := map[string]bool{}
	for _, p := range dev.Peers {
		key := p.PublicKey.String()
		seen[key] = true
		peer := state.Peer{
			PublicKey:     key,
			LastHandshake: p.LastHandshakeTime,
			RxBytes:       p.ReceiveBytes,
			TxBytes:       p.TransmitBytes,
			AllowedIPs:    []string{},
		}
		if p.Endpoint != nil {
			peer.Endpoint = p.Endpoint.String()
		}
		for _, ip := range p.AllowedIPs {
			peer.AllowedIPs = append(peer.AllowedIPs, ip.String())
		}
		cur := counterSample{at: now, rx: uint64(p.ReceiveBytes), tx: uint64(p.TransmitBytes)}
		peer.RxRate, peer.TxRate = rates(c.prev[key], cur)
		c.prev[key] = cur
		sample.RxRate += peer.RxRate
		sample.TxRate += peer.TxRate
		wg.Peers = append(wg.Peers, peer)
	}
	for key := range c.prev {
		if !seen[key] {
			delete(c.prev, key)
		}
	}
	store.Update(func(s *state.Snapshot) {
		s.WireGuard = wg
		s.AddTraffic(sample)
	})
}

var iwWarning sync.Once

func stationDump(iface string) ([]sysinfo.Station, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "iw", "dev", iface, "station", "dump").Output()
	if e != nil {
		return nil, e
	}
	return sysinfo.ParseStationDump(bytes.NewReader(out))
}

func collectClients(cfg config.Config, store *state.Store) {
	byMAC := map[string]*state.Client{}
	get := func(mac string) *state.Client {
		c, ok := byMAC[mac]
		if !ok {
			c = &state.Client{MAC: mac}
			byMAC[mac] = c
		}
		return c
	}

	if f, e := os.Open(cfg.DHCPLeasesFile); e == nil {
		leases, e := sysinfo.ParseLeases(f)
		f.Close()
		if e != nil {
			log.Printf("failed to parse %s: %v", cfg.DHCPLeasesFile, e)
		}
		for _, l := range leases {
			c := get(l.MAC)
			c.IP, c.Hostname, c.LeaseExpiry = l.IP, l.Hostname, l.Expiry
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		log.Printf("failed to read DHCP leases: %v", e)
	}

	wirelessInfo := false
	if cfg.Interfaces.Wireless != "" {
		stations, e := stationDump(cfg.Interfaces.Wireless)
		if e != nil {
			iwWarning.Do(func() { log.Printf("wireless clients unavailable (iw station dump): %v", e) })
		}
		wirelessInfo = e == nil
		for _, st := range stations {
			c := get(st.MAC)
			c.Wireless, c.Online = true, true
			c.Signal, c.ConnectedSeconds = st.Signal, st.ConnectedSeconds
			c.RxBytes, c.TxBytes = st.RxBytes, st.TxBytes
		}
	}

	if br, e := netlink.LinkByName(cfg.Interfaces.Bridge); e == nil {
		neighs, _ := netlink.NeighList(br.Attrs().Index, netlink.FAMILY_V4)
		for _, n := range neighs {
			if len(n.HardwareAddr) == 0 || n.State&netlink.NUD_PERMANENT != 0 {
				continue
			}
			active := n.State&(netlink.NUD_REACHABLE|netlink.NUD_DELAY|netlink.NUD_PROBE) != 0
			mac := strings.ToLower(n.HardwareAddr.String())
			if _, known := byMAC[mac]; !known && !active {
				continue
			}
			c := get(mac)
			if c.IP == "" {
				c.IP = n.IP.String()
			}
			c.Online = c.Online || active
		}
	}

	clients := make([]state.Client, 0, len(byMAC))
	for _, c := range byMAC {
		clients = append(clients, *c)
	}
	slices.SortFunc(clients, func(a, b state.Client) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(strings.ToLower(a.Hostname), strings.ToLower(b.Hostname)), cmp.Compare(a.IP, b.IP), cmp.Compare(a.MAC, b.MAC))
	})
	store.Update(func(s *state.Snapshot) {
		s.Clients = clients
		s.WirelessInfo = wirelessInfo
	})
}
