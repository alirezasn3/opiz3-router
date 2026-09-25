//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/wifiscan"
)

const (
	// displayPageFile remembers the display page across restarts.
	displayPageFile = "/var/lib/opiz3-router/display-page"
	// captureRetry is the pause after a failed capture before trying again.
	captureRetry = 30 * time.Second
	// maxListedNetworks bounds the networks sent to the panel.
	maxListedNetworks = 40
)

// monitorController detects the monitor-mode Wi-Fi adapter, captures beacons
// with it while it is plugged in, and chooses what the display shows.
type monitorController struct {
	cfg      config.Config
	store    *state.Store
	pageFile string

	mu       sync.Mutex
	page     string
	running  bool
	cancel   context.CancelFunc
	failedAt time.Time
}

func newMonitorController(cfg config.Config, store *state.Store) *monitorController {
	return newMonitorControllerWithFile(cfg, store, displayPageFile)
}

// newMonitorControllerWithFile restores the display page saved in pageFile.
func newMonitorControllerWithFile(cfg config.Config, store *state.Store, pageFile string) *monitorController {
	m := &monitorController{cfg: cfg, store: store, pageFile: pageFile, page: state.PageStatus}
	if data, e := os.ReadFile(pageFile); e == nil {
		switch strings.TrimSpace(string(data)) {
		case state.PageChannels, state.PageNetwork:
			m.page = strings.TrimSpace(string(data))
		}
	}
	return m
}

func (m *monitorController) adapterPresent() bool {
	_, e := netlink.LinkByName(m.cfg.Monitor.Interface)
	return e == nil
}

// SetDisplayPage chooses what the display shows and remembers it.
func (m *monitorController) SetDisplayPage(page string) error {
	if page != state.PageStatus && page != state.PageChannels && page != state.PageNetwork {
		return fmt.Errorf("unknown page %q", page)
	}
	if page == state.PageChannels && !m.adapterPresent() {
		return fmt.Errorf("the Wi-Fi adapter %s is not plugged in", m.cfg.Monitor.Interface)
	}
	m.mu.Lock()
	m.page = page
	m.mu.Unlock()
	if e := os.MkdirAll(filepath.Dir(m.pageFile), 0o755); e == nil {
		if e = os.WriteFile(m.pageFile, []byte(page+"\n"), 0o644); e != nil {
			log.Printf("monitor: remember display page: %v", e)
		}
	}
	m.store.Update(func(s *state.Snapshot) { s.Monitor.Page = page })
	return nil
}

// Run watches for the adapter until ctx is cancelled.
func (m *monitorController) Run(ctx context.Context) {
	every(ctx, 5*time.Second, func() { m.reconcile(ctx) })
}

func (m *monitorController) reconcile(ctx context.Context) {
	iface := m.cfg.Monitor.Interface
	present := m.adapterPresent()
	driver := ""
	if present {
		if p, e := filepath.EvalSymlinks(filepath.Join("/sys/class/net", iface, "device", "driver")); e == nil {
			driver = filepath.Base(p)
		}
	}

	m.mu.Lock()
	page := m.page
	start := m.cfg.Monitor.Enabled && present && !m.running && time.Since(m.failedAt) >= captureRetry
	if start {
		cctx, cancel := context.WithCancel(ctx)
		m.running, m.cancel = true, cancel
		go m.capture(cctx)
	}
	m.mu.Unlock()

	m.store.Update(func(s *state.Snapshot) {
		mon := &s.Monitor
		if mon.Present != present && mon.Interface != "" {
			if present {
				s.Notify(state.Info, "monitor", "Wi-Fi adapter "+iface+" plugged in")
			} else {
				s.Notify(state.Warning, "monitor", "Wi-Fi adapter "+iface+" removed")
			}
		}
		mon.Page, mon.Interface, mon.Present, mon.Driver = page, iface, present, driver
	})
}

// capture puts the adapter into monitor mode, hops through the channels and
// feeds beacons to a tracker, publishing a summary every second.
func (m *monitorController) capture(ctx context.Context) {
	iface := m.cfg.Monitor.Interface
	fail := func(e error) {
		log.Printf("monitor: %s: %v", iface, e)
		m.mu.Lock()
		m.failedAt = time.Now()
		m.mu.Unlock()
		m.store.Update(func(s *state.Snapshot) {
			s.Monitor.Active, s.Monitor.Error = false, e.Error()
		})
	}
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	if e := setWifiType(iface, "monitor"); e != nil {
		fail(e)
		return
	}
	// Hand the adapter back in managed mode when capture stops.
	defer func() {
		if m.adapterPresent() {
			_ = setWifiType(iface, "managed")
		}
	}()
	fd, e := openCapture(iface)
	if e != nil {
		fail(e)
		return
	}
	defer unix.Close(fd)

	own := ""
	if l, e := netlink.LinkByName(m.cfg.Interfaces.Wireless); e == nil {
		own = l.Attrs().HardwareAddr.String()
	}
	tracker := wifiscan.NewTracker(own)
	log.Printf("monitor: capturing on %s", iface)
	m.store.Update(func(s *state.Snapshot) { s.Monitor.Active, s.Monitor.Error = true, "" })
	defer m.store.Update(func(s *state.Snapshot) {
		s.Monitor.Active, s.Monitor.Channels, s.Monitor.Networks = false, nil, nil
	})

	hopErr := make(chan error, 1)
	var scanChannel atomic.Int32
	go hopChannels(ctx, iface, time.Duration(m.cfg.Monitor.HopIntervalMS)*time.Millisecond, &scanChannel, hopErr)

	buf := make([]byte, wifiscan.Snaplen)
	lastPublish := time.Time{}
	for ctx.Err() == nil {
		select {
		case e := <-hopErr:
			fail(e)
			return
		default:
		}
		n, e := readFrame(fd, buf)
		switch {
		case errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EINTR):
			// Receive timeout: go round to check for cancellation.
		case e != nil:
			fail(fmt.Errorf("capture: %w", e)) // e.g. the adapter was unplugged
			return
		default:
			if b, ok := wifiscan.ParseFrame(buf[:n]); ok {
				tracker.Add(b, time.Now())
			}
		}
		if now := time.Now(); now.Sub(lastPublish) >= time.Second {
			lastPublish = now
			m.publish(tracker.Summarize(now, hostapdChannel(m.cfg.Monitor.HostapdConfig)), int(scanChannel.Load()), now)
		}
	}
}

