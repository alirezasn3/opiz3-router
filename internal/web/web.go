// Package web serves the router's status panel: an embedded single page app, a
// JSON status endpoint and a Server-Sent Events stream of status updates.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"opiz3-router/internal/state"
)

//go:embed static
var static embed.FS

type Options struct {
	// Username and Password enable HTTP basic auth when Password is set.
	Username string
	Password string
	// MinEventInterval limits how often a stream sends updates.
	MinEventInterval time.Duration
	// KeepAliveInterval is how often an idle stream sends a comment so
	// proxies and browsers keep the connection open.
	KeepAliveInterval time.Duration
	// Network, when set, lets the panel switch the Ethernet port's mode.
	Network NetworkSwitcher
	// Display, when set, lets the panel choose what the display shows.
	Display DisplayController
}

// DisplayController chooses the display's page.
type DisplayController interface {
	SetDisplayPage(page string) error
}

// NetworkSwitcher changes the router's network setup. Errors are shown to
// the user as they are.
type NetworkSwitcher interface {
	SetEthernetMode(mode string) error
	KeepSwitch() error
	UndoSwitch() error
}

func NewHandler(store *state.Store, opts Options) http.Handler {
	if opts.MinEventInterval <= 0 {
		opts.MinEventInterval = time.Second
	}
	if opts.KeepAliveInterval <= 0 {
		opts.KeepAliveInterval = 15 * time.Second
	}
	files, e := fs.Sub(static, "static")
	if e != nil {
		panic(e)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if e := json.NewEncoder(w).Encode(store.Snapshot()); e != nil {
			log.Printf("web: encode status: %v", e)
		}
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		streamEvents(w, r, store, opts)
	})
	if n := opts.Network; n != nil {
		mux.Handle("POST /api/network/ethernet-mode", action(func(r *http.Request) error {
			var body struct {
				Mode string `json:"mode"`
			}
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				return fmt.Errorf("invalid request: %w", e)
			}
			return n.SetEthernetMode(body.Mode)
		}))
		mux.Handle("POST /api/network/keep", action(func(*http.Request) error { return n.KeepSwitch() }))
		mux.Handle("POST /api/network/undo", action(func(*http.Request) error { return n.UndoSwitch() }))
	}
	if d := opts.Display; d != nil {
		mux.Handle("POST /api/display/page", action(func(r *http.Request) error {
			var body struct {
				Page string `json:"page"`
			}
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				return fmt.Errorf("invalid request: %w", e)
			}
			return d.SetDisplayPage(body.Page)
		}))
	}

	var h http.Handler = mux
	if opts.Password != "" {
		h = basicAuth(h, opts.Username, opts.Password)
	}
	return securityHeaders(h)
}

func streamEvents(w http.ResponseWriter, r *http.Request, store *state.Store, opts Options) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	updates, cancel := store.Subscribe()
	defer cancel()

	send := func() error {
		data, e := json.Marshal(store.Snapshot())
		if e != nil {
			return e
		}
		if _, e = fmt.Fprintf(w, "event: status\ndata: %s\n\n", data); e != nil {
			return e
		}
		return rc.Flush()
	}
	if send() != nil {
		return
	}

	keepAlive := time.NewTicker(opts.KeepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, e := fmt.Fprint(w, ": keep-alive\n\n"); e != nil || rc.Flush() != nil {
				return
			}
		case <-updates:
			if send() != nil {
				return
			}
			// Collectors update several times a second; coalesce them so a
			// slow client gets at most one update per interval.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(opts.MinEventInterval):
			}
		}
	}
}

// action wraps a state-changing request. It only accepts JSON requests from
// the panel itself, so another web page open in the same browser cannot
// trigger it (cross-site request forgery or DNS rebinding).
func action(do func(*http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := sameOrigin(r); e != nil {
			writeError(w, http.StatusForbidden, e)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if e := do(r); e != nil {
			writeError(w, http.StatusConflict, e)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

func sameOrigin(r *http.Request) error {
	// Browsers only send a JSON content type cross-origin after a CORS
	// preflight, which this server never approves.
	if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
		return errors.New("expected a JSON request")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return errors.New("cross-site request refused")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host != r.Host {
			return errors.New("cross-origin request refused")
		}
	}
	// A page that points its own domain name at the router (DNS rebinding)
	// looks same-origin, but its requests carry that domain as Host.
	host := r.Host
	if h, _, e := net.SplitHostPort(r.Host); e == nil {
		host = h
	}
	if net.ParseIP(host) == nil && host != "localhost" {
		return errors.New("open the panel by its IP address to make changes")
	}
	return nil
}

func writeError(w http.ResponseWriter, code int, e error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}

func basicAuth(next http.Handler, username, password string) http.Handler {
	wantUser := sha256.Sum256([]byte(username))
	wantPass := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		// Compare hashes so the comparison time does not depend on length.
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="opiz3-router", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe serves h on addr until ctx is cancelled. Binding is retried
// because the listen address usually belongs to the bridge, which may not be
// configured yet when the router starts.
func ListenAndServe(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: the event stream is a long-lived response. Request
		// contexts derive from ctx so open streams end on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	var ln net.Listener
	for {
		var e error
		ln, e = net.Listen("tcp", addr)
		if e == nil {
			break
		}
		log.Printf("web: listen on %s: %v (retrying)", addr, e)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("web: panel listening on http://%s", addr)
	if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	return nil
}
