package mnm

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func TestBuild(t *testing.T) {
	cfg := config.Default()
	d := Build(cfg, false)

	if b := d.Bridges[0]; b.Name != "br0" || len(b.Ports) != 0 {
		t.Errorf("uplink mode bridge = %+v", b)
	}
	if b := Build(cfg, true).Bridges[0]; !slices.Equal(b.Ports, []string{"end0"}) {
		t.Errorf("LAN mode bridge ports = %v", b.Ports)
	}
	if a := d.Addrs[0]; a.Address != "172.16.0.1/12" || a.Interface != "br0" {
		t.Errorf("address = %+v", a)
	}

	// Kill switch on: the rule always applies, and a blackhole route takes
	// over in the VPN table when the tunnel route is missing.
	if len(d.Routes) != 2 || d.Routes[0].Dev != "wg0" || d.Routes[1].Type != "blackhole" || d.Routes[1].Metric <= d.Routes[0].Metric {
		t.Errorf("routes = %+v", d.Routes)
	}
	if r := d.Rules[0]; r.Iif != "br0" || r.Table != "200" || r.Priority != 1000 || len(d.RuleChecks) != 0 {
		t.Errorf("rule = %+v checks %v", r, d.RuleChecks)
	}

	// Kill switch off: no blackhole, and the rule follows the health check.
	cfg.WireGuard.KillSwitch = false
	d = Build(cfg, false)
	if len(d.Routes) != 1 || d.RuleChecks[keyVPNRule] != keyVPNCheck {
		t.Errorf("without kill switch: routes %+v checks %v", d.Routes, d.RuleChecks)
	}
	if c := d.Checks[0]; c.Target != "1.1.1.1" || c.Interface != "wg0" {
		t.Errorf("check = %+v", c)
	}

	for _, r := range d.Firewall {
		if _, ok := keyOf(&r.Meta); !ok {
			t.Errorf("untagged firewall rule: %+v", r)
		}
	}
}

func TestBuildFirewall(t *testing.T) {
	var got []string
	for _, r := range Build(config.Default(), false).Firewall {
		got = append(got, strings.TrimPrefix(r.Comment, TagPrefix)+" "+r.Chain+" "+r.Action)
	}
	want := []string{
		"panel-80-br0 input accept", "panel-80-lo input accept", "panel-80-drop input drop",
		"panel-8080-br0 input accept", "panel-8080-lo input accept", "panel-8080-drop input drop",
		"forward-established forward accept", "forward-from-lan forward accept", "forward-to-lan forward drop",
		"masquerade-end0 srcnat masquerade", "masquerade-enxd236db4ff68a srcnat masquerade", "masquerade-wg0 srcnat masquerade",
		"clamp-mss mangle clamp-mss",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("firewall:\n got %q\nwant %q", got, want)
	}

	// Without the panel only mini-net-man's own port needs protecting.
	cfg := config.Default()
	cfg.Panel.Enabled = false
	for _, r := range Build(cfg, false).Firewall {
		if r.DstPort == "80" {
			t.Fatalf("rule for the disabled panel: %+v", r)
		}
	}
}

