//go:build linux

package main

import (
	"fmt"
	"os"
	"path"
	"sync"
	"time"
)

type LED struct {
	path string
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

func NewLED(p string) (*LED, error) {
	// 1. Take control of the LED from the OS. Setting this to "none" allows manual control.
	e := writeToFile(path.Join(p, "trigger"), "none")
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
	return writeToFile(path.Join(led.path, "brightness"), v)
}

// Off stops any blinking and turns the LED off.
func (led *LED) Off() error {
	led.mu.Lock()
	defer led.mu.Unlock()
	led.stopBlink()
	return led.setBrightness(false)
}

// Blink turns the LED on for d and off for i, repeatedly, until stopped.
func (led *LED) Blink(d, i time.Duration) {
	led.mu.Lock()
	defer led.mu.Unlock()
	led.stopBlink()
	stop, done := make(chan struct{}), make(chan struct{})
	led.stop, led.done = stop, done

	go func() {
		defer close(done)
		for {
			_ = led.setBrightness(true)
			select {
			case <-stop:
				return
			case <-time.After(d):
			}
			_ = led.setBrightness(false)
			select {
			case <-stop:
				return
			case <-time.After(i):
			}
		}
	}()
}

// StopBlink stops blinking and leaves the LED off.
func (led *LED) StopBlink() {
	led.mu.Lock()
	defer led.mu.Unlock()
	led.stopBlink()
}

// stopBlink must be called with mu held. It waits for the blink goroutine to
// exit so a late write cannot turn the LED back on.
func (led *LED) stopBlink() {
	if led.stop == nil {
		return
	}
	close(led.stop)
	<-led.done
	led.stop, led.done = nil, nil
	_ = led.setBrightness(false)
}

// Helper function to write strings to sysfs files
func writeToFile(filename, data string) error {
	// os.WriteFile overwrites the file with our byte slice
	return os.WriteFile(filename, []byte(data), 0644)
}
