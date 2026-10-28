//go:build !linux

package collector

import (
	"errors"

	"golang.zx2c4.com/wireguard/wgctrl"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func newWGClient() (*wgctrl.Client, error) {
	return nil, errors.New("wgctrl unavailable on non-linux")
}

func (c *linkCollector) collect(store *state.Store) {}

func (c *wireguardCollector) collect(store *state.Store) {}

func collectClients(cfg config.Config, store *state.Store) {}