func TestApplySyncsOnceAndThenDoesNothing(t *testing.T) {
	f, client := newFakeAPI(t)
	s := NewSyncer(config.Default(), client, state.NewStore())

	ch, e := s.Apply(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	want := len(Build(config.Default(), false).Firewall) + 1 + 1 + 1 + 2 + 1 // firewall, bridge, address, check, routes, rule
	if ch.Created != want || ch.Updated != 0 || ch.Deleted != 0 {
		t.Fatalf("first sync = %+v, want %d created", ch, want)
	}
	if f.sysctl["ip_forward"] != true {
		t.Errorf("IP forwarding not enabled: %v", f.sysctl)
	}
	if f.tuned["tcp_congestion_control"] != "bbr" {
		t.Errorf("BBR not set: %v", f.tuned)
	}
	if _, set := f.tuned["default_qdisc"]; set {
		t.Errorf("fq set although the kernel does not offer it: %v", f.tuned)
	}
	if _, set := f.tuned["tcp_mtu_probing"]; set {
		t.Errorf("a setting the router does not care about was changed: %v", f.tuned)
	}

	f.resetWrites()
	if _, e = s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := f.writeCount(); n != 0 {
		t.Fatalf("a second sync made %d changes: %v", n, f.writes)
	}
}

func TestApplyLeavesHandMadeItemsAlone(t *testing.T) {
	f, client := newFakeAPI(t)
	f.add("firewall/rules", map[string]any{"chain": "input", "action": "accept", "proto": "tcp", "dst_port": "22", "comment": "ssh"})
	f.add("routes", map[string]any{"dst": "10.9.0.0/24", "dev": "wg1", "comment": ""})
	s := NewSyncer(config.Default(), client, state.NewStore())
	for range 2 {
		if _, e := s.Apply(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if got := f.comments("firewall/rules"); got[0] != "ssh" {
		t.Fatalf("hand-made rule moved or removed: %v", got)
	}
	if len(f.lists["routes"]) != 3 {
		t.Fatalf("routes = %v", f.lists["routes"])
	}
}

func TestApplyPutsRulesBackInOrder(t *testing.T) {
	f, client := newFakeAPI(t)
	s := NewSyncer(config.Default(), client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	want := f.comments("firewall/rules")

	// Someone deletes a rule in the middle of the chain in the panel.
	f.mu.Lock()
	rules := f.lists["firewall/rules"]
	f.lists["firewall/rules"] = append(append([]map[string]any{}, rules[:4]...), rules[5:]...)
	f.mu.Unlock()

	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if got := f.comments("firewall/rules"); !slices.Equal(got, want) {
		t.Fatalf("after repair:\n got %q\nwant %q", got, want)
	}
}

func TestApplyChangesWhatDiffers(t *testing.T) {
	f, client := newFakeAPI(t)
	cfg := config.Default()
	s := NewSyncer(cfg, client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}

	// Turning the kill switch off removes its route and gates the rule.
	cfg.WireGuard.KillSwitch = false
	s2 := NewSyncer(cfg, client, state.NewStore())
	f.resetWrites()
	ch, e := s2.Apply(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if ch.Deleted != 1 || ch.Updated != 1 || ch.Created != 0 {
		t.Fatalf("changes = %+v, writes %v", ch, f.writes)
	}
	if rule := f.lists["rules"][0]; rule["check"] == "" || rule["check"] != f.lists["checks"][0]["id"] {
		t.Fatalf("rule is not gated by the health check: %v", rule)
	}

	// An edited managed item is put back.
	f.mu.Lock()
	f.lists["addresses"][0]["address"] = "192.168.9.1/24"
	f.mu.Unlock()
	if ch, e = s2.Apply(context.Background()); e != nil || ch.Updated != 1 {
		t.Fatalf("repair = %+v, %v", ch, e)
	}
	if f.lists["addresses"][0]["address"] != "172.16.0.1/12" {
		t.Fatalf("address = %v", f.lists["addresses"][0])
	}
}

func TestSetLANMovesThePort(t *testing.T) {
	f, client := newFakeAPI(t)
	s := NewSyncer(config.Default(), client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := s.SetLAN(context.Background(), true); e != nil {
		t.Fatal(e)
	}
	if ports, _ := f.lists["bridges"][0]["ports"].([]any); len(ports) != 1 || ports[0] != "end0" {
		t.Fatalf("bridge = %v", f.lists["bridges"][0])
	}
	if e := s.SetLAN(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	if _, has := f.lists["bridges"][0]["ports"]; has {
		t.Fatalf("bridge = %v", f.lists["bridges"][0])
	}

	// A router that restarts reads the mode back instead of undoing it.
	if e := s.SetLAN(context.Background(), true); e != nil {
		t.Fatal(e)
	}
	f.resetWrites()
	s2 := NewSyncer(config.Default(), client, state.NewStore())
	if _, e := s2.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if f.writeCount() != 1 { // only the baseline sysctl write
		t.Fatalf("restart rewrote things: %v", f.writes)
	}
	if !s2.lan {
		t.Fatal("LAN mode forgotten after a restart")
	}
}

func TestSetLANFailureKeepsThePreviousMode(t *testing.T) {
	f, client := newFakeAPI(t)
	s := NewSyncer(config.Default(), client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.token = "changed"
	if e := s.SetLAN(context.Background(), true); e == nil {
		t.Fatal("expected an error")
	}
	if s.lan {
		t.Fatal("the syncer believes the port is a LAN port although the change failed")
	}
}

func TestPublishReportsState(t *testing.T) {
	f, client := newFakeAPI(t)
	store := state.NewStore()
	s := NewSyncer(config.Default(), client, store)
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	r := store.Snapshot().Routing
	if !r.Synced || r.VPN != "up" || !r.KillSwitch || r.Error != "" || r.SyncedAt.IsZero() {
		t.Fatalf("routing = %+v", r)
	}

	// Only issues of the router's own items are shown.
	f.mu.Lock()
	routeID := f.lists["routes"][0]["id"].(string)
	f.issues = []Issue{{Kind: "route", ID: routeID, Message: `no such interface "wg0"`}, {Kind: "route", ID: "other", Message: "not ours"}}
	f.checkState = "down"
	f.mu.Unlock()
	s.mu.Lock()
	s.publishLocked(context.Background())
	s.mu.Unlock()
	r = store.Snapshot().Routing
	if r.VPN != "down" || len(r.Issues) != 1 || !strings.Contains(r.Issues[0], "wg0") {
		t.Fatalf("routing = %+v", r)
	}
	var notes []string
	for _, n := range store.Snapshot().Notifications {
		notes = append(notes, n.Text)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "stopped answering") {
		t.Fatalf("notifications = %v", notes)
	}
}

func TestPublishWhenMiniNetManRefusesOrIsGone(t *testing.T) {
	f, client := newFakeAPI(t)
	store := state.NewStore()
	s := NewSyncer(config.Default(), client, store)
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.token = "revoked"
	_, e := s.Apply(context.Background())
	if !Unauthorized(e) {
		t.Fatalf("error = %v", e)
	}
	r := store.Snapshot().Routing
	if r.Synced || !strings.Contains(r.Error, "mini_net_man.token") {
		t.Fatalf("routing = %+v", r)
	}

	dead := NewSyncer(config.Default(), NewClient("http://127.0.0.1:1", "x"), store)
	if _, e = dead.Apply(context.Background()); e == nil {
		t.Fatal("expected an error")
	}
	if r = store.Snapshot().Routing; r.Synced || !strings.Contains(r.Error, "cannot reach mini-net-man") {
		t.Fatalf("routing = %+v", r)
	}
}

func TestHandsOffLeavesEditsAndAdditionsAlone(t *testing.T) {
	f, client := newFakeAPI(t)
	cfg := config.Default()
	s := NewSyncer(cfg, client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}

	s.SetEnforce(false)
	// The user edits one of the router's rules, deletes another, and adds
	// their own, in the middle of the chain.
	f.mu.Lock()
	rules := f.lists["firewall/rules"]
	rules[0]["dst_port"] = "2222"
	f.lists["firewall/rules"] = append(append([]map[string]any{}, rules[:3]...), rules[4:]...)
	f.lists["firewall/rules"] = append(f.lists["firewall/rules"], map[string]any{"id": "mine", "chain": "forward", "action": "accept", "comment": "port forward"})
	before := len(f.lists["firewall/rules"])
	f.mu.Unlock()
	f.resetWrites()

	for range 2 {
		if _, e := s.Apply(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if n := f.writeCount(); n != 0 {
		t.Fatalf("hands-off sync changed things: %v", f.writes)
	}
	if got := len(f.lists["firewall/rules"]); got != before || f.lists["firewall/rules"][0]["dst_port"] != "2222" {
		t.Fatalf("rules = %v", f.lists["firewall/rules"])
	}

	// Enforcing again puts the router's rules back and keeps the user's.
	s.SetEnforce(true)
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	got := f.comments("firewall/rules")
	if !slices.Contains(got, "port forward") || len(got) != len(Build(cfg, false).Firewall)+1 {
		t.Fatalf("after enforcing: %q", got)
	}
	if f.lists["firewall/rules"][slices.Index(got, "panel-80-br0")]["dst_port"] != "80" {
		t.Fatal("the edited rule was not put back")
	}
}

func TestHandsOffStillSetsUpAnEmptyMiniNetMan(t *testing.T) {
	f, client := newFakeAPI(t)
	cfg := config.Default()
	cfg.MiniNetMan.Enforce = false
	s := NewSyncer(cfg, client, state.NewStore())
	ch, e := s.Apply(context.Background())
	if e != nil || ch.Created == 0 {
		t.Fatalf("first sync = %+v, %v", ch, e)
	}
	if f.sysctl["ip_forward"] != true {
		t.Fatalf("forwarding not enabled on setup: %v", f.sysctl)
	}

	// A router that starts later leaves what is there alone.
	f.mu.Lock()
	f.lists["rules"][0]["priority"] = 5
	f.mu.Unlock()
	f.resetWrites()
	if _, e = NewSyncer(cfg, client, state.NewStore()).Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if f.writeCount() != 0 {
		t.Fatalf("restart changed things: %v", f.writes)
	}
}

func TestHandsOffKeepsReportingAndSwitchesThePort(t *testing.T) {
	f, client := newFakeAPI(t)
	cfg := config.Default()
	store := state.NewStore()
	s := NewSyncer(cfg, client, store)
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	s2 := NewSyncer(func() config.Config { c := cfg; c.MiniNetMan.Enforce = false; return c }(), client, store)
	f.resetWrites()
	if _, e := s2.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if r := store.Snapshot().Routing; !r.Synced || r.Enforce || r.VPN != "up" {
		t.Fatalf("routing = %+v", r)
	}

	// An issue with one of the router's items is still reported.
	f.mu.Lock()
	f.issues = []Issue{{Kind: "route", ID: f.lists["routes"][0]["id"].(string), Message: "no such interface"}}
	f.mu.Unlock()
	s2.mu.Lock()
	s2.publishLocked(context.Background())
	s2.mu.Unlock()
	if r := store.Snapshot().Routing; len(r.Issues) != 1 {
		t.Fatalf("issues = %v", r.Issues)
	}

	// The port moves in and out of the bridge, and nothing else changes.
	if e := s2.SetLAN(context.Background(), true); e != nil {
		t.Fatal(e)
	}
	if f.writeCount() != 1 || f.writes[0] != "PUT /api/bridges/"+f.lists["bridges"][0]["id"].(string) {
		t.Fatalf("writes = %v", f.writes)
	}
	if ports, _ := f.lists["bridges"][0]["ports"].([]any); len(ports) != 1 {
		t.Fatalf("bridge = %v", f.lists["bridges"][0])
	}
}

func TestRestorePutsTheSetupBackWhileHandsOff(t *testing.T) {
	f, client := newFakeAPI(t)
	cfg := config.Default()
	cfg.MiniNetMan.Enforce = false
	s := NewSyncer(cfg, client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.lists["addresses"][0]["address"] = "192.168.9.1/24"
	f.mu.Unlock()
	if ch, e := s.Apply(context.Background()); e != nil || ch.Total() != 0 {
		t.Fatalf("hands-off sync = %+v, %v", ch, e)
	}
	ch, e := s.Restore(context.Background())
	if e != nil || ch.Updated != 1 {
		t.Fatalf("restore = %+v, %v", ch, e)
	}
	if f.lists["addresses"][0]["address"] != "172.16.0.1/12" {
		t.Fatalf("address = %v", f.lists["addresses"][0])
	}
	if s.Enforcing() {
		t.Fatal("restoring turned enforcing on")
	}
}

func TestCopiesOfManagedItemsAreLeftAlone(t *testing.T) {
	f, client := newFakeAPI(t)
	s := NewSyncer(config.Default(), client, state.NewStore())
	if _, e := s.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	// Cloning a rule in the panel copies its comment too.
	f.mu.Lock()
	clone := map[string]any{}
	for k, v := range f.lists["firewall/rules"][2] {
		clone[k] = v
	}
	clone["id"], clone["dst_port"] = "clone", "2222"
	f.lists["firewall/rules"] = append(f.lists["firewall/rules"], clone)
	f.mu.Unlock()
	f.resetWrites()
	for range 2 {
		if _, e := s.Apply(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if f.writeCount() != 0 {
		t.Fatalf("the router touched a copy: %v", f.writes)
	}
}

func TestRulesControllerSavesTheSetting(t *testing.T) {
	_, client := newFakeAPI(t)
	cfg := config.Default()
	path := filepath.Join(t.TempDir(), "config.json")
	if e := cfg.Save(path); e != nil {
		t.Fatal(e)
	}
	s := NewSyncer(cfg, client, state.NewStore())
	r := NewRules(s, path)
	if e := r.SetEnforce(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	saved, e := config.Load(path)
	if e != nil || saved.MiniNetMan.Enforce || s.Enforcing() {
		t.Fatalf("saved %v, enforcing %v, err %v", saved.MiniNetMan.Enforce, s.Enforcing(), e)
	}
	if e = r.SetEnforce(context.Background(), true); e != nil || !s.Enforcing() {
		t.Fatalf("enforcing %v, err %v", s.Enforcing(), e)
	}
	if e = r.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
}
