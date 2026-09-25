// Package netplan writes the router's netplan configuration for the two ways
// the Ethernet port can be used.
package netplan

import (
	"bytes"
	"fmt"
	"text/template"

	"opiz3-router/internal/config"
)

// Mode is how the Ethernet port is used.
type Mode string

const (
	// Uplink: the Ethernet port gets an address by DHCP and is an internet
	// connection, next to the 4G modem.
	Uplink Mode = "uplink"
	// LAN: the Ethernet port is a port of the LAN bridge, so devices plugged
	// into it join the LAN. The 4G modem is the only internet connection.
	LAN Mode = "lan"
)

func (m Mode) Valid() bool { return m == Uplink || m == LAN }

// Both modes keep the Wi-Fi access point on the bridge; hostapd adds wlan0.
var tmpl = template.Must(template.New("netplan").Parse(`# Written by opiz3-router. Switching the Ethernet mode in the panel
# replaces this file.
network:
  version: 2
  renderer: networkd
  ethernets:
{{- if eq .Mode "uplink"}}
    "{{.Ethernet}}":
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
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
        route-metric: 101
      nameservers:
        addresses: [{{.DNS}}]
{{- end}}
  bridges:
    "{{.Bridge}}":
      addresses: ["{{.BridgeAddress}}"]
{{- if eq .Mode "lan"}}
      interfaces: ["{{.Ethernet}}"]
{{- end}}
`))

// Render returns the netplan file for mode. cfg must have been validated, so
// the names and addresses are safe to put into YAML.
func Render(mode Mode, cfg config.Config) ([]byte, error) {
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
	e := tmpl.Execute(&b, map[string]any{
		"Mode":          string(mode),
		"Ethernet":      cfg.Interfaces.Ethernet,
		"Modem":         cfg.Interfaces.USBModem,
		"Bridge":        cfg.Interfaces.Bridge,
		"BridgeAddress": cfg.Network.BridgeAddress,
		"DNS":           dns,
	})
	return b.Bytes(), e
}
