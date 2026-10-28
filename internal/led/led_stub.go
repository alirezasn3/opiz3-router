//go:build !linux

package led

func NewLED(p string) (*LED, error) {
	return &LED{path: p}, nil
}

func (led *LED) setBrightness(on bool) error {
	return nil
}
