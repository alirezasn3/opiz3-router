// Package state holds the latest status of the router. Collectors write into
// a Store and the screen and web panel read snapshots from it.
package state

import (
	"sync"
	"time"
)

// HistoryLength is the number of traffic samples kept (one per second).
const HistoryLength = 180

// NotificationLimit is the number of notifications kept, newest last.
const NotificationLimit = 100

type Snapshot struct {
	Time       time.Time   `json:"time"`
	Interfaces []Interface `json:"interfaces"`
	WireGuard  WireGuard   `json:"wireguard"`
	Modem      Modem       `json:"modem"`
	Clients    []Client    `json:"clients"`
	// WirelessInfo is false when per-station Wi-Fi data (iw station dump)
	// is unavailable, so clients cannot be split into Wi-Fi and wired.
	WirelessInfo  bool           `json:"wireless_info"`
	SMS           SMSInbox       `json:"sms"`
	Network       Network        `json:"network"`
	Monitor       Monitor        `json:"monitor"`
	NetworkScan   NetworkScan    `json:"network_scan"`
	Notifications []Notification `json:"notifications"`
	System        System         `json:"system"`
	Routing       Routing        `json:"routing"`
	// Traffic is the VPN throughput history, oldest first.
	Traffic []TrafficSample `json:"traffic"`
}

// NetworkScan is the latest discovery of devices reachable through the
// Ethernet uplink.
type NetworkScan struct {
	Interface string          `json:"interface"`
	Subnet    string          `json:"subnet,omitempty"`
	Active    bool            `json:"active"`
	Error     string          `json:"error,omitempty"`
	Devices   []NetworkDevice `json:"devices"`
	UpdatedAt time.Time       `json:"updated_at,omitzero"`
}

type NetworkDevice struct {
	IP           string `json:"ip"`
	Name         string `json:"name,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	MAC          string `json:"mac,omitempty"`
	Reachable    bool   `json:"reachable"`
}

type Interface struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Present   bool     `json:"present"`
	Up        bool     `json:"up"`
	OperState string   `json:"oper_state"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
	RxBytes   uint64   `json:"rx_bytes"`
	TxBytes   uint64   `json:"tx_bytes"`
	RxRate    float64  `json:"rx_rate"`
	TxRate    float64  `json:"tx_rate"`
}

type WireGuard struct {
	Interface  string `json:"interface"`
	Up         bool   `json:"up"`
	Error      string `json:"error,omitempty"`
	PublicKey  string `json:"public_key,omitempty"`
	ListenPort int    `json:"listen_port,omitempty"`
	Peers      []Peer `json:"peers"`
}

type Peer struct {
	PublicKey     string    `json:"public_key"`
	Endpoint      string    `json:"endpoint,omitempty"`
	AllowedIPs    []string  `json:"allowed_ips"`
	LastHandshake time.Time `json:"last_handshake"`
	RxBytes       int64     `json:"rx_bytes"`
	TxBytes       int64     `json:"tx_bytes"`
	RxRate        float64   `json:"rx_rate"`
	TxRate        float64   `json:"tx_rate"`
}

type Modem struct {
	Reachable        bool   `json:"reachable"`
	Error            string `json:"error,omitempty"`
	NetworkType      string `json:"network_type,omitempty"`
	SignalStrength   string `json:"signal_strength,omitempty"`
	SignalLevel      int64  `json:"signal_level"`
	IPAddress        string `json:"ip_address,omitempty"`
	Uptime           string `json:"uptime,omitempty"`
	ConnectedDevices int64  `json:"connected_devices"`
	SoftwareVersion  string `json:"software_version,omitempty"`
}

// Display pages.
const (
	PageStatus   = "status"
	PageChannels = "channels"
	PageNetwork  = "network"
)

