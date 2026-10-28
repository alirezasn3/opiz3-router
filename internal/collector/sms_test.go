package collector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	dwr910m "github.com/alirezasn3/dlink-dwr-910m-api"
	"opiz3-router/internal/state"
)

type fakeModem struct {
	mu       sync.Mutex
	msgs     []dwr910m.Message
	pageSize int
}

func (f *fakeModem) set(msgs ...dwr910m.Message) {
	f.mu.Lock()
	f.msgs = msgs
	f.mu.Unlock()
}

func (f *fakeModem) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	page, _ := strconv.Atoi(r.URL.Query().Get("pageIndex"))
	pages := (len(f.msgs) + f.pageSize - 1) / f.pageSize
	start := min((page-1)*f.pageSize, len(f.msgs))
	end := min(start+f.pageSize, len(f.msgs))
	_ = json.NewEncoder(w).Encode(dwr910m.Messages{CurrentPage: int64(page), TotalPages: int64(pages), TotalRecords: int64(len(f.msgs)), Messages: f.msgs[start:end]})
}

func sms(id int64, sender, date, text string) dwr910m.Message {
	return dwr910m.Message{ID: id, Sender: sender, Date: date, Content: text}
}

func TestSMSCollectorNotifiesOnlyNewMessages(t *testing.T) {
	fake := &fakeModem{pageSize: 2}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := state.NewStore()
	c := newSMSCollector(dwr910m.New(srv.URL, 2*time.Second))
	old := []dwr910m.Message{
		sms(0, "Irancell", "2026-09-20 09:00:00", "first"),
		sms(1, "Irancell", "2026-09-22 10:00:00", "second"),
		sms(2, "Bank", "2026-09-24 10:59:00", "third"),
	}

	fake.set(old...)
	c.collect(store)
	snap := store.Snapshot()
	if !snap.SMS.Available || len(snap.SMS.Messages) != 3 || len(snap.Notifications) != 0 {
		t.Fatalf("after first read: sms=%+v notifications=%+v", snap.SMS, snap.Notifications)
	}
	if want := time.Date(2026, 9, 24, 10, 59, 0, 0, time.Local); !snap.SMS.Messages[0].Time.Equal(want) {
		t.Fatalf("newest time = %v, want %v (modem local time)", snap.SMS.Messages[0].Time, want)
	}

	fake.set(
		sms(5, "Irancell", "2026-09-20 09:00:00", "first"),
		sms(6, "Irancell", "2026-09-22 10:00:00", "second"),
		sms(7, "Bank", "2026-09-24 10:59:00", "third"),
		sms(0, "+989121234567", "2026-09-25 12:00:00", "new"),
	)
	c.collect(store)
	snap = store.Snapshot()
	if len(snap.SMS.Messages) != 4 || snap.SMS.Messages[0].Text != "new" {
		t.Fatalf("inbox not newest first: %+v", snap.SMS.Messages)
	}
	n, ok := snap.LatestNotification()
	if !ok || len(snap.Notifications) != 1 || n.Source != "sms" || n.Text != "SMS from +989121234567" {
		t.Fatalf("notifications = %+v", snap.Notifications)
	}

	c.collect(store)
	if got := len(store.Snapshot().Notifications); got != 1 {
		t.Fatalf("notifications = %d after an unchanged read", got)
	}

	fake.set(append(old, sms(9, "Odd", "yesterday", "no date"))...)
	c.collect(store)
	if snap = store.Snapshot(); snap.SMS.Error != "" || len(snap.SMS.Messages) != 4 {
		t.Fatalf("bad date broke the read: %+v", snap.SMS)
	}

	srv.Close()
	c.collect(store)
	snap = store.Snapshot()
	if snap.SMS.Error == "" || len(snap.SMS.Messages) != 4 {
		t.Fatalf("after modem loss: %+v", snap.SMS)
	}
}

func TestCleanModemText(t *testing.T) {
	for in, want := range map[string]string{
		"LTE":                   "LTE",
		"  LTE\n\t":             "LTE",
		"-71\t dBm":             "-71 dBm",
		"-71&nbsp;dBm":          "-71 dBm",
		"3 days\n    04:12:55 ": "3 days 04:12:55",
		" ":                     "",
	} {
		if got := cleanModemText(in); got != want {
			t.Errorf("cleanModemText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewSMSIsListedFirst(t *testing.T) {
	fake := &fakeModem{pageSize: 10}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	store := state.NewStore()
	c := newSMSCollector(dwr910m.New(srv.URL, 2*time.Second))

	var old []dwr910m.Message
	for i := range smsLimit + 20 {
		old = append(old, sms(int64(i+1), "Irancell", fmt.Sprintf("25/09/2026 10:%02d", i%60), fmt.Sprintf("old %d", i)))
	}
	fake.set(old...)
	c.collect(store)

	fake.set(append(old, sms(999, "VIP", "25/09/2026 11:00", "urgent"))...)
	c.collect(store)
	snap := store.Snapshot()
	if len(snap.SMS.Messages) != smsLimit {
		t.Fatalf("len = %d, want %d", len(snap.SMS.Messages), smsLimit)
	}
	if snap.SMS.Messages[0].Text != "urgent" {
		t.Fatalf("newest message not on top: %+v", snap.SMS.Messages[0])
	}
}
