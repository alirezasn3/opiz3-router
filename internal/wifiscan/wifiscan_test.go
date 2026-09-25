package wifiscan

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/net/bpf"
)

type frameOpts struct {
	frameControl byte // 0x80 beacon, 0x50 probe response, 0x08 data
	freq         int  // radiotap channel frequency, 0 to leave out
	noDSSet      bool
	signal       int8
}

// frame builds a radiotap header with a channel and signal field, followed
// by an 802.11 management frame carrying an SSID and a DS parameter set.
func frame(bssid, ssid string, channel int, o frameOpts) []byte {
	if o.frameControl == 0 {
		o.frameControl = 0x80
	}
	rt := make([]byte, 13)
	binary.LittleEndian.PutUint16(rt[2:], 13)
	binary.LittleEndian.PutUint32(rt[4:], 1<<3|1<<5) // channel, dBm antenna signal
	binary.LittleEndian.PutUint16(rt[8:], uint16(o.freq))
	rt[12] = byte(o.signal)

	mac, _ := net.ParseMAC(bssid)
	hdr := []byte{o.frameControl, 0, 0, 0}
	hdr = append(hdr, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	hdr = append(hdr, mac...)
	hdr = append(hdr, mac...)
	hdr = append(hdr, 0, 0)
	body := make([]byte, 12) // timestamp, beacon interval, capabilities
	body = append(body, 0, byte(len(ssid)))
	body = append(body, ssid...)
	if !o.noDSSet {
		body = append(body, 3, 1, byte(channel))
	}
	return append(append(rt, hdr...), body...)
}

func TestParseFrame(t *testing.T) {
	b, ok := ParseFrame(frame("aa:bb:cc:00:00:01", "Home", 6, frameOpts{freq: 2437, signal: -52}))
	if !ok || b != (Beacon{BSSID: "aa:bb:cc:00:00:01", SSID: "Home", Channel: 6, Signal: -52}) {
		t.Fatalf("beacon = %+v %v", b, ok)
	}
	b, ok = ParseFrame(frame("aa:bb:cc:00:00:02", "", 11, frameOpts{frameControl: 0x50, freq: 2462, noDSSet: true, signal: -80}))
	if !ok || b.Channel != 11 || b.SSID != "" {
		t.Fatalf("probe response without DS set = %+v %v", b, ok)
	}
	// No radiotap channel: the channel must come from the DS parameter set,
	// which is the last element here.
	b, ok = ParseFrame(frame("aa:bb:cc:00:00:05", "Cafe", 3, frameOpts{signal: -70}))
	if !ok || b.Channel != 3 || b.SSID != "Cafe" {
		t.Fatalf("channel from DS parameter set = %+v %v", b, ok)
	}
	if _, ok = ParseFrame(frame("aa:bb:cc:00:00:03", "x", 1, frameOpts{frameControl: 0x08})); ok {
		t.Fatal("data frame parsed as a beacon")
	}
	if _, ok = ParseFrame(frame("aa:bb:cc:00:00:04", "5GHz", 36, frameOpts{freq: 5180})); ok {
		t.Fatal("5 GHz network accepted")
	}
	if _, ok = ParseFrame([]byte{0, 0, 8}); ok {
		t.Fatal("truncated frame parsed")
	}
}

func TestFilter(t *testing.T) {
	prog, e := Filter()
	if e != nil {
		t.Fatal(e)
	}
	insns := make([]bpf.Instruction, len(prog))
	for i, r := range prog {
		insns[i] = r.Disassemble()
	}
	vm, e := bpf.NewVM(insns)
	if e != nil {
		t.Fatal(e)
	}
	long := frame("aa:bb:cc:00:00:01", "x", 1, frameOpts{})
	binary.LittleEndian.PutUint16(long[2:], 300) // radiotap header over 255 bytes
	for name, tc := range map[string]struct {
		pkt  []byte
		keep bool
	}{
		"beacon":         {frame("aa:bb:cc:00:00:01", "x", 1, frameOpts{}), true},
		"probe response": {frame("aa:bb:cc:00:00:01", "x", 1, frameOpts{frameControl: 0x50}), true},
		"data":           {frame("aa:bb:cc:00:00:01", "x", 1, frameOpts{frameControl: 0x08}), false},
		"probe request":  {frame("aa:bb:cc:00:00:01", "x", 1, frameOpts{frameControl: 0x40}), false},
		"long radiotap":  {long, false},
	} {
		n, e := vm.Run(tc.pkt)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if (n > 0) != tc.keep {
			t.Errorf("%s: kept %d bytes, want keep=%v", name, n, tc.keep)
		}
	}
}

func TestFrequencyChannel(t *testing.T) {
	for mhz, want := range map[int]int{2412: 1, 2437: 6, 2472: 13, 2484: 0, 5180: 0, 2413: 0} {
		if got := FrequencyChannel(mhz); got != want {
			t.Errorf("FrequencyChannel(%d) = %d, want %d", mhz, got, want)
		}
	}
}

func TestTrackerSummary(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tr := NewTracker("DC:44:6D:12:34:56")
	add := func(bssid, ssid string, ch, sig int, at time.Time) {
		tr.Add(Beacon{BSSID: bssid, SSID: ssid, Channel: ch, Signal: sig}, at)
	}
	add("dc:44:6d:12:34:56", "OrangePiZero3", 6, -30, now) // our own access point
	add("aa:00:00:00:00:01", "Neighbour", 1, -45, now)
	add("aa:00:00:00:00:02", "Cafe", 1, -70, now)
	add("aa:00:00:00:00:03", "Office", 3, -60, now)
	add("aa:00:00:00:00:04", "Old", 11, -40, now.Add(-2*Expiry)) // expired

	s := tr.Summarize(now, 0)
	if len(s.Networks) != 3 || s.Networks[0].SSID != "Neighbour" {
		t.Fatalf("networks = %+v", s.Networks)
	}
	if c := s.Channels[0]; c.Channel != 1 || c.Networks != 2 || c.Strongest != -45 {
		t.Fatalf("channel 1 = %+v", c)
	}
	if s.Channels[5].Networks != 0 {
		t.Fatal("own access point counted")
	}
	if s.OwnChannel != 6 || s.OwnSignal != -30 {
		t.Fatalf("own access point = channel %d, signal %d", s.OwnChannel, s.OwnSignal)
	}
	if s := tr.Summarize(now.Add(2*Expiry), 0); s.OwnSignal != 0 {
		t.Fatalf("own signal kept after expiry: %d", s.OwnSignal)
	}
	// Channels 1 and 3 are busy and overlap 6 a little; 11 is free.
	if s.Recommended != 11 {
		t.Fatalf("recommended = %d", s.Recommended)
	}
	if s := tr.Summarize(now, 1); s.OwnChannel != 1 {
		t.Fatalf("configured own channel ignored: %d", s.OwnChannel)
	}
}

func TestRecommendKeepsCurrentOnTie(t *testing.T) {
	if got := recommend(nil, 6); got != 6 {
		t.Fatalf("empty air, current 6: got %d", got)
	}
	if got := recommend(nil, 0); got != 1 {
		t.Fatalf("empty air, no current: got %d", got)
	}
	// A strong network on 6 pushes the choice away from it.
	if got := recommend([]Network{{Channel: 6, Signal: -40}}, 6); got == 6 {
		t.Fatal("recommended the busy channel")
	}
}
