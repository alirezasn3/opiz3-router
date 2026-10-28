package router

import (
	"bytes"
	"fmt"
	"text/template"

	"opiz3-router/internal/config"
)

type Mode string

const (
	ModeUplink Mode = "uplink"
	ModeLAN    Mode = "lan"
)

func (m Mode) Valid() bool { return m == ModeUplink || m == ModeLAN }

var netplanTmpl = template.Must(template.New("netplan").Parse(`# Written by opiz3-router. Switching the Ethernet mode in the panel
# replaces this file. The LAN bridge is not here: mini-net-man creates it.
network:
  version: 2
  renderer: networkd
  ethernets:
{{- if eq .Mode "uplink"}}
    "{{.Ethernet}}":
      optional: true
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
        route-metric: 100
      nameservers:
        addresses: [{{.DNS}}]
{{- else}}
    "{{.Ethernet}}": {}
{{- end}}
{{- if .Modem}}
    "{{.Modem}}":
      optional: true
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
        route-metric: 101
      nameservers:
        addresses: [{{.DNS}}]
{{- end}}
`))

// RenderNetplan returns the netplan file for mode.
func RenderNetplan(mode Mode, cfg config.Config) ([]byte, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("unknown ethernet mode %q", mode)
	}
	dns := ""
	for i, d := range cfg.Network.DNS {
		if i > 0 {
			dns += ", "
		}
		dns += `"` + d + `"`
	}
	var b bytes.Buffer
	e := netplanTmpl.Execute(&b, map[string]any{
		"Mode":     string(mode),
		"Ethernet": cfg.Interfaces.Ethernet,
		"Modem":    cfg.Interfaces.USBModem,
		"DNS":      dns,
	})
	return b.Bytes(), e
}
