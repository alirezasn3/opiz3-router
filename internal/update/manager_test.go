package update

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"opiz3-router/internal/state"
)

type managerTest struct {
	*Manager
	gh        *fakeGitHub
	store     *state.Store
	exe       string
	mu        sync.Mutex
	restarted []string
	restartCh chan struct{}
}

func newManagerTest(t *testing.T, current string, tags ...string) *managerTest {
	t.Helper()
	bins := map[string][]byte{}
	for _, tag := range tags {
		bins[tag] = script(tag)
	}
	mt := &managerTest{gh: newFakeGitHub(t, bins), store: state.NewStore(), exe: installedExe(t, current), restartCh: make(chan struct{}, 4)}
	m := NewManager(current, "opiz3-router", false, mt.store)
	m.Repo, m.APIBase, m.Exe = "owner/name", mt.gh.URL, mt.exe
	m.underSystemd = func() bool { return true }
	m.restartAfter = time.Millisecond
	m.restart = func(name string) error {
		mt.mu.Lock()
		mt.restarted = append(mt.restarted, name)
		mt.mu.Unlock()
		mt.restartCh <- struct{}{}
		return nil
	}
	mt.Manager = m
	return mt
}

func (mt *managerTest) update() state.Update { return mt.store.Snapshot().Update }

func (mt *managerTest) waitRestart(t *testing.T) {
	t.Helper()
	select {
	case <-mt.restartCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the service was not restarted")
	}
}

func TestManagerCheckPublishesTheLatestRelease(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.0.0", "v1.1.0")
	mt.Publish()
	if u := mt.update(); u.Current != "v1.0.0" || u.Latest != "" || u.Available || !u.Comparable {
		t.Fatalf("before a check: %+v", u)
	}
	if e := mt.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	u := mt.update()
	if u.Latest != "v1.1.0" || !u.Available || u.Busy != "" || u.CheckedAt.IsZero() || !strings.Contains(u.Notes, "notes for v1.1.0") || u.URL == "" || u.Error != "" {
		t.Fatalf("after a check: %+v", u)
	}
	if u.CanRollback {
		t.Fatal("rollback offered before any update")
	}
	// The log hears of a release once.
	if e := mt.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	var notes []string
	for _, n := range mt.store.Snapshot().Notifications {
		notes = append(notes, n.Text)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "v1.1.0 is available") {
		t.Fatalf("notifications = %v", notes)
	}
}

func TestManagerCheckFailureIsShown(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0")
	if e := mt.Check(context.Background()); e == nil {
		t.Fatal("expected an error")
	}
	if u := mt.update(); u.Error == "" || u.Busy != "" {
		t.Fatalf("state = %+v", u)
	}
}

func TestManagerInstallSwapsAndRestarts(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.0.0", "v1.1.0")
	if e := mt.Install(context.Background()); e != nil {
		t.Fatal(e)
	}
	if version(t, mt.exe) != "v1.1.0" {
		t.Fatalf("version = %q", version(t, mt.exe))
	}
	u := mt.update()
	if u.Installed != "v1.1.0" || !u.CanRollback || u.Busy != "" || u.Error != "" {
		t.Fatalf("state = %+v", u)
	}
	mt.waitRestart(t)
	if len(mt.restarted) != 1 || mt.restarted[0] != "opiz3-router" {
		t.Fatalf("restarted = %v", mt.restarted)
	}
	// Nothing more is done until the service has restarted into it.
	if e := mt.Install(context.Background()); e == nil || !strings.Contains(e.Error(), "restart") {
		t.Fatalf("a second install: %v", e)
	}
}

