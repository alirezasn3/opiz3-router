package mnm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

const (
	statusInterval = 5 * time.Second
	// resyncInterval is how often the whole config is compared with
	// mini-net-man's, in case someone edited the managed items in its panel.
	resyncInterval = 30 * time.Second
)

// tuning is kept by mini-net-man at these values. They are only applied when
// the kernel offers them.
var tuning = map[string]string{
	"tcp_congestion_control": "bbr",
	"default_qdisc":          "fq",
}

// Syncer keeps mini-net-man's config equal to what the router needs and
// publishes how that is going.
type Syncer struct {
	cfg    config.Config
	client *Client
	store  *state.Store

	mu sync.Mutex
	// enforce: put back whatever differs from the router's setup. When off,
	// the setup is only made once, on a mini-net-man that has none of it.
	enforce    bool
	seeded     bool // the check for an existing setup has been made since enforce was turned off
	lan        bool
	lanKnown   bool
	baseline   bool // sysctl and tuning have been set since mini-net-man was last unreachable
	ids        map[string]bool
	checkID    string
	lastSync   time.Time
	syncErr    error
	seen       bool
	reachable  bool
	vpnState   string
	kick       chan struct{}
	lastIssues string
}

func NewSyncer(cfg config.Config, client *Client, store *state.Store) *Syncer {
	return &Syncer{cfg: cfg, client: client, store: store, enforce: cfg.MiniNetMan.Enforce, kick: make(chan struct{}, 1)}
}

// Enforcing reports whether the router puts mini-net-man's rules back.
func (s *Syncer) Enforcing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enforce
}

// SetEnforce turns enforcing on or off. Turning it on makes the next sync put
// everything back. Turning it off leaves the rules as they are.
func (s *Syncer) SetEnforce(on bool) {
	s.mu.Lock()
	s.enforce, s.seeded = on, false
	s.mu.Unlock()
	s.Kick()
}

// Restore puts mini-net-man's rules back to the router's setup once, whether
// or not it is enforcing.
func (s *Syncer) Restore(ctx context.Context) (Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx, true)
}

// Apply makes mini-net-man's config match the router's. It is safe to call
// often: nothing is written when nothing differs.
func (s *Syncer) Apply(ctx context.Context) (Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx, false)
}

// applyLocked syncs mini-net-man. A full sync makes every managed item equal
// to the router's; without it only what the router needs to know is read.
func (s *Syncer) applyLocked(ctx context.Context, restore bool) (ch Changes, err error) {
	defer func() {
		s.syncErr = err
		if err == nil {
			s.lastSync = time.Now()
		}
		s.publishLocked(ctx)
	}()

	if !s.lanKnown {
		var bridges []Bridge
		if e := s.client.Do(ctx, "GET", "/api/bridges", nil, &bridges); e != nil {
			return ch, fmt.Errorf("list bridges: %w", e)
		}
		for _, b := range bridges {
			if k, ok := keyOf(&b.Meta); ok && k == keyBridge {
				s.lan = slices.Contains(b.Ports, s.cfg.Interfaces.Ethernet)
			}
		}
		s.lanKnown = true
	}

	full := s.enforce || restore
	if !full && !s.seeded {
		// Hands off, unless this mini-net-man has none of the router's
		// setup yet: it is made once, so the router works out of the box.
		bridges, e := listOurs[Bridge](ctx, s.client, "/api/bridges")
		if e != nil {
			return ch, e
		}
		s.seeded = true
		full = len(bridges) == 0
	}
	if !full {
		return ch, s.discover(ctx)
	}
	if !s.baseline {
		if e := s.client.Do(ctx, "PUT", "/api/sysctl", map[string]any{"ip_forward": true}, nil); e != nil {
			return ch, fmt.Errorf("enable ip forwarding: %w", e)
		}
		s.tune(ctx)
		s.baseline = true
	}

	d := Build(s.cfg, s.lan)
	ids := map[string]bool{}
	collect := func(m map[string]string, c Changes, e error) (map[string]string, error) {
		ch.add(c)
		for _, id := range m {
			ids[id] = true
		}
		return m, e
	}
	if _, e := collect(reconcile(ctx, s.client, "/api/bridges", d.Bridges, false)); e != nil {
		return ch, e
	}
	if _, e := collect(reconcile(ctx, s.client, "/api/addresses", d.Addrs, false)); e != nil {
		return ch, e
	}
	checkIDs, e := collect(reconcile(ctx, s.client, "/api/checks", d.Checks, false))
	if e != nil {
		return ch, e
	}
	if _, e := collect(reconcile(ctx, s.client, "/api/routes", d.Routes, false)); e != nil {
		return ch, e
	}
	for i := range d.Rules {
		if ck := d.RuleChecks[keyOfMeta(d.Rules[i].Meta)]; ck != "" {
			d.Rules[i].Check = checkIDs[ck]
		}
	}
	if _, e := collect(reconcile(ctx, s.client, "/api/rules", d.Rules, false)); e != nil {
		return ch, e
	}
	if _, e := collect(reconcile(ctx, s.client, "/api/firewall/rules", d.Firewall, true)); e != nil {
		return ch, e
	}
	s.ids, s.checkID = ids, checkIDs[keyVPNCheck]
	if ch.Total() > 0 {
		log.Printf("mini-net-man: synced (%d created, %d updated, %d deleted)", ch.Created, ch.Updated, ch.Deleted)
	}
	return ch, nil
}