// Monitor is the Wi-Fi adapter in monitor mode and what it hears.
type Monitor struct {
	// Page is what the display shows.
	Page      string `json:"page"`
	Interface string `json:"interface"`
	Present   bool   `json:"present"`
	Driver    string `json:"driver,omitempty"`
	// Active is true while the adapter captures.
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
	// Channels holds 2.4 GHz channels 1 to 13.
	Channels []MonitorChannel `json:"channels"`
	Networks []MonitorNetwork `json:"networks"`
	// OwnChannel is the router's access point channel, 0 if unknown.
	OwnChannel int `json:"own_channel"`
	// OwnSignal is how strongly the adapter hears the router's own access
	// point in dBm, 0 if not heard.
	OwnSignal int `json:"own_signal"`
	// ScanChannel is the channel the adapter is listening on right now.
	ScanChannel int `json:"scan_channel"`
	// HopIntervalMS is how long the adapter listens on each channel.
	HopIntervalMS int       `json:"hop_interval_ms"`
	Recommended   int       `json:"recommended"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
}

type MonitorChannel struct {
	Channel  int `json:"channel"`
	Networks int `json:"networks"`
	// Strongest is in dBm, 0 when the channel is empty.
	Strongest int `json:"strongest"`
}

type MonitorNetwork struct {
	SSID    string `json:"ssid"`
	Channel int    `json:"channel"`
	Signal  int    `json:"signal"`
}

// Network is the state of switching the Ethernet port's mode from the panel.
type Network struct {
	// EthernetMode is "uplink" or "lan", or empty when unknown.
	EthernetMode string `json:"ethernet_mode"`
	// Switchable is false when the router cannot switch the mode, with the
	// reason in Unavailable.
	Switchable  bool   `json:"switchable"`
	Unavailable string `json:"unavailable,omitempty"`
	// Busy is true while a netplan change is being applied.
	Busy bool `json:"busy"`
	// Pending is set after a switch until it is kept, or undone at Deadline.
	Pending *PendingSwitch `json:"pending,omitempty"`
	Error   string         `json:"error,omitempty"`
}

type PendingSwitch struct {
	Mode     string    `json:"mode"`
	Previous string    `json:"previous"`
	Deadline time.Time `json:"deadline"`
}

type SMSInbox struct {
	// Available is false until the modem's inbox has been read once.
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// Messages are newest first.
	Messages []SMS `json:"messages"`
}

type SMS struct {
	ID     int64  `json:"id"`
	Sender string `json:"sender"`
	// Time is the modem's date for the message, zero if it could not be read.
	Time time.Time `json:"time,omitzero"`
	// ReceivedAt is when the router first saw the message, zero for messages
	// already in the inbox when it started.
	ReceivedAt time.Time `json:"received_at,omitzero"`
	Text       string    `json:"text"`
}

// Notification levels.
const (
	Info     = "info"
	Good     = "good"
	Warning  = "warning"
	Critical = "critical"
)

// Notification is an entry in the router's event log, such as a link going
// down or a new SMS.
type Notification struct {
	Time   time.Time `json:"time"`
	Level  string    `json:"level"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
}

