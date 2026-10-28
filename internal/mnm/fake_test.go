package mnm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is an in-memory stand-in for the parts of mini-net-man's API the
// router uses. Items are stored as raw JSON objects, so it behaves like the
// real server about unknown and missing fields.
type fakeAPI struct {
	mu     sync.Mutex
	lists  map[string][]map[string]any
	nextID int
	writes []string // "METHOD path" of every change
	token  string
	sysctl map[string]any
	tuned  map[string]string
	// checkState is reported for every health check.
	checkState string
	issues     []Issue
}

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{
		lists: map[string][]map[string]any{}, token: "mnm_test", checkState: "up",
		tuned: map[string]string{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL, f.token)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid API token"}`)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	if r.Body != nil && r.Method != "GET" {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if r.Method != "GET" {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}

	switch {
	case path == "me":
		reply(map[string]string{"user": "token:test"})
	case path == "overview":
		reply(map[string]any{"issues": f.issues})
	case path == "checks/status":
		out := map[string]CheckStatus{}
		for _, c := range f.lists["checks"] {
			out[c["id"].(string)] = CheckStatus{State: f.checkState}
		}
		reply(out)
	case path == "sysctl" && r.Method == "PUT":
		f.sysctl = body
		reply(map[string]any{"issues": []Issue{}})
	case path == "tuning" && r.Method == "GET":
		reply([]map[string]any{
			{"id": "tcp_congestion_control", "supported": true, "managed": f.tuned["tcp_congestion_control"], "available": []string{"reno", "cubic", "bbr"}},
			{"id": "default_qdisc", "supported": true, "managed": f.tuned["default_qdisc"], "available": []string{"fq_codel"}},
			{"id": "tcp_mtu_probing", "supported": true},
		})
	case strings.HasPrefix(path, "tuning/") && r.Method == "PUT":
		f.tuned[strings.TrimPrefix(path, "tuning/")] = body["value"].(string)
		reply(map[string]any{"issues": []Issue{}})
	default:
		f.serveList(w, r, path, body)
	}
}

func (f *fakeAPI) serveList(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	list, id, _ := strings.Cut(path, "/")
	if strings.HasPrefix(path, "firewall/rules") {
		list, id = "firewall/rules", strings.TrimPrefix(strings.TrimPrefix(path, "firewall/rules"), "/")
	}
	items := f.lists[list]
	index := func() int {
		for i, it := range items {
			if it["id"] == id {
				return i
			}
		}
		return -1
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && id == "":
		if items == nil {
			items = []map[string]any{}
		}
		reply(items)
	case r.Method == "POST":
		f.nextID++
		body["id"] = fmt.Sprintf("%08x", f.nextID)
		f.lists[list] = append(items, body)
		reply(map[string]any{"id": body["id"], "issues": []Issue{}})
	case r.Method == "PUT" && index() >= 0:
		body["id"] = id
		items[index()] = body
		reply(map[string]any{"issues": []Issue{}})
	case r.Method == "DELETE" && index() >= 0:
		f.lists[list] = append(items[:index()], items[index()+1:]...)
		reply(map[string]any{"issues": []Issue{}})
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such item"}`)
	}
}

func (f *fakeAPI) resetWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = nil
}

func (f *fakeAPI) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// comments lists the comments of a list, in order.
func (f *fakeAPI) comments(list string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, it := range f.lists[list] {
		out = append(out, strings.TrimPrefix(it["comment"].(string), TagPrefix))
	}
	return out
}

func (f *fakeAPI) add(list string, item map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "ff" + strconv.Itoa(f.nextID)
	item["id"] = id
	f.lists[list] = append(f.lists[list], item)
	return id
}