func TestManagerInstallRefusals(t *testing.T) {
	mt := newManagerTest(t, "v1.1.0", "v1.0.0", "v1.1.0")
	if e := mt.Install(context.Background()); e == nil || !strings.Contains(e.Error(), "up to date") {
		t.Fatalf("up to date: %v", e)
	}
	if u := mt.update(); !strings.Contains(u.Error, "up to date") || u.Busy != "" {
		t.Fatalf("state = %+v", u)
	}
	if version(t, mt.exe) != "v1.1.0" || len(mt.restarted) != 0 {
		t.Fatal("something was changed")
	}

	dev := newManagerTest(t, "v1.0.0-2-gabc1234", "v1.1.0")
	if e := dev.Install(context.Background()); e == nil || !strings.Contains(e.Error(), "local build") {
		t.Fatalf("local build: %v", e)
	}
	if u := dev.update(); u.Comparable || u.Available {
		t.Fatalf("a local build compared with a release: %+v", u)
	}
	if e := dev.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	if u := dev.update(); u.Latest != "v1.1.0" || u.Available || u.Comparable {
		t.Fatalf("local build after a check: %+v", u)
	}
}

func TestManagerWithoutSystemdLeavesTheRestartToYou(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.1.0")
	mt.underSystemd = func() bool { return false }
	if e := mt.Install(context.Background()); e != nil {
		t.Fatal(e)
	}
	time.Sleep(20 * time.Millisecond)
	if len(mt.restarted) != 0 {
		t.Fatal("restarted although not run by systemd")
	}
	if u := mt.update(); u.Installed != "v1.1.0" {
		t.Fatalf("state = %+v", u)
	}
	snap := mt.store.Snapshot()
	last, _ := snap.LatestNotification()
	if !strings.Contains(last.Text, "Restart opiz3-router") {
		t.Fatalf("notification = %+v", last)
	}
}

func TestManagerRestartFailureIsShown(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.1.0")
	failed := make(chan struct{})
	mt.restart = func(string) error { defer close(failed); return errors.New("no systemctl") }
	if e := mt.Install(context.Background()); e != nil {
		t.Fatal(e)
	}
	<-failed
	deadline := time.Now().Add(2 * time.Second)
	for mt.update().Error == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if u := mt.update(); !strings.Contains(u.Error, "did not restart") {
		t.Fatalf("state = %+v", u)
	}
}

func TestManagerRollback(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.1.0")
	if e := mt.Rollback(context.Background()); e == nil || !strings.Contains(e.Error(), "no previous version") {
		t.Fatalf("rollback without a backup: %v", e)
	}
	if e := mt.Install(context.Background()); e != nil {
		t.Fatal(e)
	}
	mt.waitRestart(t)
	// A fresh process (the restarted service) can go back.
	mt2 := NewManager("v1.1.0", "opiz3-router", false, mt.store)
	mt2.Exe, mt2.underSystemd, mt2.restartAfter, mt2.restart = mt.exe, mt.underSystemd, time.Millisecond, mt.restart
	mt2.Publish()
	if !mt.update().CanRollback {
		t.Fatalf("rollback not offered: %+v", mt.update())
	}
	if e := mt2.Rollback(context.Background()); e != nil {
		t.Fatal(e)
	}
	if version(t, mt.exe) != "v1.0.0" {
		t.Fatalf("version = %q", version(t, mt.exe))
	}
	mt.waitRestart(t)
}

func TestManagerOneThingAtATime(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.1.0")
	mt.mu.Lock()
	mt.Manager.busy = "checking"
	mt.mu.Unlock()
	for name, e := range map[string]error{
		"check":    mt.Check(context.Background()),
		"install":  mt.Install(context.Background()),
		"rollback": mt.Rollback(context.Background()),
	} {
		if e == nil || !strings.Contains(e.Error(), "busy") {
			t.Errorf("%s while busy: %v", name, e)
		}
	}
}

func TestManagerAutoCheck(t *testing.T) {
	mt := newManagerTest(t, "v1.0.0", "v1.1.0")
	mt.AutoCheck = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mt.Run(ctx); close(done) }()
	// The first check waits for the network to come up after boot.
	time.Sleep(50 * time.Millisecond)
	if mt.update().Latest != "" {
		t.Fatal("checked at once")
	}
	if mt.update().Current != "v1.0.0" {
		t.Fatalf("not published: %+v", mt.update())
	}
	cancel()
	<-done

	off := newManagerTest(t, "v1.0.0", "v1.1.0")
	off.Run(context.Background()) // returns at once when auto check is off
	if off.update().Current != "v1.0.0" {
		t.Fatal("not published")
	}
}
