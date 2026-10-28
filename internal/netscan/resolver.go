package netscan

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"opiz3-router/internal/state"
	"opiz3-router/internal/sysinfo"
)

// resolveNetworkNames tries the names commonly available on a LAN, in
// precedence order: dnsmasq's DHCP lease, ordinary reverse DNS, mDNS,
// LLMNR, and NetBIOS name service.
func resolveNetworkNames(ctx context.Context, devices []state.NetworkDevice, ifIndex int, source net.IP, leasePath string) {
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
			if name := lookupMulticastPTR(ctx, ifIndex, net.ParseIP(ip), "224.0.0.251", 5353); name != "" {
				devices[i].Name = name
				return
			}
			if name := lookupMulticastPTR(ctx, ifIndex, net.ParseIP(ip), "224.0.0.252", 5355); name != "" {
				devices[i].Name = name
				return
			}
			devices[i].Name = lookupNetBIOS(ctx, source, net.ParseIP(ip))
		}(i)
	}
	wg.Wait()
}

func lookupMulticastPTR(ctx context.Context, ifIndex int, ip net.IP, group string, port int) string {
	iface, e := net.InterfaceByIndex(ifIndex)
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
	if v == nil {
		return ""
	}
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
		_, p, e = readDNSName(msg, p)
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
	count := int(buf[56])
	for p := 57; p+18 <= n && count > 0; p, count = p+18, count-1 {
		name := strings.TrimSpace(string(buf[p : p+15]))
		if name != "" && name != "__MSBROWSE__" && name != "__BROWSE__" && !strings.HasSuffix(name, "$") {
			return name
		}
	}
	return ""
}
