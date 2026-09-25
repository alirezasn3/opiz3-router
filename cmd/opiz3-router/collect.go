//go:build linux

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	dwr910m "github.com/alirezasn3/dlink-dwr-910m-api"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/sysinfo"
)

// startCollectors keeps store up to date until ctx is cancelled.
func startCollectors(ctx context.Context, cfg config.Config, store *state.Store) error {
	wgc, e := wgctrl.New()
	if e != nil {
		return e
	}
	go func() {
		<-ctx.Done()
		wgc.Close()
	}()

	dwr := dwr910m.New("http://"+cfg.Modem.Address, 2*time.Second)
	links := &linkCollector{cfg: cfg, prev: map[string]counterSample{}}
	wg := &wireguardCollector{name: cfg.WireGuard.Interface, client: wgc, prev: map[string]counterSample{}}

	go every(ctx, time.Second, func() { wg.collect(store) })
	go every(ctx, 2*time.Second, func() { links.collect(store) })
	go every(ctx, 3*time.Second, func() { collectModem(dwr, store) })
	sms := newSMSCollector(dwr)
	go every(ctx, smsInterval, func() { sms.collect(store) })
	go every(ctx, 5*time.Second, func() { collectClients(cfg, store) })
	go every(ctx, 5*time.Second, func() { collectSystem(store) })
	return nil
}

// every runs fn now and then at each interval until ctx is cancelled.
func every(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type counterSample struct {
	at     time.Time
	rx, tx uint64
}

// rates returns bytes per second since the previous sample, or zero when there
// is none or the counters were reset.
func rates(prev, cur counterSample) (float64, float64) {
	dt := cur.at.Sub(prev.at).Seconds()
	if prev.at.IsZero() || dt <= 0 || cur.rx < prev.rx || cur.tx < prev.tx {
		return 0, 0
	}
	return float64(cur.rx-prev.rx) / dt, float64(cur.tx-prev.tx) / dt
}

type linkCollector struct {
	cfg  config.Config
	prev map[string]counterSample
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
		// A port of the LAN bridge (end0 in server mode, wlan0 when hostapd
		// bridges it) is part of the LAN rather than an uplink.
		if bridgeIndex != 0 && a.MasterIndex == bridgeIndex {
			iface.Role = "lan-port"
			if item.role == "wan-wifi" {
				iface.Role = "wifi"
			}
		}
		iface.Present = true
		iface.OperState = a.OperState.String()
		// Tunnels such as WireGuard report an "unknown" operational state.
		iface.Up = a.Flags&net.FlagUp != 0 && (a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown)
		if len(a.HardwareAddr) > 0 {
			iface.MAC = a.HardwareAddr.String()
		}
		if addrs, e := netlink.AddrList(link, netlink.FAMILY_ALL); e == nil {
			for _, addr := range addrs {
				// Skip IPv6 link-local addresses; they are noise on a dashboard.
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

type wireguardCollector struct {
	name   string
	client *wgctrl.Client
	prev   map[string]counterSample
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

// cleanModemText collapses the whitespace, line breaks and &nbsp; the modem's
// status page leaves inside values into single spaces.
func cleanModemText(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "&nbsp;", " ")), " ")
}

func collectModem(dwr *dwr910m.DWR910M, store *state.Store) {
	var m state.Modem
	st, e := dwr.GetStatus()
	if e != nil {
		m.Error = e.Error()
	} else {
		m = state.Modem{
			Reachable:        true,
			NetworkType:      cleanModemText(st.NetworkType),
			SignalStrength:   cleanModemText(st.SignalStrength),
			SignalLevel:      st.SignalStrengthLevel,
			IPAddress:        cleanModemText(st.IPAddress),
			Uptime:           cleanModemText(st.Uptime),
			ConnectedDevices: st.ConnectedDevices,
			SoftwareVersion:  cleanModemText(st.SoftwareVersion),
		}
	}
	store.Update(func(s *state.Snapshot) { s.Modem = m })
}

// smsInterval is how often the modem's inbox is read.
const smsInterval = 20 * time.Second

// smsLimit is the number of messages shown in the panel.
const smsLimit = 50

// smsMaxPages bounds how many inbox pages are read per poll.
const smsMaxPages = 30

type smsCollector struct {
	modem *dwr910m.DWR910M
	// seen maps each message to when the router first saw it (zero for
	// messages already there at startup), so only new ones notify and new
	// ones can be listed first. Messages are keyed by content rather than by
	// the modem's message ID, which is not guaranteed to stay the same.
	seen    map[string]time.Time
	primed  bool
	lastErr string
	// badDates holds date formats already logged as unreadable.
	badDates map[string]bool
}

func newSMSCollector(modem *dwr910m.DWR910M) *smsCollector {
	return &smsCollector{modem: modem, seen: map[string]time.Time{}, badDates: map[string]bool{}}
}

// modemDateLayouts are the date formats modems are known to use. The modem
// reports its local time.
var modemDateLayouts = []string{
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"2006-01-02T15:04:05",
	"02/01/2006 15:04:05",
	"2006-01-02 15:04",
	"02/01/2006 15:04",
}

func parseModemDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range modemDateLayouts {
		if t, e := time.ParseInLocation(layout, s, time.Local); e == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// dateShape reduces a date to its format, e.g. "25/09/2026 10:00" to
// "00/00/0000 00:00", so each unreadable format is logged once.
func dateShape(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '0'
		}
		return r
	}, s)
}

