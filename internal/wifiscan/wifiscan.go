// Package wifiscan turns 802.11 beacons captured by a Wi-Fi adapter in
// monitor mode into a picture of the 2.4 GHz channels: how many networks use
// each channel, how strong they are, and which channel is the least busy.
package wifiscan

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/bpf"
)

// Channels are the 2.4 GHz channels that are scanned.
const Channels = 13

// Expiry is how long a network is kept after its last beacon.
const Expiry = 90 * time.Second

// Snaplen is how much of each frame the capture filter keeps; beacons fit.
const Snaplen = 4096

// Beacon is what one beacon or probe response says about a network.
type Beacon struct {
	BSSID   string
	SSID    string
	Channel int
	// Signal is in dBm, or 0 when the adapter does not report it.
	Signal int
}

// Filter is a classic BPF program that keeps only beacons and probe
// responses, so the kernel drops all other traffic before it reaches Go. It
// reads the radiotap header's length to find the 802.11 frame control byte.
func Filter() ([]bpf.RawInstruction, error) {
	return bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: 3, Size: 1},                       // radiotap length, high byte
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 0, SkipTrue: 5}, // longer than 255: drop
		bpf.LoadAbsolute{Off: 2, Size: 1},                       // radiotap length, low byte
		bpf.TAX{},                                               // X = start of the 802.11 frame
		bpf.LoadIndirect{Off: 0, Size: 1},                       // frame control
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 0x80, SkipTrue: 2}, // beacon
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 0x50, SkipTrue: 1}, // probe response
		bpf.RetConstant{Val: 0},                                 // drop
		bpf.RetConstant{Val: Snaplen},                           // keep
	})
}

