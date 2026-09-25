//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/assets"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/sysinfo"
)

const networkScanInterval = 30 * time.Second
const arpScanTimeout = 3 * time.Second

// startNetworkScan periodically discovers hosts on the IPv4 subnet assigned to
// the Ethernet uplink.
func startNetworkScan(ctx context.Context, cfg config.Config, store *state.Store) {
	go every(ctx, networkScanInterval, func() {
		scanNetwork(ctx, cfg, store)
	})
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
	resolveNetworkNames(parent, devices, link, addrs[0].IP, cfg.DHCPLeasesFile)
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

// resolveNetworkNames tries the names commonly available on a LAN, in
// precedence order: dnsmasq's DHCP lease, ordinary reverse DNS, mDNS,
// LLMNR, and NetBIOS name service.
func resolveNetworkNames(ctx context.Context, devices []state.NetworkDevice, link netlink.Link, source net.IP, leasePath string) {
	oui := loadOUIDatabase()
	byIP := map[string]string{}
	if f, e := os.Open(leasePath); e == nil {
		leases, _ := sysinfo.ParseLeases(bufio.NewReader(f))
		_ = f.Close()
		for _, l := range leases {
			if l.Hostname != "" {
				byIP[l.IP] = l.Hostname
			}
		}
	}
	var wg sync.WaitGroup
	for i := range devices {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := devices[i].IP
			devices[i].Manufacturer = manufacturer(oui, devices[i].MAC)
			if name := byIP[ip]; name != "" {
				devices[i].Name = name
				return
			}
			if names, e := net.DefaultResolver.LookupAddr(ctx, ip); e == nil && len(names) != 0 {
				devices[i].Name = strings.TrimSuffix(names[0], ".")
				return
			}
			if name := lookupMulticastPTR(ctx, link, net.ParseIP(ip), "224.0.0.251", 5353); name != "" {
				devices[i].Name = name
				return
			}
			if name := lookupMulticastPTR(ctx, link, net.ParseIP(ip), "224.0.0.252", 5355); name != "" {
				devices[i].Name = name
				return
			}
			devices[i].Name = lookupNetBIOS(ctx, source, net.ParseIP(ip))
		}(i)
	}
	wg.Wait()
}

// loadOUIDatabase reads the Wireshark manuf database embedded in the binary.
func loadOUIDatabase() map[string]string {
	ouis := make(map[string]string)
	f, e := assets.Files.Open(assets.ManufPath)
	if e != nil {
		return ouis
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		prefix := strings.SplitN(fields[0], "/", 2)[0]
		prefix = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(prefix))
		if len(prefix) >= 6 {
			name := fields[1]
			if len(fields) >= 3 {
				name = strings.Join(fields[2:], " ")
			}
			ouis[prefix] = name
		}
	}
	return ouis
}

func macOUI(mac string) string {
	mac = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(mac)))
	if len(mac) < 6 {
		return ""
	}
	return mac
}

func manufacturer(ouis map[string]string, mac string) string {
	hex := macOUI(mac)
	for n := min(len(hex), 12); n >= 6; n-- {
		if name := ouis[hex[:n]]; name != "" {
			return name
		}
	}
	return ""
}

func lookupMulticastPTR(ctx context.Context, link netlink.Link, ip net.IP, group string, port int) string {
	iface, e := net.InterfaceByIndex(link.Attrs().Index)
	if e != nil {
		return ""
	}
	groupIP := net.ParseIP(group)
	conn, e := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{IP: groupIP, Port: port})
	if e != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, e = conn.WriteToUDP(dnsPTRQuery(ip), &net.UDPAddr{IP: groupIP, Port: port}); e != nil {
		return ""
	}
	buf := make([]byte, 1500)
	for {
		n, _, e := conn.ReadFromUDP(buf)
		if e != nil {
			return ""
		}
		if name := dnsPTRAnswer(buf[:n]); name != "" {
			return name
		}
		select {
		case <-ctx.Done():
			return ""
		default:
		}
	}
}

func dnsPTRQuery(ip net.IP) []byte {
	ptr := reverseName(ip)
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[0:2], uint16(time.Now().UnixNano()))
	binary.BigEndian.PutUint16(q[4:6], 1)
	return append(append(q, dnsName(ptr)...), 0, 12, 0, 1)
}

func reverseName(ip net.IP) string {
	v := ip.To4()
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", v[3], v[2], v[1], v[0])
}

func dnsName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return out
}

func dnsPTRAnswer(msg []byte) string {
	if len(msg) < 12 || binary.BigEndian.Uint16(msg[6:8]) == 0 {
		return ""
	}
	p := 12
	var e bool
	p, e = skipDNSName(msg, p)
	if e || p+4 > len(msg) {
		return ""
	}
	p += 4
	for i := 0; i < int(binary.BigEndian.Uint16(msg[6:8])); i++ {
		var name string
		name, p, e = readDNSName(msg, p)
		if e || p+10 > len(msg) {
			return ""
		}
		typ, class, rdlen := binary.BigEndian.Uint16(msg[p:p+2]), binary.BigEndian.Uint16(msg[p+2:p+4]), int(binary.BigEndian.Uint16(msg[p+8:p+10]))
		p += 10
		if p+rdlen > len(msg) {
			return ""
		}
		if typ == 12 && class&0x7fff == 1 {
			value, _, ok := readDNSName(msg, p)
			if ok && value != "" {
				return strings.TrimSuffix(value, ".")
			}
		}
		_ = name
		p += rdlen
	}
	return ""
}

func skipDNSName(msg []byte, p int) (int, bool) {
	_, p, ok := readDNSName(msg, p)
	return p, !ok
}

func readDNSName(msg []byte, p int) (string, int, bool) {
	var labels []string
	for jumps := 0; jumps < 16; jumps++ {
		if p >= len(msg) {
			return "", p, false
		}
		n := int(msg[p])
		if n == 0 {
			return strings.Join(labels, ".") + ".", p + 1, true
		}
		if n&0xc0 == 0xc0 {
			if p+1 >= len(msg) {
				return "", p, false
			}
			target := int(binary.BigEndian.Uint16(msg[p:p+2]) & 0x3fff)
			value, _, ok := readDNSName(msg, target)
			return strings.Join(append(labels, strings.TrimSuffix(value, ".")), "."), p + 2, ok
		}
		p++
		if p+n > len(msg) {
			return "", p, false
		}
		labels = append(labels, string(msg[p:p+n]))
		p += n
	}
	return "", p, false
}

func lookupNetBIOS(ctx context.Context, source, ip net.IP) string {
	conn, e := net.DialUDP("udp4", &net.UDPAddr{IP: source}, &net.UDPAddr{IP: ip, Port: 137})
	if e != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	query := []byte{0, 0x01, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x20, 'C', 'K', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 0, 0, 0x21, 0, 1}
	if _, e = conn.Write(query); e != nil {
		return ""
	}
	buf := make([]byte, 1500)
	n, e := conn.Read(buf)
	if e != nil || n < 57 {
		return ""
	}
	// NBSTAT names are 18-byte entries: 15-byte padded name, suffix, flags.
	count := int(buf[56])
	for p := 57; p+18 <= n && count > 0; p, count = p+18, count-1 {
		name := strings.TrimSpace(string(buf[p : p+15]))
		if name != "" && name != "__MSBROWSE__" && name != "__BROWSE__" && !strings.HasSuffix(name, "$") {
			return name
		}
	}
	return ""
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

func subnetHosts(n *net.IPNet) []net.IP {
	ip := n.IP.To4()
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
