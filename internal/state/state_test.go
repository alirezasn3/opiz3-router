package state

import (
	"sync"
	"testing"
	"time"
)

func TestSnapshotIsACopy(t *testing.T) {
	s := NewStore()
	s.Update(func(snap *Snapshot) {
		snap.Clients = []Client{{MAC: "aa"}}
		snap.Interfaces = []Interface{{Name: "br0", Addresses: []string{"172.16.0.1/12"}}}
	})
	c := s.Snapshot()
	c.Clients[0].MAC = "bb"
	c.Interfaces[0].Addresses[0] = "x"
	again := s.Snapshot()
	if again.Clients[0].MAC != "aa" || again.Interfaces[0].Addresses[0] != "172.16.0.1/12" {
		t.Fatal("snapshot shares memory with the store")
	}
	if again.WireGuard.Peers == nil || again.Traffic == nil {
		t.Fatal("empty slices should not be nil")
	}
}

func TestSubscribeCoalescesAndUnsubscribes(t *testing.T) {
	s := NewStore()
	ch, cancel := s.Subscribe()
	for range 5 {
		s.Update(func(*Snapshot) {})
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	select {
	case <-ch:
		t.Fatal("notifications were not coalesced")
	default:
	}
	cancel()
	s.Update(func(*Snapshot) {})
	select {
	case <-ch:
		t.Fatal("notified after unsubscribe")
	default:
	}
}

func TestAddTrafficKeepsLatest(t *testing.T) {
	var snap Snapshot
	for i := range HistoryLength + 10 {
		snap.AddTraffic(TrafficSample{RxRate: float64(i)})
	}
	if len(snap.Traffic) != HistoryLength {
		t.Fatalf("len = %d", len(snap.Traffic))
	}
	if snap.Traffic[0].RxRate != 10 || snap.Traffic[HistoryLength-1].RxRate != HistoryLength+9 {
		t.Fatalf("wrong window: first %v last %v", snap.Traffic[0].RxRate, snap.Traffic[HistoryLength-1].RxRate)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 100 {
				s.Update(func(snap *Snapshot) { snap.AddTraffic(TrafficSample{RxRate: float64(i)}) })
				_ = s.Snapshot()
			}
		})
	}
	wg.Wait()
	if n := len(s.Snapshot().Traffic); n != HistoryLength {
		t.Fatalf("len = %d", n)
	}
}

func TestNotifyKeepsLatest(t *testing.T) {
	s := NewStore()
	empty := s.Snapshot()
	if _, ok := empty.LatestNotification(); ok {
		t.Fatal("empty log has a latest notification")
	}
	for i := range NotificationLimit + 5 {
		s.Notify(Info, "test", string(rune('a'+i%26)))
	}
	snap := s.Snapshot()
	if len(snap.Notifications) != NotificationLimit {
		t.Fatalf("len = %d", len(snap.Notifications))
	}
	last, _ := snap.LatestNotification()
	if want := string(rune('a' + (NotificationLimit+4)%26)); last.Text != want || last.Source != "test" || last.Time.IsZero() {
		t.Fatalf("latest = %+v, want text %q", last, want)
	}
	snap.Notifications[0].Text = "changed"
	if s.Snapshot().Notifications[0].Text == "changed" {
		t.Fatal("snapshot shares notifications with the store")
	}
}

func TestOnNotification(t *testing.T) {
	s := NewStore()
	var received []Notification
	s.OnNotification(func(n Notification) {
		received = append(received, n)
	})
	s.Notify(Good, "test", "hello")
	if len(received) != 1 || received[0].Text != "hello" || received[0].Level != Good {
		t.Fatalf("received = %+v", received)
	}
}