// ParseFrame decodes a radiotap-framed beacon or probe response.
func ParseFrame(data []byte) (Beacon, bool) {
	p := gopacket.NewPacket(data, layers.LayerTypeRadioTap, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	d, ok := p.Layer(layers.LayerTypeDot11).(*layers.Dot11)
	if !ok {
		return Beacon{}, false
	}
	// The information elements follow the fixed fields of the frame body.
	var elements []byte
	switch d.Type {
	case layers.Dot11TypeMgmtBeacon:
		if l := p.Layer(layers.LayerTypeDot11MgmtBeacon); l != nil {
			elements = l.LayerPayload()
		}
	case layers.Dot11TypeMgmtProbeResp:
		if l := p.Layer(layers.LayerTypeDot11MgmtProbeResp); l != nil {
			elements = l.LayerPayload()
		}
	default:
		return Beacon{}, false
	}
	b := Beacon{BSSID: d.Address3.String()}
	rt, _ := p.Layer(layers.LayerTypeRadioTap).(*layers.RadioTap)
	if rt != nil && rt.Present.DBMAntennaSignal() {
		b.Signal = int(rt.DBMAntennaSignal)
	}
	// gopacket's element decoder rejects valid short elements at the end of
	// a frame (and then drops the rest), so the elements are read here.
	for len(elements) >= 2 {
		id, n := elements[0], int(elements[1])
		if len(elements) < 2+n {
			break
		}
		info := elements[2 : 2+n]
		switch layers.Dot11InformationElementID(id) {
		case layers.Dot11InformationElementIDSSID:
			// Hidden networks send an empty or zero-filled name.
			b.SSID = strings.TrimRight(string(info), "\x00")
		case layers.Dot11InformationElementIDDSSet:
			if n == 1 {
				b.Channel = int(info[0])
			}
		}
		elements = elements[2+n:]
	}
	// Without a DS parameter set, fall back to the frequency it was heard on.
	if b.Channel == 0 && rt != nil && rt.Present.Channel() {
		b.Channel = FrequencyChannel(int(rt.ChannelFrequency))
	}
	return b, b.Channel >= 1 && b.Channel <= Channels
}

// FrequencyChannel returns the 2.4 GHz channel for a frequency in MHz, or 0.
func FrequencyChannel(mhz int) int {
	if mhz >= 2412 && mhz <= 2472 && (mhz-2407)%5 == 0 {
		return (mhz - 2407) / 5
	}
	return 0
}

type network struct {
	ssid    string
	channel int
	signal  float64 // smoothed dBm
	last    time.Time
}

// Tracker collects beacons. It is not safe for concurrent use.
type Tracker struct {
	own      string // BSSID of the router's own access point
	networks map[string]*network
	// The own access point as last heard: channel and smoothed signal.
	ownSeen   int
	ownSignal float64
	ownLast   time.Time
}

// NewTracker returns a Tracker that leaves out the network with ownBSSID
// (the router's own access point) from the counts.
func NewTracker(ownBSSID string) *Tracker {
	return &Tracker{own: strings.ToLower(ownBSSID), networks: map[string]*network{}}
}

func (t *Tracker) Add(b Beacon, now time.Time) {
	if strings.EqualFold(b.BSSID, t.own) {
		t.ownSeen, t.ownLast = b.Channel, now
		if b.Signal != 0 {
			if t.ownSignal == 0 {
				t.ownSignal = float64(b.Signal)
			}
			t.ownSignal = 0.7*t.ownSignal + 0.3*float64(b.Signal)
		}
		return
	}
	n, ok := t.networks[b.BSSID]
	if !ok {
		n = &network{signal: float64(b.Signal)}
		t.networks[b.BSSID] = n
	}
	n.ssid, n.channel, n.last = b.SSID, b.Channel, now
	if b.Signal != 0 {
		// Smooth out beacon-to-beacon noise.
		n.signal = 0.7*n.signal + 0.3*float64(b.Signal)
	}
}

type Network struct {
	SSID    string
	Channel int
	Signal  int
}

type Channel struct {
	Channel  int
	Networks int
	// Strongest is the strongest signal on the channel in dBm, 0 if none.
	Strongest int
}

type Summary struct {
	Networks []Network // strongest first
	Channels []Channel // 1..Channels
	// OwnChannel is the router's own channel: the one given to Summarize,
	// or else the one its beacons were heard on; 0 if unknown.
	OwnChannel int
	// OwnSignal is how strongly the adapter hears the own access point, in
	// dBm, or 0 if it has not been heard within Expiry.
	OwnSignal int
	// Recommended is the least busy of the non-overlapping channels 1, 6
	// and 11.
	Recommended int
}

// Summarize drops networks not heard for Expiry and describes the rest.
func (t *Tracker) Summarize(now time.Time, ownChannel int) Summary {
	s := Summary{OwnChannel: ownChannel}
	if s.OwnChannel == 0 {
		s.OwnChannel = t.ownSeen
	}
	if !t.ownLast.IsZero() && now.Sub(t.ownLast) <= Expiry {
		s.OwnSignal = int(math.Round(t.ownSignal))
	}
	s.Channels = make([]Channel, Channels)
	for i := range s.Channels {
		s.Channels[i].Channel = i + 1
	}
	for bssid, n := range t.networks {
		if now.Sub(n.last) > Expiry {
			delete(t.networks, bssid)
			continue
		}
		sig := int(math.Round(n.signal))
		s.Networks = append(s.Networks, Network{SSID: n.ssid, Channel: n.channel, Signal: sig})
		c := &s.Channels[n.channel-1]
		c.Networks++
		if c.Strongest == 0 || sig > c.Strongest {
			c.Strongest = sig
		}
	}
	slices.SortFunc(s.Networks, func(a, b Network) int {
		return cmp.Or(cmp.Compare(b.Signal, a.Signal), cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.SSID, b.SSID))
	})
	s.Recommended = recommend(s.Networks, s.OwnChannel)
	return s
}

// recommend picks the channel among 1, 6 and 11 with the least interference.
// Channels closer than 5 apart overlap; the closer and stronger a network,
// the more it counts. Ties keep the current channel, then the lowest.
func recommend(networks []Network, current int) int {
	best, bestScore := 0, math.Inf(1)
	for _, c := range []int{1, 6, 11} {
		score := 0.0
		for _, n := range networks {
			d := math.Abs(float64(c - n.Channel))
			if d >= 5 {
				continue
			}
			// Map -100..-30 dBm to 1..71; unknown signals count as weak.
			w := 1.0
			if n.Signal != 0 {
				w = max(1, float64(n.Signal)+100)
			}
			score += w * (1 - d/5)
		}
		if score < bestScore || (score == bestScore && c == current) {
			best, bestScore = c, score
		}
	}
	return best
}
