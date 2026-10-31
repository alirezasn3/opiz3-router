//go:build linux

package main

import (
	"opiz3-router/internal/config"
	"opiz3-router/internal/mnm"
	"opiz3-router/internal/setup"
	"opiz3-router/internal/web"
)

// panelControls are the parts the web panel's buttons act on.
type panelControls struct {
	Network web.NetworkSwitcher
	Display web.DisplayController
	Scanner web.NetworkScanner
	Syncer  *mnm.Syncer
	Updater web.UpdateController
}

// panelOptions connects the web panel to every control it offers. A control
// that is left out has no route at all, and its button gets a 405 from the
// panel, so every field of web.Options is set here (panel_test.go checks it).
func panelOptions(cfg config.Config, cfgPath string, c panelControls) web.Options {
	return web.Options{
		Username: cfg.Panel.Username,
		Password: cfg.Panel.Password,
		Network:  c.Network,
		Display:  c.Display,
		Scanner:  c.Scanner,
		Updater:  c.Updater,
		Rules:    mnm.NewRules(c.Syncer, cfgPath),
		WiFi:     setup.NewAP(cfg, cfgPath),
	}
}
