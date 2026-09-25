// Package sysinfo parses the text formats the router reads its status from:
// dnsmasq leases, `iw station dump` and /proc files. The parsers take readers
// so they can be tested on any platform.
package sysinfo

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Lease struct {
	// Expiry is zero for infinite leases.
	Expiry   time.Time
	MAC      string
	IP       string
	Hostname string
}

// ParseLeases parses a dnsmasq leases file. Malformed lines are skipped.
func ParseLeases(r io.Reader) ([]Lease, error) {
	var leases []Lease
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		// IPv6 leases have a "duid" header line and use a different layout.
		if len(f) < 4 || f[0] == "duid" {
			continue
		}
		expiry, e := strconv.ParseInt(f[0], 10, 64)
		if e != nil {
			continue
		}
		l := Lease{MAC: strings.ToLower(f[1]), IP: f[2]}
		if expiry > 0 {
			l.Expiry = time.Unix(expiry, 0)
		}
		if f[3] != "*" {
			l.Hostname = f[3]
		}
		leases = append(leases, l)
	}
	return leases, s.Err()
}

type Station struct {
	MAC              string
	Signal           int
	ConnectedSeconds int64
	InactiveMillis   int64
	RxBytes          uint64
	TxBytes          uint64
}

// ParseStationDump parses the output of `iw dev <iface> station dump`.
func ParseStationDump(r io.Reader) ([]Station, error) {
	var stations []Station
	var cur *Station
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := s.Text()
		if rest, ok := strings.CutPrefix(line, "Station "); ok {
			stations = append(stations, Station{MAC: strings.ToLower(strings.Fields(rest)[0])})
			cur = &stations[len(stations)-1]
			continue
		}
		if cur == nil {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		switch key {
		case "signal":
			cur.Signal, _ = strconv.Atoi(fields[0])
		case "connected time":
			cur.ConnectedSeconds, _ = strconv.ParseInt(fields[0], 10, 64)
		case "inactive time":
			cur.InactiveMillis, _ = strconv.ParseInt(fields[0], 10, 64)
		case "rx bytes":
			cur.RxBytes, _ = strconv.ParseUint(fields[0], 10, 64)
		case "tx bytes":
			cur.TxBytes, _ = strconv.ParseUint(fields[0], 10, 64)
		}
	}
	return stations, s.Err()
}

// ParseUptime parses /proc/uptime and returns the uptime in seconds.
func ParseUptime(data string) (float64, error) {
	f := strings.Fields(data)
	if len(f) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	return strconv.ParseFloat(f[0], 64)
}

// ParseLoadavg parses the 1, 5 and 15 minute averages from /proc/loadavg.
func ParseLoadavg(data string) ([3]float64, error) {
	var load [3]float64
	f := strings.Fields(data)
	if len(f) < 3 {
		return load, fmt.Errorf("short loadavg %q", data)
	}
	for i := range load {
		v, e := strconv.ParseFloat(f[i], 64)
		if e != nil {
			return load, e
		}
		load[i] = v
	}
	return load, nil
}

// ParseMeminfo returns MemTotal and MemAvailable from /proc/meminfo in bytes.
func ParseMeminfo(r io.Reader) (total, available uint64, err error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 {
			continue
		}
		v, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found")
	}
	return total, available, s.Err()
}

// ParseMilliCelsius parses a thermal zone temperature such as "48250".
func ParseMilliCelsius(data string) (float64, error) {
	v, e := strconv.ParseInt(strings.TrimSpace(data), 10, 64)
	if e != nil {
		return 0, e
	}
	return float64(v) / 1000, nil
}
