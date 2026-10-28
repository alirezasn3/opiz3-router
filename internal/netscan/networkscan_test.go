package netscan

import (
	"net"
	"testing"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func TestMacOUI(t *testing.T) {
	for _, test := range []struct {
		mac, want string
	}{
		{"", ""},
		{"00:1B:C5:12:34:56", "001BC5123456"},
		{"00-1B-C5-12-34-56", "001BC5123456"},
		{"12:34", ""},
	} {
		if got := macOUI(test.mac); got != test.want {
			t.Errorf("macOUI(%q) = %q, want %q", test.mac, got, test.want)
		}
	}
}

func TestManufacturerLookup(t *testing.T) {
	ouis := loadOUIDatabase()
	if len(ouis) == 0 {
		t.Fatal("empty OUI database")
	}
	// Raspberry Pi OUI: B8:27:EB
	mac := "B8:27:EB:00:11:22"
	vendor := manufacturer(ouis, mac)
	if vendor == "" {
		t.Errorf("expected vendor for Raspberry Pi OUI %s, got empty", mac)
	}
}

func TestSubnetHosts(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.168.1.0/29")
	if err != nil {
		t.Fatal(err)
	}
	hosts := subnetHosts(subnet)
	// /29 has 8 addresses: .0 network, .7 broadcast, 6 usable hosts (.1 to .6)
	if len(hosts) != 6 {
		t.Fatalf("expected 6 usable hosts for /29, got %d", len(hosts))
	}
	if hosts[0].String() != "192.168.1.1" || hosts[len(hosts)-1].String() != "192.168.1.6" {
		t.Errorf("first=%s, last=%s", hosts[0], hosts[len(hosts)-1])
	}
}

func TestReverseName(t *testing.T) {
	ip := net.ParseIP("192.168.1.42")
	if got, want := reverseName(ip), "42.1.168.192.in-addr.arpa"; got != want {
		t.Errorf("reverseName() = %q, want %q", got, want)
	}
}

func TestScannerTrigger(t *testing.T) {
	cfg := config.Default()
	store := state.NewStore()
	scanner := NewScanner(cfg, store)
	if err := scanner.TriggerScan(); err != nil {
		t.Fatalf("TriggerScan failed: %v", err)
	}
}
