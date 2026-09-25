//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"opiz3-router/internal/config"
	"opiz3-router/internal/netplan"
	"opiz3-router/internal/state"
)

// switchConfirmWindow is how long a switch waits to be kept before it is
// undone, in case it cut off the device that made it.
const switchConfirmWindow = 90 * time.Second

var modeNames = map[netplan.Mode]string{netplan.Uplink: "internet uplink", netplan.LAN: "LAN port"}

// netSwitcher switches the Ethernet port between an internet uplink and a
// LAN port by rewriting the router's netplan file. Like `netplan try`, a
// switch is undone unless it is kept within confirmWindow.
type netSwitcher struct {
	cfg   config.Config
	store *state.Store

	// Replaceable in tests.
	confirmWindow time.Duration
	apply         func(context.Context) error
	currentMode   func() (netplan.Mode, error)
	netplanReady  func() error

	mu      sync.Mutex
	busy    bool
	pending *pendingSwitch
	lastErr string
}

type pendingSwitch struct {
	mode, previousMode netplan.Mode
	previous           []byte // the netplan file before the switch
	hadPrevious        bool
	deadline           time.Time
	timer              *time.Timer
}

func newNetSwitcher(cfg config.Config, store *state.Store) *netSwitcher {
	s := &netSwitcher{cfg: cfg, store: store, confirmWindow: switchConfirmWindow}
	s.apply = func(ctx context.Context) error { return applyNetplan(ctx, cfg) }
	s.currentMode = func() (netplan.Mode, error) { return ethernetMode(cfg) }
	s.netplanReady = func() error { return checkNetplanDir(cfg.Network.NetplanFile) }
	return s
}

// ethernetMode reads the Ethernet port's mode from the running system.
func ethernetMode(cfg config.Config) (netplan.Mode, error) {
	link, e := netlink.LinkByName(cfg.Interfaces.Ethernet)
	if e != nil {
		return "", e
	}
	if link.Attrs().MasterIndex != 0 {
		return netplan.LAN, nil
	}
	return netplan.Uplink, nil
}

// checkNetplanDir returns why the router cannot manage the netplan
// configuration, or nil if it can. The router's file must be the only one:
// netplan merges all files, so another file could undo the switch.
func checkNetplanDir(file string) error {
	if _, e := exec.LookPath("netplan"); e != nil {
		return errors.New("netplan is not installed")
	}
	return checkOtherNetplanFiles(file)
}

func checkOtherNetplanFiles(file string) error {
	dir := filepath.Dir(file)
	entries, e := os.ReadDir(dir)
	if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	var others []string
	for _, en := range entries {
		ext := filepath.Ext(en.Name())
		if !en.IsDir() && (ext == ".yaml" || ext == ".yml") && filepath.Join(dir, en.Name()) != file {
			others = append(others, en.Name())
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("other netplan files in %s would override the router's settings: %s. Move them out of %s, then write the current setup to %s (see the README)",
			dir, strings.Join(others, ", "), dir, filepath.Base(file))
	}
	return nil
}

// applyNetplan validates and applies the netplan configuration.
func applyNetplan(ctx context.Context, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, cmd := range []string{"generate", "apply"} {
		if out, e := exec.CommandContext(ctx, "netplan", cmd).CombinedOutput(); e != nil {
			return fmt.Errorf("netplan %s: %v: %s", cmd, e, strings.TrimSpace(string(out)))
		}
	}
	rebridgeWifi(cfg)
	return nil
}

// rebridgeWifi restarts hostapd if applying netplan took the Wi-Fi interface
// off the bridge; hostapd adds it back when it starts.
func rebridgeWifi(cfg config.Config) {
	time.Sleep(3 * time.Second)
	wifi, e := netlink.LinkByName(cfg.Interfaces.Wireless)
	if e != nil {
		return
	}
	br, e := netlink.LinkByName(cfg.Interfaces.Bridge)
	if e != nil || wifi.Attrs().MasterIndex == br.Attrs().Index {
		return
	}
	log.Printf("netmode: %s left %s; restarting hostapd", cfg.Interfaces.Wireless, cfg.Interfaces.Bridge)
	if out, e := exec.Command("systemctl", "restart", "hostapd").CombinedOutput(); e != nil {
		log.Printf("netmode: restart hostapd: %v: %s", e, out)
	}
}

// writeFileAtomic replaces path so that netplan never sees a partial file.
func writeFileAtomic(path string, data []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0o755); e != nil {
		return e
	}
	tmp := path + ".tmp"
	// netplan warns about files other users can read.
	if e := os.WriteFile(tmp, data, 0o600); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}

