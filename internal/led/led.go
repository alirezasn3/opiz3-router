package led

import (
	"sync"
	"time"
)

type LED struct {
	path string
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
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

func (led *LED) stopBlink() {
	if led.stop == nil {
		return
	}
	close(led.stop)
	<-led.done
	led.stop, led.done = nil, nil
	_ = led.setBrightness(false)
}
