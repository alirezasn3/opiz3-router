package screen

import (
	"fmt"
	"time"

	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
)

type NetworkModel struct {
	Title, Summary, Message, Footer string
	Devices                         []state.NetworkDevice
	FooterDot                       bool
	FooterLevel                     Level
}

func BuildNetwork(snap state.Snapshot, cfg config.Config, now time.Time) NetworkModel {
	n := snap.NetworkScan
	m := NetworkModel{Title: "Network scan", Summary: n.Interface}
	switch {
	case n.Error != "":
		m.Message = "Scan error: " + n.Error
	case n.UpdatedAt.IsZero():
		m.Message = "Scanning " + n.Interface + "..."
	default:
		m.Devices = n.Devices
		m.Summary = fmt.Sprintf("%d device%s", len(n.Devices), pluralSuffix(len(n.Devices)))
		if n.Subnet != "" {
			m.Footer = n.Subnet
		}
	}
	if f, ok := footerNotification(snap.Notifications, now); ok {
		m.Footer, m.FooterDot, m.FooterLevel = f.Text, true, NotificationLevel(f.Level)
	}
	return m
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