// SetEthernetMode starts switching the Ethernet port to mode. The change is
// applied in the background; its progress is published to the store.
func (s *netSwitcher) SetEthernetMode(mode string) error {
	m := netplan.Mode(mode)
	if !m.Valid() {
		return fmt.Errorf("unknown mode %q", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.busy:
		return errors.New("a change is being applied")
	case s.pending != nil:
		return errors.New("keep or undo the last switch first")
	}
	if e := s.netplanReady(); e != nil {
		return e
	}
	current, e := s.currentMode()
	if e != nil {
		return fmt.Errorf("cannot read the Ethernet port's mode: %w", e)
	}
	if current == m {
		return fmt.Errorf("the Ethernet port is already a %s", modeNames[m])
	}
	content, e := netplan.Render(m, s.cfg)
	if e != nil {
		return e
	}
	previous, e := os.ReadFile(s.cfg.Network.NetplanFile)
	hadPrevious := e == nil
	if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}

	s.busy, s.lastErr = true, ""
	s.publishLocked()
	log.Printf("netmode: switching %s to %s", s.cfg.Interfaces.Ethernet, modeNames[m])
	go func() {
		e := writeFileAtomic(s.cfg.Network.NetplanFile, content)
		if e == nil {
			e = s.apply(context.Background())
		}
		if e != nil {
			// Put the old configuration back so the router keeps working.
			if re := s.restore(previous, hadPrevious); re != nil {
				e = fmt.Errorf("%w; restoring the previous setup also failed: %v", e, re)
			}
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		s.busy = false
		if e != nil {
			s.fail("Switching the Ethernet port failed", e)
			return
		}
		p := &pendingSwitch{mode: m, previousMode: current, previous: previous, hadPrevious: hadPrevious, deadline: time.Now().Add(s.confirmWindow)}
		p.timer = time.AfterFunc(s.confirmWindow, func() { s.undo(p, true) })
		s.pending = p
		s.store.Notify(state.Warning, "network", fmt.Sprintf("Ethernet switched to %s; keep it within %d s or it is undone", modeNames[m], int(s.confirmWindow.Seconds())))
		s.publishLocked()
	}()
	return nil
}

// KeepSwitch makes the pending switch permanent.
func (s *netSwitcher) KeepSwitch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	if p == nil {
		return errors.New("there is no switch to keep")
	}
	p.timer.Stop()
	s.pending = nil
	log.Printf("netmode: kept %s as %s", s.cfg.Interfaces.Ethernet, modeNames[p.mode])
	s.store.Notify(state.Good, "network", "Kept Ethernet as "+modeNames[p.mode])
	s.publishLocked()
	return nil
}

// UndoSwitch undoes the pending switch now.
func (s *netSwitcher) UndoSwitch() error {
	s.mu.Lock()
	p := s.pending
	s.mu.Unlock()
	if p == nil {
		return errors.New("there is no switch to undo")
	}
	go s.undo(p, false)
	return nil
}

// undo restores the configuration from before switch p, unless p was kept
// or already undone.
func (s *netSwitcher) undo(p *pendingSwitch, auto bool) {
	s.mu.Lock()
	if s.pending != p || s.busy {
		s.mu.Unlock()
		return
	}
	p.timer.Stop()
	// busy keeps a new switch from starting while the old setup is applied.
	s.pending, s.busy = nil, true
	s.publishLocked()
	s.mu.Unlock()

	e := s.restore(p.previous, p.hadPrevious)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	if e != nil {
		s.fail("Undoing the Ethernet switch failed", e)
		return
	}
	msg := "Ethernet switch undone; it is a " + modeNames[p.previousMode] + " again"
	if auto {
		msg = "Ethernet switch was not kept in time and was undone; it is a " + modeNames[p.previousMode] + " again"
	}
	log.Printf("netmode: %s", msg)
	s.store.Notify(state.Warning, "network", msg)
	s.publishLocked()
}

// restore writes back the netplan file from before a switch and applies it.
func (s *netSwitcher) restore(previous []byte, hadPrevious bool) error {
	var e error
	if hadPrevious {
		e = writeFileAtomic(s.cfg.Network.NetplanFile, previous)
	} else if e = os.Remove(s.cfg.Network.NetplanFile); errors.Is(e, fs.ErrNotExist) {
		e = nil
	}
	if e != nil {
		return e
	}
	return s.apply(context.Background())
}

func (s *netSwitcher) fail(what string, e error) {
	log.Printf("netmode: %s: %v", what, e)
	s.lastErr = e.Error()
	s.store.Notify(state.Critical, "network", what)
	s.publishLocked()
}

// Publish refreshes the switching state in the store.
func (s *netSwitcher) Publish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked()
}

func (s *netSwitcher) publishLocked() {
	n := state.Network{Busy: s.busy, Error: s.lastErr, Switchable: true}
	if m, e := s.currentMode(); e == nil {
		n.EthernetMode = string(m)
	}
	if e := s.netplanReady(); e != nil {
		n.Switchable, n.Unavailable = false, e.Error()
	}
	if p := s.pending; p != nil {
		n.Pending = &state.PendingSwitch{Mode: string(p.mode), Previous: string(p.previousMode), Deadline: p.deadline}
	}
	s.store.Update(func(snap *state.Snapshot) { snap.Network = n })
}
