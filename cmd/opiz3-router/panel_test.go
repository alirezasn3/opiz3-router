//go:build linux

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"opiz3-router/internal/config"
	"opiz3-router/internal/mnm"
	"opiz3-router/internal/state"
	"opiz3-router/internal/web"
)

type noopControls struct{}

func (noopControls) SetEthernetMode(string) error     { return nil }
func (noopControls) KeepSwitch() error                { return nil }
func (noopControls) UndoSwitch() error                { return nil }
func (noopControls) SetDisplayPower(bool) error       { return nil }
func (noopControls) SetWakeOnNotification(bool) error { return nil }
func (noopControls) TriggerScan() error               { return nil }
func (noopControls) Check(context.Context) error      { return nil }
func (noopControls) Install(context.Context) error    { return nil }
func (noopControls) Rollback(context.Context) error   { return nil }

func testPanel(t *testing.T) (web.Options, http.Handler) {
	t.Helper()
	cfg := config.Default()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if e := cfg.Save(cfgPath); e != nil {
		t.Fatal(e)
	}
	store := state.NewStore()
	// The syncer is never started; it only has to exist.
	syncer := mnm.NewSyncer(cfg, mnm.NewClient("http://127.0.0.1:1", "x"), store)
	n := noopControls{}
	opts := panelOptions(cfg, cfgPath, panelControls{Network: n, Display: n, Scanner: n, Syncer: syncer, Updater: n})
	return opts, web.NewHandler(store, opts)
}

// Every control the panel can have must be connected. One that is not has no
// route, and its button answers 405.
func TestPanelHasEveryControl(t *testing.T) {
	opts, _ := testPanel(t)
	v := reflect.ValueOf(opts)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if k := f.Type.Kind(); (k == reflect.Interface || k == reflect.Pointer || k == reflect.Func) && v.Field(i).IsNil() {
			t.Errorf("web.Options.%s is not connected in panelOptions", f.Name)
		}
	}
}

// Each button's request must reach a handler.
func TestPanelRoutesExist(t *testing.T) {
	_, h := testPanel(t)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/status", ""},
		{"GET", "/api/wifi", ""},
		{"POST", "/api/wifi", `{}`},
		{"POST", "/api/network/ethernet-mode", `{"mode":"lan"}`},
		{"POST", "/api/network/keep", `{}`},
		{"POST", "/api/network/undo", `{}`},
		{"POST", "/api/network/scan", `{}`},
		{"POST", "/api/display/power", `{"powered":true}`},
		{"POST", "/api/display/wake-on-notification", `{"enabled":true}`},
		{"POST", "/api/update/check", `{}`},
		{"POST", "/api/update/install", `{}`},
		{"POST", "/api/update/rollback", `{}`},
		{"POST", "/api/rules/enforce", `{"enabled":true}`},
		{"POST", "/api/rules/restore", `{}`},
	} {
		req := httptest.NewRequest(route.method, "http://172.16.0.1"+route.path, strings.NewReader(route.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s answers %d: no handler is connected", route.method, route.path, rec.Code)
		}
	}
}
