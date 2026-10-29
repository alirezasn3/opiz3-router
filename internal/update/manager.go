package update

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"opiz3-router/internal/state"
)

const (
	firstCheckDelay = 45 * time.Second
	checkInterval   = 12 * time.Hour
	// restartDelay lets the answer to the click that installed an update
	// reach the browser before the service restarts.
	restartDelay = 1500 * time.Millisecond
	maxNotes     = 2000
)

// Manager is the panel's view of updates: it looks for new releases, installs
// one when asked, and publishes how that is going.
type Manager struct {
	// Current is the running version, as stamped into the binary.
	Current string
	Repo    string
	// APIBase is GitHub's API URL, for tests; empty means GitHub.
	APIBase string
	Service string
	// Exe is the binary to replace; the running one when empty.
	Exe string
	// AutoCheck looks for a release now and then without being asked.
	AutoCheck bool

	store *state.Store

	// Replaceable in tests.
	restart      func(service string) error
	underSystemd func() bool
	restartAfter time.Duration

	mu        sync.Mutex
	latest    *Release
	checkedAt time.Time
	busy      string
	installed string
	errText   string
	notified  string
}

func NewManager(current, service string, autoCheck bool, store *state.Store) *Manager {
	return &Manager{
		Current: current, Service: service, AutoCheck: autoCheck, store: store,
		restart:      func(name string) error { return RestartService(name, true) },
		underSystemd: UnderSystemd,
		restartAfter: restartDelay,
	}
}

func (m *Manager) exe() (string, error) {
	if m.Exe != "" {
		return m.Exe, nil
	}
	return Executable()
}

// begin marks an operation as running, unless one already is.
func (m *Manager) begin(what string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy != "" {
		return fmt.Errorf("busy %s", m.busy)
	}
	if m.installed != "" {
		return errors.New("an update was installed; restart the service to use it first")
	}
	m.busy, m.errText = what, ""
	m.publishLocked()
	return nil
}

// end clears the running operation and records its error.
func (m *Manager) end(e error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = ""
	if e != nil {
		m.errText = e.Error()
	}
	m.publishLocked()
	return e
}

// Check asks GitHub for the latest release.
func (m *Manager) Check(ctx context.Context) error {
	if e := m.begin("checking"); e != nil {
		return e
	}
	rel, e := New(m.Repo, m.APIBase).Latest(ctx)
	m.mu.Lock()
	if e == nil {
		m.latest, m.checkedAt = rel, time.Now()
		m.notifyLocked()
	}
	m.mu.Unlock()
	return m.end(e)
}

// notifyLocked tells the log once about each new release.
func (m *Manager) notifyLocked() {
	if m.latest == nil || m.latest.Tag == m.notified {
		return
	}
	if newer, e := Newer(m.Current, m.latest.Tag); e == nil && newer {
		m.notified = m.latest.Tag
		m.store.Notify(state.Info, "update", fmt.Sprintf("Version %s is available", m.latest.Tag))
	}
}

// Install installs the latest release if it is newer, then restarts the
// service into it.
func (m *Manager) Install(ctx context.Context) error {
	if !Comparable(m.Current) {
		return fmt.Errorf("this is a local build (%s) with no release version; update it with: opiz3-router -update -force", m.Current)
	}
	if e := m.begin("installing"); e != nil {
		return e
	}
	return m.end(m.install(ctx))
}

func (m *Manager) install(ctx context.Context) error {
	exe, e := m.exe()
	if e != nil {
		return e
	}
	c := New(m.Repo, m.APIBase)
	rel, e := c.Latest(ctx)
	if e != nil {
		return e
	}
	m.mu.Lock()
	m.latest, m.checkedAt = rel, time.Now()
	m.mu.Unlock()
	if newer, e := Newer(m.Current, rel.Tag); e != nil {
		return e
	} else if !newer {
		return fmt.Errorf("already up to date (%s)", m.Current)
	}
	done, e := c.Install(ctx, rel, exe)
	if e != nil {
		return e
	}
	log.Printf("update: installed %s", done.Tag)
	m.store.Notify(state.Good, "update", "Installed "+done.Tag)
	m.afterSwap(done.Tag)
	return nil
}

// Rollback goes back to the previous version and restarts into it.
func (m *Manager) Rollback(context.Context) error {
	if e := m.begin("rolling back"); e != nil {
		return e
	}
	exe, e := m.exe()
	if e == nil {
		e = Rollback(exe)
	}
	if e == nil {
		log.Print("update: went back to the previous version")
		m.store.Notify(state.Warning, "update", "Went back to the previous version")
		m.afterSwap("the previous version")
	}
	return m.end(e)
}

// afterSwap records that a new binary is in place and restarts the service
// into it, a moment later so the panel gets its answer first. Without
// systemd there is no service to restart; the person has to.
func (m *Manager) afterSwap(what string) {
	m.mu.Lock()
	m.installed = what
	m.mu.Unlock()
	if !m.underSystemd() {
		m.store.Notify(state.Warning, "update", "Restart opiz3-router to use "+what)
		return
	}
	time.AfterFunc(m.restartAfter, func() {
		if e := m.restart(m.Service); e != nil {
			log.Printf("update: restart: %v", e)
			m.mu.Lock()
			m.errText = "Installed, but the service did not restart: " + e.Error()
			m.publishLocked()
			m.mu.Unlock()
		}
	})
}

// Run looks for a new release now and then, when AutoCheck is on, until ctx
// ends.
func (m *Manager) Run(ctx context.Context) {
	m.Publish()
	if !m.AutoCheck {
		return
	}
	wait := firstCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		if e := m.Check(cctx); e != nil {
			log.Printf("update: check: %v", e)
		}
		cancel()
		wait = checkInterval
	}
}

func (m *Manager) Publish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishLocked()
}

func (m *Manager) publishLocked() {
	u := state.Update{
		Current: m.Current, Comparable: Comparable(m.Current), CheckedAt: m.checkedAt,
		Busy: m.busy, Installed: m.installed, Error: m.errText,
	}
	if m.latest != nil {
		u.Latest, u.URL = m.latest.Tag, m.latest.URL
		u.Notes = strings.TrimSpace(m.latest.Notes)
		if len(u.Notes) > maxNotes {
			u.Notes = u.Notes[:maxNotes] + "…"
		}
		if newer, e := Newer(m.Current, m.latest.Tag); e == nil {
			u.Available = newer
		}
	}
	if exe, e := m.exe(); e == nil {
		if _, e := os.Stat(exe + ".old"); e == nil {
			u.CanRollback = true
		}
	}
	m.store.Update(func(s *state.Snapshot) { s.Update = u })
}