func smsKey(m dwr910m.Message) string {
	return m.Sender + "\x00" + m.Date + "\x00" + m.Content
}

// readInbox reads every page of the modem's inbox. The library's GetMessages
// stops after a given count, which misses new messages when the inbox lists
// the oldest first, and fails the whole read if a single date does not parse.
func readInbox(modem *dwr910m.DWR910M) ([]dwr910m.Message, error) {
	var all []dwr910m.Message
	for page := 1; page <= smsMaxPages; page++ {
		res, e := modem.C.Get(fmt.Sprintf("%s/PageList?pageIndex=%d", modem.Address, page))
		if e != nil {
			return nil, e
		}
		var p dwr910m.Messages
		e = json.NewDecoder(res.Body).Decode(&p)
		res.Body.Close()
		if e != nil {
			return nil, fmt.Errorf("inbox page %d: %w", page, e)
		}
		all = append(all, p.Messages...)
		if len(p.Messages) == 0 || int64(page) >= p.TotalPages {
			break
		}
	}
	return all, nil
}

func (c *smsCollector) collect(store *state.Store) {
	msgs, e := readInbox(c.modem)
	if e != nil {
		if e.Error() != c.lastErr {
			log.Printf("sms: cannot read the modem's inbox: %v", e)
			c.lastErr = e.Error()
		}
		// Keep showing the last messages read while the modem is away.
		store.Update(func(s *state.Snapshot) { s.SMS.Error = e.Error() })
		return
	}
	c.lastErr = ""

	var inbox, fresh []state.SMS
	listed := map[string]bool{}
	for _, m := range msgs {
		key := smsKey(m)
		if listed[key] {
			continue // the same message listed twice
		}
		listed[key] = true
		t, ok := parseModemDate(m.Date)
		if shape := dateShape(m.Date); !ok && !c.badDates[shape] {
			c.badDates[shape] = true
			log.Printf("sms: cannot read the modem's date %q; ordering by arrival instead", m.Date)
		}
		received, known := c.seen[key]
		if !known {
			// Messages already there when the router starts are not new.
			if c.primed {
				received = time.Now()
			}
			c.seen[key] = received
		}
		sms := state.SMS{ID: m.ID, Sender: m.Sender, Time: t, ReceivedAt: received, Text: m.Content}
		inbox = append(inbox, sms)
		if !known && c.primed {
			fresh = append(fresh, sms)
		}
	}
	if !c.primed {
		log.Printf("sms: %d messages in the modem's inbox", len(inbox))
	}
	c.primed = true

	// Messages that arrived while the router runs come first, newest first:
	// the modem's dates and IDs cannot be relied on to put them on top, and
	// only the first smsLimit messages are shown.
	slices.SortStableFunc(inbox, func(a, b state.SMS) int {
		return cmp.Or(b.ReceivedAt.Compare(a.ReceivedAt), b.Time.Compare(a.Time), cmp.Compare(b.ID, a.ID))
	})
	if len(inbox) > smsLimit {
		inbox = inbox[:smsLimit]
	}
	store.Update(func(s *state.Snapshot) {
		s.SMS = state.SMSInbox{Available: true, Messages: inbox}
	})

	// Oldest first, so the newest ends up as the latest notification.
	slices.SortStableFunc(fresh, func(a, b state.SMS) int { return a.Time.Compare(b.Time) })
	for _, m := range fresh {
		log.Printf("sms: new message from %s", m.Sender)
		store.Notify(state.Info, "sms", "SMS from "+m.Sender)
	}
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

	// The neighbour table shows wired clients and ones with static addresses.
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

func collectSystem(store *state.Store) {
	var sys state.System
	sys.Hostname, _ = os.Hostname()
	if data, e := os.ReadFile("/proc/sys/kernel/osrelease"); e == nil {
		sys.Kernel = strings.TrimSpace(string(data))
	}
	if data, e := os.ReadFile("/proc/uptime"); e == nil {
		sys.UptimeSeconds, _ = sysinfo.ParseUptime(string(data))
	}
	if data, e := os.ReadFile("/proc/loadavg"); e == nil {
		sys.Load, _ = sysinfo.ParseLoadavg(string(data))
	}
	if f, e := os.Open("/proc/meminfo"); e == nil {
		sys.MemTotal, sys.MemAvailable, _ = sysinfo.ParseMeminfo(f)
		f.Close()
	}
	if data, e := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); e == nil {
		if t, e := sysinfo.ParseMilliCelsius(string(data)); e == nil {
			sys.TemperatureC = &t
		}
	}
	store.Update(func(s *state.Snapshot) { s.System = sys })
}
