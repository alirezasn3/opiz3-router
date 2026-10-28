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

	mu         sync.Mutex
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
	return &Syncer{cfg: cfg, client: client, store: store, kick: make(chan struct{}, 1)}
}

// Apply makes mini-net-man's config match the router's. It is safe to call
// often: nothing is written when nothing differs.
func (s *Syncer) Apply(ctx context.Context) (Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx)
}

func (s *Syncer) applyLocked(ctx context.Context) (ch Changes, err error) {
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
func (s *Syncer) SetLAN(ctx context.Context, lan bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, prevKnown := s.lan, s.lanKnown
	s.lan, s.lanKnown = lan, true
	if _, e := s.applyLocked(ctx); e != nil {
		s.lan, s.lanKnown = prev, prevKnown
		return e
	}
	return nil
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
			_, _ = s.applyLocked(ctx)
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
	r := state.Routing{KillSwitch: s.cfg.WireGuard.KillSwitch, SyncedAt: s.lastSync, VPN: "unknown"}
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
