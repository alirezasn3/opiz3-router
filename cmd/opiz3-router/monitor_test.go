//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/wifiscan"
)

func newTestMonitor(t *testing.T, iface string) (*monitorController, *state.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Monitor.Interface = iface
	store := state.NewStore()
	return newMonitorControllerWithFile(cfg, store, filepath.Join(t.TempDir(), "state", "display-page")), store
}

func TestDisplayPageNeedsAdapter(t *testing.T) {
	m, _ := newTestMonitor(t, "nosuchwlan9")
	e := m.SetDisplayPage(state.PageChannels)
	if e == nil || !strings.Contains(e.Error(), "nosuchwlan9") {
		t.Fatalf("error = %v", e)
	}
	if e := m.SetDisplayPage(state.PageStatus); e != nil {
		t.Fatal(e)
	}
	if e := m.SetDisplayPage("clock"); e == nil {
		t.Fatal("unknown page accepted")
	}
	if e := m.SetDisplayPage(state.PageNetwork); e != nil {
		t.Fatalf("network page should not require the Wi-Fi adapter: %v", e)
	}
}

func TestDisplayPageIsRemembered(t *testing.T) {
	// The loopback interface always exists, so it stands in for the adapter.
	m, store := newTestMonitor(t, "lo")
	if e := m.SetDisplayPage(state.PageChannels); e != nil {
		t.Fatal(e)
	}
	if got := store.Snapshot().Monitor.Page; got != state.PageChannels {
		t.Fatalf("published page = %q", got)
	}
	data, e := os.ReadFile(m.pageFile)
	if e != nil || strings.TrimSpace(string(data)) != state.PageChannels {
		t.Fatalf("page file = %q %v", data, e)
	}

	// A restart reads the page back.
	cfg := config.Default()
	cfg.Monitor.Interface = "lo"
	restarted := newMonitorControllerWithFile(cfg, state.NewStore(), m.pageFile)
	if restarted.page != state.PageChannels {
		t.Fatalf("page after restart = %q", restarted.page)
	}
}

func TestHostapdChannel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hostapd.conf")
	if e := os.WriteFile(p, []byte("interface=wlan0\n# channel=1\nchannel=6\nhw_mode=g\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if got := hostapdChannel(p); got != 6 {
		t.Fatalf("channel = %d", got)
	}
	if got := hostapdChannel(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Fatalf("missing file: channel = %d", got)
	}
}

// testFrame builds a radiotap-framed 802.11 management frame; frameControl
// 0x80 is a beacon, 0x08 a data frame.
func testFrame(frameControl byte, ssid string, channel byte) []byte {
	rt := []byte{0, 0, 9, 0, 1 << 5, 0, 0, 0, 0xc0} // radiotap: dBm signal -64
	hdr := []byte{frameControl, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xaa, 0, 0, 0, 0, 1, 0xaa, 0, 0, 0, 0, 1, 0, 0}
	body := append(make([]byte, 12), 0, byte(len(ssid)))
	body = append(append(body, ssid...), 3, 1, channel)
	return append(append(rt, hdr...), body...)
}

// The capture socket's kernel filter must pass beacons and drop the rest,
// and reads must time out so capture can stop. Frames are injected on the
// loopback interface of a throwaway network namespace.
func TestOpenCaptureFiltersBeacons(t *testing.T) {
	inNewNetns(t, func() {
		lo, _ := netlink.LinkByName("lo")
		_ = netlink.LinkSetUp(lo)
		fd, e := openCapture("lo")
		if e != nil {
			t.Skipf("cannot open a packet socket: %v", e)
		}
		defer unix.Close(fd)

		tx, e := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
		if e != nil {
			t.Fatal(e)
		}
		defer unix.Close(tx)
		to := &unix.SockaddrLinklayer{Ifindex: lo.Attrs().Index, Protocol: htons(unix.ETH_P_ALL)}
		for _, f := range [][]byte{testFrame(0x08, "data", 1), testFrame(0x80, "Neighbour", 11)} {
			if e := unix.Sendto(tx, f, 0, to); e != nil {
				t.Fatal(e)
			}
		}

		buf := make([]byte, wifiscan.Snaplen)
		var got []wifiscan.Beacon
		for {
			n, e := readFrame(fd, buf)
			if errors.Is(e, unix.EAGAIN) {
				break // the receive timeout: nothing more is coming
			}
			if e != nil {
				t.Fatal(e)
			}
			b, ok := wifiscan.ParseFrame(buf[:n])
			if !ok {
				t.Fatalf("filter passed a frame that is not a beacon: % x", buf[:n])
			}
			got = append(got, b)
		}
		if len(got) != 1 || got[0].SSID != "Neighbour" || got[0].Channel != 11 || got[0].Signal != -64 {
			t.Fatalf("captured %+v", got)
		}
	})
}
