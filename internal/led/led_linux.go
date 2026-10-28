//go:build linux

package led

import (
	"fmt"
	"os"
	"path"
)

func NewLED(p string) (*LED, error) {
	e := os.WriteFile(path.Join(p, "trigger"), []byte("none"), 0644)
	if e != nil {
		return nil, fmt.Errorf("failed to write to trigger file (are you running as root?): %w", e)
	}
	return &LED{path: p}, nil
}

func (led *LED) setBrightness(on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return os.WriteFile(path.Join(led.path, "brightness"), []byte(v), 0644)
}