type Client struct {
	MAC         string    `json:"mac"`
	IP          string    `json:"ip,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	LeaseExpiry time.Time `json:"lease_expiry,omitzero"`
	Online      bool      `json:"online"`
	Wireless    bool      `json:"wireless"`
	// Signal is in dBm; only set for wireless stations.
	Signal           int    `json:"signal,omitempty"`
	ConnectedSeconds int64  `json:"connected_seconds,omitempty"`
	RxBytes          uint64 `json:"rx_bytes,omitempty"`
	TxBytes          uint64 `json:"tx_bytes,omitempty"`
}

type System struct {
	Hostname      string     `json:"hostname"`
	Kernel        string     `json:"kernel,omitempty"`
	UptimeSeconds float64    `json:"uptime_seconds"`
	Load          [3]float64 `json:"load"`
	MemTotal      uint64     `json:"mem_total"`
	MemAvailable  uint64     `json:"mem_available"`
	// TemperatureC is the SoC temperature, or nil when unavailable.
	TemperatureC *float64 `json:"temperature_c"`
}

// Routing describes whether LAN traffic is protected by the VPN.
type Routing struct {
	IPForward bool `json:"ip_forward"`
	// Rule is the "iif <bridge> lookup <table>" policy rule.
	Rule bool `json:"rule"`
	// VPNRoute is the default route through the WireGuard interface.
	VPNRoute bool `json:"vpn_route"`
	// KillSwitch is the fallback blackhole route that stops LAN traffic from
	// leaking out of the WAN when the VPN route disappears.
	KillSwitch bool `json:"kill_switch"`
	// KillSwitchEnabled is the configured setting; KillSwitch is whether the
	// route is actually present.
	KillSwitchEnabled bool `json:"kill_switch_enabled"`
	// Restores counts how often missing rules or routes were put back,
	// usually after another program such as systemd-networkd removed them.
	Restores          int       `json:"restores"`
	LastRestore       time.Time `json:"last_restore,omitzero"`
	LastRestoreReason string    `json:"last_restore_reason,omitempty"`
	Firewall          bool      `json:"firewall"`
	Error             string    `json:"error,omitempty"`
}

type TrafficSample struct {
	Time   time.Time `json:"t"`
	RxRate float64   `json:"rx"`
	TxRate float64   `json:"tx"`
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	snap Snapshot
	subs map[chan struct{}]struct{}
}

func NewStore() *Store {
	return &Store{subs: map[chan struct{}]struct{}{}}
}

// Update applies fn to the snapshot and notifies subscribers.
func (s *Store) Update(fn func(*Snapshot)) {
	s.mu.Lock()
	fn(&s.snap)
	s.snap.Time = time.Now()
	for ch := range s.subs {
		// Subscribers only need to know that something changed, so a pending
		// notification is enough and slow readers never block collectors.
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.snap
	c.Interfaces = cloneSlice(s.snap.Interfaces)
	for i := range c.Interfaces {
		c.Interfaces[i].Addresses = cloneSlice(c.Interfaces[i].Addresses)
	}
	c.WireGuard.Peers = cloneSlice(s.snap.WireGuard.Peers)
	for i := range c.WireGuard.Peers {
		c.WireGuard.Peers[i].AllowedIPs = cloneSlice(c.WireGuard.Peers[i].AllowedIPs)
	}
	c.Clients = cloneSlice(s.snap.Clients)
	c.SMS.Messages = cloneSlice(s.snap.SMS.Messages)
	c.Notifications = cloneSlice(s.snap.Notifications)
	c.Monitor.Channels = cloneSlice(s.snap.Monitor.Channels)
	c.Monitor.Networks = cloneSlice(s.snap.Monitor.Networks)
	c.NetworkScan.Devices = cloneSlice(s.snap.NetworkScan.Devices)
	if p := s.snap.Network.Pending; p != nil {
		v := *p
		c.Network.Pending = &v
	}
	c.Traffic = cloneSlice(s.snap.Traffic)
	if t := s.snap.System.TemperatureC; t != nil {
		v := *t
		c.System.TemperatureC = &v
	}
	return c
}

// Subscribe returns a channel that receives a value after each update, and a
// function that ends the subscription.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Notify adds a notification to the log.
func (s *Store) Notify(level, source, text string) {
	s.Update(func(snap *Snapshot) { snap.Notify(level, source, text) })
}

// Notify adds a notification to the log, for use inside Store.Update.
func (snap *Snapshot) Notify(level, source, text string) {
	snap.Notifications = append(snap.Notifications, Notification{Time: time.Now(), Level: level, Source: source, Text: text})
	if n := len(snap.Notifications); n > NotificationLimit {
		snap.Notifications = append(snap.Notifications[:0], snap.Notifications[n-NotificationLimit:]...)
	}
}

// LatestNotification returns the newest notification, if any.
func (snap *Snapshot) LatestNotification() (Notification, bool) {
	if len(snap.Notifications) == 0 {
		return Notification{}, false
	}
	return snap.Notifications[len(snap.Notifications)-1], true
}

// AddTraffic appends a throughput sample, keeping the last HistoryLength.
func (snap *Snapshot) AddTraffic(sample TrafficSample) {
	snap.Traffic = append(snap.Traffic, sample)
	if n := len(snap.Traffic); n > HistoryLength {
		snap.Traffic = append(snap.Traffic[:0], snap.Traffic[n-HistoryLength:]...)
	}
}

// cloneSlice copies s, keeping nil as an empty slice so JSON gets [] not null.
func cloneSlice[T any](s []T) []T {
	return append(make([]T, 0, len(s)), s...)
}
