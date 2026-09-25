package sysinfo

import (
	"strings"
	"testing"
	"time"
)

func TestParseLeases(t *testing.T) {
	in := `1758800000 AA:BB:CC:DD:EE:01 172.16.0.23 phone 01:aa:bb:cc:dd:ee:01
0 aa:bb:cc:dd:ee:02 172.16.0.24 * *
garbage
duid 00:01:00:01:2c:1f
`
	leases, e := ParseLeases(strings.NewReader(in))
	if e != nil {
		t.Fatal(e)
	}
	if len(leases) != 2 {
		t.Fatalf("got %d leases", len(leases))
	}
	if l := leases[0]; l.MAC != "aa:bb:cc:dd:ee:01" || l.IP != "172.16.0.23" || l.Hostname != "phone" || !l.Expiry.Equal(time.Unix(1758800000, 0)) {
		t.Fatalf("lease 0 = %+v", l)
	}
	if l := leases[1]; l.Hostname != "" || !l.Expiry.IsZero() {
		t.Fatalf("lease 1 = %+v", l)
	}
}

func TestParseStationDump(t *testing.T) {
	in := `Station 12:34:56:78:9A:BC (on wlan0)
	inactive time:	304 ms
	rx bytes:	18816
	rx packets:	173
	tx bytes:	5695
	signal:  	-42 [-42] dBm
	signal avg:	-43 [-43] dBm
	connected time:	83 seconds
Station 12:34:56:78:9a:bd (on wlan0)
	signal:  	-70 dBm
`
	st, e := ParseStationDump(strings.NewReader(in))
	if e != nil {
		t.Fatal(e)
	}
	if len(st) != 2 {
		t.Fatalf("got %d stations", len(st))
	}
	want := Station{MAC: "12:34:56:78:9a:bc", Signal: -42, ConnectedSeconds: 83, InactiveMillis: 304, RxBytes: 18816, TxBytes: 5695}
	if st[0] != want {
		t.Fatalf("station 0 = %+v", st[0])
	}
	if st[1].Signal != -70 {
		t.Fatalf("station 1 = %+v", st[1])
	}
}

func TestParseProcFiles(t *testing.T) {
	if up, e := ParseUptime("12345.67 40000.00\n"); e != nil || up != 12345.67 {
		t.Fatalf("uptime = %v, %v", up, e)
	}
	if load, e := ParseLoadavg("0.15 0.10 0.05 1/123 4567\n"); e != nil || load != [3]float64{0.15, 0.10, 0.05} {
		t.Fatalf("load = %v, %v", load, e)
	}
	total, avail, e := ParseMeminfo(strings.NewReader("MemTotal:        1000 kB\nMemFree:  10 kB\nMemAvailable:     500 kB\n"))
	if e != nil || total != 1024000 || avail != 512000 {
		t.Fatalf("meminfo = %d %d %v", total, avail, e)
	}
	if c, e := ParseMilliCelsius("48250\n"); e != nil || c != 48.25 {
		t.Fatalf("temp = %v, %v", c, e)
	}
	if _, e := ParseLoadavg("1.0"); e == nil {
		t.Fatal("expected error for short loadavg")
	}
}
