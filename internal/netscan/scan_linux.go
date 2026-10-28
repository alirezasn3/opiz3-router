//go:build linux

package netscan

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}

func scanNetwork(parent context.Context, cfg config.Config, store *state.Store) {
	iface := cfg.Interfaces.Ethernet
	store.Update(func(s *state.Snapshot) {
		s.NetworkScan.Interface = iface
		s.NetworkScan.Active = true
		s.NetworkScan.Error = ""
	})
	link, e := netlink.LinkByName(iface)
	if e != nil {
		publishNetworkScan(store, iface, "", nil, e)
		return
	}
	addrs, e := netlink.AddrList(link, netlink.FAMILY_V4)
	if e != nil || len(addrs) == 0 {
		if e == nil {
			e = fmt.Errorf("interface has no IPv4 address")
		}
		publishNetworkScan(store, iface, "", nil, e)
		return
	}
	_, subnet, e := net.ParseCIDR(addrs[0].IPNet.String())
	if e != nil {
		publishNetworkScan(store, iface, "", nil, e)
		return
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones < 22 {
		publishNetworkScan(store, iface, subnet.String(), nil, fmt.Errorf("subnet is too large to scan"))
		return
	}
	hosts := subnetHosts(subnet)
	devices, e := arpScan(parent, link, addrs[0].IP, hosts)
	if e != nil {
		publishNetworkScan(store, iface, subnet.String(), nil, e)
		return
	}
	resolveNetworkNames(parent, devices, link.Attrs().Index, addrs[0].IP, cfg.DHCPLeasesFile)
	slices.SortFunc(devices, func(a, b state.NetworkDevice) int { return strings.Compare(a.IP, b.IP) })
	publishNetworkScan(store, iface, subnet.String(), devices, nil)
}

func arpScan(parent context.Context, link netlink.Link, source net.IP, hosts []net.IP) ([]state.NetworkDevice, error) {
	hw := link.Attrs().HardwareAddr
	if len(hw) != 6 {
		return nil, fmt.Errorf("interface has no Ethernet address")
	}
	fd, e := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ARP)))
	if e != nil {
		return nil, fmt.Errorf("ARP socket: %w", e)
	}
	defer unix.Close(fd)
	if e = unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  link.Attrs().Index,
	}); e != nil {
		return nil, fmt.Errorf("bind ARP socket: %w", e)
	}
	timeout := arpScanTimeout
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = max(time.Until(deadline), time.Millisecond)
	}
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	if e = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); e != nil {
		return nil, fmt.Errorf("set ARP timeout: %w", e)
	}

	for _, target := range hosts {
		if target.Equal(source) {
			continue
		}
		frame, e := arpRequest(hw, source, target)
		if e != nil {
			return nil, e
		}
		if e = unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{
			Protocol: htons(unix.ETH_P_ARP),
			Ifindex:  link.Attrs().Index,
			Halen:    6,
			Addr:     [8]uint8{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		}); e != nil {
			return nil, fmt.Errorf("send ARP request: %w", e)
		}
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	buf := make([]byte, 1500)
	found := map[string]state.NetworkDevice{}
	wanted := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		wanted[host.String()] = true
	}
	for {
		select {
		case <-ctx.Done():
			devices := make([]state.NetworkDevice, 0, len(found))
			for _, d := range found {
				devices = append(devices, d)
			}
			return devices, nil
		default:
		}

		n, _, e := unix.Recvfrom(fd, buf, 0)
		if e != nil {
			if e == unix.EAGAIN || e == unix.EWOULDBLOCK || e == unix.EINTR {
				continue
			}
			return nil, fmt.Errorf("receive ARP reply: %w", e)
		}
		packet := gopacket.NewPacket(buf[:n], layers.LayerTypeEthernet, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
		arp, ok := packet.Layer(layers.LayerTypeARP).(*layers.ARP)
		if !ok || arp.Operation != layers.ARPReply || len(arp.SourceHwAddress) != 6 || len(arp.SourceProtAddress) != 4 {
			continue
		}
		ip := net.IP(arp.SourceProtAddress).To4()
		if !wanted[ip.String()] {
			continue
		}
		found[ip.String()] = state.NetworkDevice{
			IP: ip.String(), MAC: net.HardwareAddr(arp.SourceHwAddress).String(), Reachable: true,
		}
	}
}

func arpRequest(hw net.HardwareAddr, source, target net.IP) ([]byte, error) {
	eth := &layers.Ethernet{
		SrcMAC: hw, DstMAC: net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}
	arp := &layers.ARP{
		AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4,
		HwAddressSize: 6, ProtAddressSize: 4, Operation: layers.ARPRequest,
		SourceHwAddress: hw, SourceProtAddress: source.To4(),
		DstHwAddress: net.HardwareAddr{0, 0, 0, 0, 0, 0}, DstProtAddress: target.To4(),
	}
	buf := gopacket.NewSerializeBuffer()
	if e := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{}, eth, arp); e != nil {
		return nil, fmt.Errorf("serialize ARP request: %w", e)
	}
	return buf.Bytes(), nil
}
