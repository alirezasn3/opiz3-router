package mnm

import (
	"context"
	"os"
	"testing"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

// liveClient connects to a real mini-net-man (for example `mini-net-man run
// -mock`) named by MNM_URL and MNM_TOKEN, and skips the test without one.
func liveClient(t *testing.T) *Client {
	t.Helper()
	url, token := os.Getenv("MNM_URL"), os.Getenv("MNM_TOKEN")
	if url == "" || token == "" {
		t.Skip("set MNM_URL and MNM_TOKEN to test against a running mini-net-man")
	}
	return NewClient(url, token)
}

func TestSyncAgainstMiniNetMan(t *testing.T) {
	client := liveClient(t)
	ctx := context.Background()
	wipe(t, client)
	t.Cleanup(func() { wipe(t, client) })
	cfg := config.Default()
	store := state.NewStore()
	s := NewSyncer(cfg, client, store)

	ch, e := s.Apply(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if ch.Created == 0 {
		t.Fatalf("first sync created nothing: %+v", ch)
	}

	// A second sync must not write anything, or every sync would make
	// mini-net-man re-apply its kernel config.
	ch, e = s.Apply(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if ch.Total() != 0 {
		t.Fatalf("second sync changed %+v", ch)
	}

	// Switching the Ethernet port to a LAN port only touches the bridge.
	if e = s.SetLAN(ctx, true); e != nil {
		t.Fatal(e)
	}
	var bridges []Bridge
	if e = client.Do(ctx, "GET", "/api/bridges", nil, &bridges); e != nil {
		t.Fatal(e)
	}
	if len(bridges) == 0 || len(bridges[0].Ports) != 1 || bridges[0].Ports[0] != cfg.Interfaces.Ethernet {
		t.Fatalf("bridge ports = %+v", bridges)
	}

	// A new syncer reads the mode back from mini-net-man.
	s2 := NewSyncer(cfg, client, state.NewStore())
	if ch, e = s2.Apply(ctx); e != nil || ch.Total() != 0 {
		t.Fatalf("restart sync: %+v, %v", ch, e)
	}
	if !s2.lan {
		t.Fatal("LAN mode was not read back from mini-net-man")
	}
	if e = s2.SetLAN(ctx, false); e != nil {
		t.Fatal(e)
	}

	// Without the kill switch the rule is gated by the health check.
	cfg.WireGuard.KillSwitch = false
	s3 := NewSyncer(cfg, client, store)
	if _, e = s3.Apply(ctx); e != nil {
		t.Fatal(e)
	}
	var rules []Rule
	if e = client.Do(ctx, "GET", "/api/rules", nil, &rules); e != nil {
		t.Fatal(e)
	}
	if len(rules) != 1 || rules[0].Check == "" {
		t.Fatalf("rules = %+v", rules)
	}
	var routes []Route
	if e = client.Do(ctx, "GET", "/api/routes", nil, &routes); e != nil {
		t.Fatal(e)
	}
	if len(routes) != 1 {
		t.Fatalf("kill switch route should be gone: %+v", routes)
	}

	// Someone deleting a managed rule in the panel is repaired.
	var fw []FirewallRule
	if e = client.Do(ctx, "GET", "/api/firewall/rules", nil, &fw); e != nil {
		t.Fatal(e)
	}
	if e = client.Do(ctx, "DELETE", "/api/firewall/rules/"+fw[1].ID, nil, nil); e != nil {
		t.Fatal(e)
	}
	if ch, e = s3.Apply(ctx); e != nil || ch.Created == 0 {
		t.Fatalf("repair: %+v, %v", ch, e)
	}
	var after []FirewallRule
	if e = client.Do(ctx, "GET", "/api/firewall/rules", nil, &after); e != nil {
		t.Fatal(e)
	}
	if len(after) != len(fw) {
		t.Fatalf("%d rules after repair, want %d", len(after), len(fw))
	}
	for i := range fw {
		if after[i].Comment != fw[i].Comment {
			t.Fatalf("rule %d is %q after repair, want %q", i, after[i].Comment, fw[i].Comment)
		}
	}
	if snap := store.Snapshot(); !snap.Routing.Synced {
		t.Fatalf("routing state = %+v", snap.Routing)
	}
}

// wipe removes every item the router manages, so a test starts from nothing.
func wipe(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	for _, path := range []string{"/api/firewall/rules", "/api/rules", "/api/routes", "/api/checks", "/api/addresses", "/api/bridges"} {
		var items []struct {
			Meta
		}
		if e := c.Do(ctx, "GET", path, nil, &items); e != nil {
			t.Fatal(e)
		}
		for _, it := range items {
			if _, ok := keyOf(&it.Meta); ok {
				if e := c.Do(ctx, "DELETE", path+"/"+it.ID, nil, nil); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}
