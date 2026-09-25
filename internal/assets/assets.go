package assets

import "embed"

// Files contains the display font compiled into the executable.
// Deployable system configuration remains in deploy/ because it is consumed by
// netplan, hostapd, dnsmasq, and the bootloader rather than by the Go process.
//
//go:embed fonts manuf
var Files embed.FS

const FontPath = "fonts/UbuntuMono-Regular.ttf"
const ManufPath = "manuf"
