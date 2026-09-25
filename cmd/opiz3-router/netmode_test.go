//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/netplan"
	"opiz3-router/internal/state"
)

// fakeSystem stands in for netplan and the live network: applying reads the
// router's netplan file and sets the Ethernet mode it describes.
type fakeSystem struct {
	mu      sync.Mutex
	file    string
	mode    netplan.Mode
	applied int
	fail    bool // fail the next apply
}

func (f *fakeSystem) apply(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied++
	if f.fail {
		f.fail = false
		return errors.New("netplan apply: boom")
	}
	data, e := os.ReadFile(f.file)
	switch {
	case errors.Is(e, os.ErrNotExist):
		f.mode = netplan.Uplink // the image's default setup
	case e != nil:
		return e
	case strings.Contains(string(data), "interfaces:"):
		f.mode = netplan.LAN
	default:
		f.mode = netplan.Uplink
	}
	return nil
}

func (f *fakeSystem) currentMode() (netplan.Mode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode, nil
}

func newTestSwitcher(t *testing.T, window time.Duration) (*netSwitcher, *fakeSystem, *state.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Network.NetplanFile = filepath.Join(t.TempDir(), "netplan", "opiz3-router.yaml")
	store := state.NewStore()
	fake := &fakeSystem{file: cfg.Network.NetplanFile, mode: netplan.Uplink}
	s := newNetSwitcher(cfg, store)
	s.confirmWindow = window
	s.apply = fake.apply
	s.currentMode = fake.currentMode
	s.netplanReady = func() error { return checkOtherNetplanFiles(cfg.Network.NetplanFile) }
	return s, fake, store
}

// waitFor polls the published network state until ok returns true.
func waitFor(t *testing.T, store *state.Store, what string, ok func(state.Network) bool) state.Network {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := store.Snapshot().Network
		if ok(n) {
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; state = %+v", what, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSwitchAndKeep(t *testing.T) {
	s, fake, store := newTestSwitcher(t, time.Hour)
	if e := s.SetEthernetMode("lan"); e != nil {
		t.Fatal(e)
	}
	if e := s.SetEthernetMode("uplink"); e == nil {
		t.Fatal("a second switch started while the first was in progress")
	}
	n := waitFor(t, store, "pending switch", func(n state.Network) bool { return n.Pending != nil })
	if n.EthernetMode != "lan" || n.Pending.Mode != "lan" || n.Pending.Previous != "uplink" || n.Busy || !n.Switchable {
		t.Fatalf("state = %+v", n)
	}
	if data, _ := os.ReadFile(fake.file); !strings.Contains(string(data), `interfaces: ["end0"]`) {
		t.Fatalf("netplan file = %s", data)
	}
	if info, _ := os.Stat(fake.file); info.Mode().Perm() != 0o600 {
		t.Fatalf("netplan file mode = %v", info.Mode().Perm())
	}
	if e := s.SetEthernetMode("uplink"); e == nil {
		t.Fatal("a new switch started before the last one was kept")
	}
	if e := s.KeepSwitch(); e != nil {
		t.Fatal(e)
	}
	n = store.Snapshot().Network
	if n.Pending != nil || n.EthernetMode != "lan" {
		t.Fatalf("after keep: %+v", n)
	}
	if e := s.KeepSwitch(); e == nil {
		t.Fatal("kept a switch twice")
	}
	if e := s.SetEthernetMode("lan"); e == nil || !strings.Contains(e.Error(), "already") {
		t.Fatalf("switching to the current mode: %v", e)
	}
}

func TestSwitchIsUndoneWhenNotKept(t *testing.T) {
	s, fake, store := newTestSwitcher(t, 50*time.Millisecond)
	if e := s.SetEthernetMode("lan"); e != nil {
		t.Fatal(e)
	}
	waitFor(t, store, "pending switch", func(n state.Network) bool { return n.Pending != nil })
	n := waitFor(t, store, "automatic undo", func(n state.Network) bool { return n.Pending == nil && !n.Busy })
	if n.EthernetMode != "uplink" {
		t.Fatalf("mode after undo = %q", n.EthernetMode)
	}
	// There was no file before, so undoing removes it again.
	if _, e := os.Stat(fake.file); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("netplan file still there: %v", e)
	}
	snap := store.Snapshot()
	last, _ := snap.LatestNotification()
	if !strings.Contains(last.Text, "not kept in time") {
		t.Fatalf("last notification = %+v", last)
	}
}

func TestUndoRestoresPreviousFile(t *testing.T) {
	s, fake, store := newTestSwitcher(t, time.Hour)
	previous := []byte("# the user's setup\n")
	if e := writeFileAtomic(fake.file, previous); e != nil {
		t.Fatal(e)
	}
	if e := s.SetEthernetMode("lan"); e != nil {
		t.Fatal(e)
	}
	waitFor(t, store, "pending switch", func(n state.Network) bool { return n.Pending != nil })
	if e := s.UndoSwitch(); e != nil {
		t.Fatal(e)
	}
	waitFor(t, store, "undo", func(n state.Network) bool { return n.Pending == nil && !n.Busy })
	if data, _ := os.ReadFile(fake.file); string(data) != string(previous) {
		t.Fatalf("file after undo = %q", data)
	}
}

func TestFailedSwitchRestoresPreviousSetup(t *testing.T) {
	s, fake, store := newTestSwitcher(t, time.Hour)
	fake.fail = true
	if e := s.SetEthernetMode("lan"); e != nil {
		t.Fatal(e)
	}
	n := waitFor(t, store, "failure", func(n state.Network) bool { return n.Error != "" })
	if n.Pending != nil || n.Busy || n.EthernetMode != "uplink" || !strings.Contains(n.Error, "boom") {
		t.Fatalf("state = %+v", n)
	}
	if _, e := os.Stat(fake.file); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("failed switch left its file: %v", e)
	}
	if fake.applied != 2 {
		t.Fatalf("applied %d times, want the switch and the restore", fake.applied)
	}
}

func TestSwitchRefusedWithOtherNetplanFiles(t *testing.T) {
	s, fake, store := newTestSwitcher(t, time.Hour)
	if e := writeFileAtomic(filepath.Join(filepath.Dir(fake.file), "01-netcfg.yaml"), []byte("network: {}\n")); e != nil {
		t.Fatal(e)
	}
	e := s.SetEthernetMode("lan")
	if e == nil || !strings.Contains(e.Error(), "01-netcfg.yaml") {
		t.Fatalf("error = %v", e)
	}
	s.Publish()
	if n := store.Snapshot().Network; n.Switchable || !strings.Contains(n.Unavailable, "01-netcfg.yaml") {
		t.Fatalf("state = %+v", n)
	}
	if fake.applied != 0 {
		t.Fatal("applied despite the other file")
	}
}
