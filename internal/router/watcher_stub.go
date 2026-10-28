//go:build !linux

package router

import (
	"opiz3-router/internal/config"
)

type LinkWatcher struct {
	cfg config.Config
}

func NewLinkWatcher(cfg config.Config) *LinkWatcher {
	return &LinkWatcher{cfg: cfg}
}

func LinkLabel(cfg config.Config, name string) string {
	switch name {
	case cfg.Interfaces.Ethernet:
		return "ETH"
	case cfg.Interfaces.USBModem:
		return "4G"
	case cfg.Interfaces.Wireless:
		return "WIFI"
	case cfg.WireGuard.Interface:
		return "VPN"
	case cfg.Interfaces.Bridge:
		return "BRIDGE"
	}
	return name
}