func (m *monitorController) publish(sum wifiscan.Summary, scanChannel int, now time.Time) {
	channels := make([]state.MonitorChannel, len(sum.Channels))
	for i, c := range sum.Channels {
		channels[i] = state.MonitorChannel{Channel: c.Channel, Networks: c.Networks, Strongest: c.Strongest}
	}
	var networks []state.MonitorNetwork
	for _, n := range sum.Networks[:min(len(sum.Networks), maxListedNetworks)] {
		networks = append(networks, state.MonitorNetwork{SSID: n.SSID, Channel: n.Channel, Signal: n.Signal})
	}
	m.store.Update(func(s *state.Snapshot) {
		mon := &s.Monitor
		mon.Channels, mon.Networks = channels, networks
		mon.OwnChannel, mon.OwnSignal, mon.Recommended, mon.UpdatedAt = sum.OwnChannel, sum.OwnSignal, sum.Recommended, now
		mon.ScanChannel, mon.HopIntervalMS = scanChannel, m.cfg.Monitor.HopIntervalMS
	})
}

// hopChannels moves the adapter through channels 1 to 13, listening on each
// for interval, until ctx ends. The channel in use is stored in current.
func hopChannels(ctx context.Context, iface string, interval time.Duration, current *atomic.Int32, errc chan<- error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	failures := 0
	for ch := 1; ; ch = ch%wifiscan.Channels + 1 {
		if e := runIW("dev", iface, "set", "channel", strconv.Itoa(ch)); e != nil {
			// Some channels may be disallowed by the regulatory domain; only
			// give up when hopping fails for a whole pass.
			if failures++; failures >= wifiscan.Channels {
				errc <- fmt.Errorf("cannot change channel: %w", e)
				return
			}
		} else {
			failures = 0
			current.Store(int32(ch))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func runIW(args ...string) error {
	if out, e := exec.Command("iw", args...).CombinedOutput(); e != nil {
		return fmt.Errorf("iw %s: %v: %s", strings.Join(args, " "), e, strings.TrimSpace(string(out)))
	}
	return nil
}

// setWifiType switches a Wi-Fi interface to "monitor" or "managed" mode,
// which requires it to be down.
func setWifiType(iface, mode string) error {
	link, e := netlink.LinkByName(iface)
	if e != nil {
		return e
	}
	if e = netlink.LinkSetDown(link); e != nil {
		return e
	}
	if e = runIW("dev", iface, "set", "type", mode); e != nil {
		return e
	}
	return netlink.LinkSetUp(link)
}

// openCapture opens a raw packet socket on iface that only receives beacons
// and probe responses, with a one second receive timeout so capture can stop.
// It needs no libpcap, so the binary stays static.
func openCapture(iface string) (int, error) {
	link, e := netlink.LinkByName(iface)
	if e != nil {
		return -1, e
	}
	proto := htons(unix.ETH_P_ALL)
	fd, e := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(proto))
	if e != nil {
		return -1, fmt.Errorf("packet socket: %w", e)
	}
	filter, e := wifiscan.Filter()
	if e == nil {
		prog := make([]unix.SockFilter, len(filter))
		for i, f := range filter {
			prog[i] = unix.SockFilter{Code: f.Op, Jt: f.Jt, Jf: f.Jf, K: f.K}
		}
		// Attach before binding so no unfiltered frames are queued.
		e = unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]})
	}
	if e == nil {
		e = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1})
	}
	if e == nil {
		e = unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: link.Attrs().Index})
	}
	if e != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("packet socket: %w", e)
	}
	return fd, nil
}

// readFrame reads one captured frame into buf and returns its length. It
// skips frames sent by this machine and returns EAGAIN when the receive
// timeout passes without a frame.
func readFrame(fd int, buf []byte) (int, error) {
	for {
		n, from, e := unix.Recvfrom(fd, buf, 0)
		if e != nil {
			return 0, e
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		return n, nil
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// hostapdChannel reads the access point's channel from hostapd's config, or
// returns 0 when it is not set there (e.g. automatic channel selection).
func hostapdChannel(path string) int {
	f, e := os.Open(path)
	if e != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "channel="); ok {
			if ch, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
				return ch
			}
		}
	}
	return 0
}
