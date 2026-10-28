//go:build !linux

package netscan

import (
	"context"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

func scanNetwork(parent context.Context, cfg config.Config, store *state.Store) {
	// Stub on non-Linux platforms
}
