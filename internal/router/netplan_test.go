package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"opiz3-router/internal/config"
)

func TestRenderUplink(t *testing.T) {
	out, e := RenderNetplan(ModeUplink, config.Default())
	if e != nil {
		t.Fatal(e)
	}
	want := `network:
  version: 2
  renderer: networkd
  ethernets:
    "end0":
      optional: true
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
        route-metric: 100
      nameservers:
        addresses: ["1.1.1.1", "8.8.8.8"]
    "enxd236db4ff68a":
      optional: true
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
        route-metric: 101
      nameservers:
        addresses: ["1.1.1.1", "8.8.8.8"]
`
	if got := strings.SplitN(string(out), "\n", 3)[2]; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderLAN(t *testing.T) {
	out, e := RenderNetplan(ModeLAN, config.Default())
	if e != nil {
		t.Fatal(e)
	}
	s := string(out)
	for _, want := range []string{
		"    \"end0\": {}\n",
		"    \"enxd236db4ff68a\":\n      optional: true\n      dhcp4: true\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "route-metric: 100") {
		t.Errorf("end0 still an uplink:\n%s", s)
	}
	if strings.Contains(s, "bridges:") {
		t.Errorf("the bridge belongs to mini-net-man:\n%s", s)
	}
}

func TestRenderWithoutModem(t *testing.T) {
	cfg := config.Default()
	cfg.Interfaces.USBModem = ""
	out, e := RenderNetplan(ModeUplink, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(out), `""`) || strings.Count(string(out), "dhcp4: true") != 1 {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestRenderRejectsUnknownMode(t *testing.T) {
	if _, e := RenderNetplan("bridge", config.Default()); e == nil {
		t.Fatal("expected an error")
	}
}

func TestRenderPassesNetplanGenerate(t *testing.T) {
	gen := ""
	for _, p := range []string{"/usr/libexec/netplan/generate", "/usr/lib/netplan/generate"} {
		if _, e := os.Stat(p); e == nil {
			gen = p
		}
	}
	if gen == "" {
		t.Skip("netplan is not installed")
	}
	for mode, want := range map[Mode]string{ModeUplink: "DHCP=ipv4", ModeLAN: ""} {
		root := t.TempDir()
		out, e := RenderNetplan(mode, config.Default())
		if e != nil {
			t.Fatal(e)
		}
		dir := filepath.Join(root, "etc", "netplan")
		if e = os.MkdirAll(dir, 0o755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, "opiz3-router.yaml"), out, 0o600); e != nil {
			t.Fatal(e)
		}
		if msg, e := exec.Command(gen, "--root-dir", root).CombinedOutput(); e != nil {
			t.Fatalf("%s: netplan generate: %v\n%s", mode, e, msg)
		}
		network, e := os.ReadFile(filepath.Join(root, "run", "systemd", "network", "10-netplan-end0.network"))
		if want != "" && (e != nil || !strings.Contains(string(network), want)) {
			t.Fatalf("%s: end0 network file lacks %q: %s %v", mode, want, network, e)
		}
		// A LAN port gets no address from networkd; mini-net-man bridges it.
		if want == "" && strings.Contains(string(network), "DHCP") {
			t.Fatalf("%s: end0 still asks for an address: %s", mode, network)
		}
	}
}
