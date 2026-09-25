package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opiz3-router/internal/state"
)

func TestStatusEndpoint(t *testing.T) {
	store := state.NewStore()
	store.Update(func(s *state.Snapshot) { s.Modem.NetworkType = "LTE" })
	srv := httptest.NewServer(NewHandler(store, Options{}))
	defer srv.Close()

	res, e := http.Get(srv.URL + "/api/status")
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing security headers")
	}
	var snap state.Snapshot
	if e = json.NewDecoder(res.Body).Decode(&snap); e != nil {
		t.Fatal(e)
	}
	if snap.Modem.NetworkType != "LTE" {
		t.Fatalf("modem = %+v", snap.Modem)
	}
}

func TestIndexIsServed(t *testing.T) {
	srv := httptest.NewServer(NewHandler(state.NewStore(), Options{}))
	defer srv.Close()
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		res, e := http.Get(srv.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	srv := httptest.NewServer(NewHandler(state.NewStore(), Options{Username: "admin", Password: "secret"}))
	defer srv.Close()

	for _, tc := range []struct {
		user, pass string
		want       int
	}{
		{"", "", http.StatusUnauthorized},
		{"admin", "wrong", http.StatusUnauthorized},
		{"other", "secret", http.StatusUnauthorized},
		{"admin", "secret", http.StatusOK},
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
		if tc.user != "" {
			req.SetBasicAuth(tc.user, tc.pass)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s/%s: got %d, want %d", tc.user, tc.pass, res.StatusCode, tc.want)
		}
	}
}

func TestEventStream(t *testing.T) {
	store := state.NewStore()
	srv := httptest.NewServer(NewHandler(store, Options{MinEventInterval: time.Millisecond}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	events := make(chan state.Snapshot)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var s state.Snapshot
				if json.Unmarshal([]byte(data), &s) == nil {
					events <- s
				}
			}
		}
	}()

	// The first event is the current state, sent on connect.
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("no initial event")
	}
	store.Update(func(s *state.Snapshot) { s.System.Hostname = "opiz3" })
	for {
		select {
		case s := <-events:
			if s.System.Hostname == "opiz3" {
				return
			}
		case <-ctx.Done():
			t.Fatal("update was not streamed")
		}
	}
}

type fakeSwitcher struct {
	modes        []string
	kept, undone int
	err          error
}

func (f *fakeSwitcher) SetEthernetMode(m string) error { f.modes = append(f.modes, m); return f.err }
func (f *fakeSwitcher) KeepSwitch() error              { f.kept++; return f.err }
func (f *fakeSwitcher) UndoSwitch() error              { f.undone++; return f.err }

func post(t *testing.T, h http.Handler, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://172.16.0.1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNetworkActions(t *testing.T) {
	sw := &fakeSwitcher{}
	h := NewHandler(state.NewStore(), Options{Network: sw})

	if rec := post(t, h, "/api/network/ethernet-mode", `{"mode":"lan"}`, map[string]string{"Origin": "http://172.16.0.1", "Sec-Fetch-Site": "same-origin"}); rec.Code != http.StatusAccepted {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body)
	}
	post(t, h, "/api/network/keep", `{}`, nil)
	post(t, h, "/api/network/undo", `{}`, nil)
	if len(sw.modes) != 1 || sw.modes[0] != "lan" || sw.kept != 1 || sw.undone != 1 {
		t.Fatalf("switcher calls = %+v", sw)
	}

	sw.err = errors.New("keep or undo the last switch first")
	rec := post(t, h, "/api/network/ethernet-mode", `{"mode":"uplink"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "keep or undo") {
		t.Fatalf("error not passed on: %d %s", rec.Code, rec.Body)
	}
}

func TestNetworkActionsRefuseForeignRequests(t *testing.T) {
	sw := &fakeSwitcher{}
	h := NewHandler(state.NewStore(), Options{Network: sw})
	for name, header := range map[string]map[string]string{
		"form post":       {"Content-Type": "application/x-www-form-urlencoded"},
		"no content type": {"Content-Type": ""},
		"cross-site":      {"Sec-Fetch-Site": "cross-site"},
		"other origin":    {"Origin": "http://evil.example"},
		"dns rebinding":   {"Host": "evil.example"},
	} {
		req := httptest.NewRequest("POST", "http://172.16.0.1/api/network/ethernet-mode", strings.NewReader(`{"mode":"lan"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			if k == "Host" {
				req.Host = v
			} else if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	if len(sw.modes) != 0 {
		t.Fatalf("a refused request reached the switcher: %v", sw.modes)
	}
}

func TestNetworkActionsNeedSwitcher(t *testing.T) {
	h := NewHandler(state.NewStore(), Options{})
	if rec := post(t, h, "/api/network/keep", `{}`, nil); rec.Code == http.StatusAccepted {
		t.Fatal("action accepted without a switcher")
	}
}

type fakeDisplay struct{ pages []string }

func (f *fakeDisplay) SetDisplayPage(p string) error {
	if p != "status" && p != "channels" {
		return errors.New("unknown page")
	}
	f.pages = append(f.pages, p)
	return nil
}

func TestDisplayPageAction(t *testing.T) {
	d := &fakeDisplay{}
	h := NewHandler(state.NewStore(), Options{Display: d})
	if rec := post(t, h, "/api/display/page", `{"page":"channels"}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, "/api/display/page", `{"page":"nope"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("bad page: status %d", rec.Code)
	}
	if rec := post(t, h, "/api/display/page", `{"page":"status"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: status %d", rec.Code)
	}
	if len(d.pages) != 1 || d.pages[0] != "channels" {
		t.Fatalf("pages = %v", d.pages)
	}
}