// discover learns which items are the router's, for reporting only.
func (s *Syncer) discover(ctx context.Context) error {
	ids := map[string]bool{}
	checkID := ""
	add := func(m map[string]string, e error) error {
		for k, id := range m {
			ids[id] = true
			if k == keyVPNCheck {
				checkID = id
			}
		}
		return e
	}
	for _, f := range []func() (map[string]string, error){
		func() (map[string]string, error) { return listOurs[Bridge](ctx, s.client, "/api/bridges") },
		func() (map[string]string, error) { return listOurs[Address](ctx, s.client, "/api/addresses") },
		func() (map[string]string, error) { return listOurs[Check](ctx, s.client, "/api/checks") },
		func() (map[string]string, error) { return listOurs[Route](ctx, s.client, "/api/routes") },
		func() (map[string]string, error) { return listOurs[Rule](ctx, s.client, "/api/rules") },
		func() (map[string]string, error) { return listOurs[FirewallRule](ctx, s.client, "/api/firewall/rules") },
	} {
		if e := add(f()); e != nil {
			return e
		}
	}
	s.ids, s.checkID = ids, checkID
	return nil
}

func keyOfMeta(m Meta) string {
	k, _ := keyOf(&m)
	return k
}

// tune sets the kernel tuning in mini-net-man. A setting the kernel lacks is
// skipped; it is a nicety, not a requirement.
func (s *Syncer) tune(ctx context.Context) {
	var items []struct {
		ID        string   `json:"id"`
		Supported bool     `json:"supported"`
		Managed   string   `json:"managed"`
		Available []string `json:"available"`
	}
	if e := s.client.Do(ctx, "GET", "/api/tuning", nil, &items); e != nil {
		log.Printf("mini-net-man: read tuning: %v", e)
		return
	}
	for _, it := range items {
		want, ok := tuning[it.ID]
		if !ok || !it.Supported || it.Managed == want {
			continue
		}
		if len(it.Available) > 0 && !slices.Contains(it.Available, want) {
			log.Printf("mini-net-man: kernel does not offer %s=%s; skipping", it.ID, want)
			continue
		}
		if e := s.client.Do(ctx, "PUT", "/api/tuning/"+it.ID, map[string]string{"value": want}, nil); e != nil {
			log.Printf("mini-net-man: set %s: %v", it.ID, e)
		}
	}
}

// SetLAN makes the Ethernet port a bridge port (true) or an uplink (false).
// When not enforcing, only the bridge's ports change; the rest is left alone.
func (s *Syncer) SetLAN(ctx context.Context, lan bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, prevKnown := s.lan, s.lanKnown
	s.lan, s.lanKnown = lan, true
	var e error
	if s.enforce {
		_, e = s.applyLocked(ctx, false)
	} else {
		d := Build(s.cfg, lan)
		_, _, e = reconcile(ctx, s.client, "/api/bridges", d.Bridges, false)
		s.publishLocked(ctx)
	}
	if e != nil {
		s.lan, s.lanKnown = prev, prevKnown
	}
	return e
}

// Kick asks the Run loop to sync soon.
func (s *Syncer) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run syncs and publishes status until ctx ends. mini-net-man may start after
// the router, so failures are retried every few seconds.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	force := true
	for {
		s.mu.Lock()
		if force || s.syncErr != nil || time.Since(s.lastSync) >= resyncInterval {
			_, _ = s.applyLocked(ctx, false)
		} else {
			s.publishLocked(ctx)
		}
		s.mu.Unlock()
		force = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
			force = true
		}
	}
}

// publishLocked reads mini-net-man's view and writes it into the state store.
func (s *Syncer) publishLocked(ctx context.Context) {
	r := state.Routing{Enforce: s.enforce, KillSwitch: s.cfg.WireGuard.KillSwitch, SyncedAt: s.lastSync, VPN: "unknown"}
	var statuses map[string]CheckStatus
	var overview struct {
		Issues []Issue `json:"issues"`
	}
	e := s.client.Do(ctx, "GET", "/api/overview", nil, &overview)
	if e == nil && s.checkID != "" {
		e = s.client.Do(ctx, "GET", "/api/checks/status", nil, &statuses)
	}
	reachable := e == nil
	switch {
	case e != nil:
		r.Error = describe(e)
		// mini-net-man may have restarted with an old config; set the
		// kernel settings again once it answers.
		s.baseline = false
	case s.syncErr != nil:
		r.Error = describe(s.syncErr)
	default:
		r.Synced = true
	}
	for _, is := range overview.Issues {
		if s.ids[is.ID] {
			r.Issues = append(r.Issues, is.String())
		}
	}
	if st, ok := statuses[s.checkID]; ok && st.State != "" {
		r.VPN = st.State
	}

	if s.seen && reachable != s.reachable {
		if reachable {
			s.store.Notify(state.Good, "router", "Reconnected to mini-net-man")
		} else {
			s.store.Notify(state.Critical, "router", "Lost contact with mini-net-man")
		}
	}
	s.seen, s.reachable = true, reachable
	if r.VPN != "unknown" {
		if s.vpnState != "" && r.VPN != s.vpnState {
			if r.VPN == "down" {
				s.store.Notify(state.Warning, "vpn", "VPN tunnel stopped answering")
			} else {
				s.store.Notify(state.Good, "vpn", "VPN tunnel is answering again")
			}
		}
		s.vpnState = r.VPN
	}
	if text := fmt.Sprint(r.Issues); text != s.lastIssues {
		s.lastIssues = text
		if len(r.Issues) > 0 {
			log.Printf("mini-net-man: could not apply: %v", r.Issues)
		}
	}
	s.store.Update(func(snap *state.Snapshot) { snap.Routing = r })
}

func describe(e error) string {
	var api *APIError
	switch {
	case errors.As(e, &api) && api.Status == 401:
		return "mini-net-man refused the API token (check mini_net_man.token)"
	case errors.As(e, &api):
		return e.Error()
	}
	return "cannot reach mini-net-man: " + e.Error()
}
