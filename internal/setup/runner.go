// Package setup prepares a stock Orange Pi Ubuntu image for the router, so the
// image itself needs no changes: packages, NetworkManager, the SPI display
// overlay, the Wi-Fi access point and DHCP server, the fallback netplan file
// and mini-net-man.
package setup

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs the system commands setup needs.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, e := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if e != nil {
		return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), e, text)
	}
	return text, nil
}

// ExecRunner runs commands on this machine.
func ExecRunner() Runner { return execRunner{} }
