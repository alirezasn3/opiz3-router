//go:build !linux
// +build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "opiz3-router requires Linux (for netlink/systemd/GPIO support). Build with GOOS=linux GOARCH=arm64 for the Orange Pi target.")
	os.Exit(2)
}
