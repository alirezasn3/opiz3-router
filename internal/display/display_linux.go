//go:build linux

package display

import (
	"fmt"
	"image/color"
	"log"
	"time"

	st7789 "github.com/nwhirschfeld/periph.io-st7789"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

type linuxST7789Device struct {
	dev    st7789.Device
	blkPin gpio.PinIO
}

func (l *linuxST7789Device) fillRectangleWithBuffer(x, y, w, h int, buf []color.RGBA) error {
	return l.dev.FillRectangleWithBuffer(int16(x), int16(y), int16(w), int16(h), buf)
}

func (l *linuxST7789Device) enableBacklight(on bool) error {
	l.dev.EnableBacklight(on)
	return nil
}

// Setup initializes the physical ST7789 display over SPI.
func (d *Display) Setup(port, res, dc, cs, blk string) error {
	if _, e := host.Init(); e != nil {
		return fmt.Errorf("failed to initialize host: %w", e)
	}

	spiPort, e := spireg.Open(port)
	if e != nil {
		return fmt.Errorf("failed to open spi port: %w", e)
	}

	conn, e := spiPort.Connect(30*physic.MegaHertz, spi.Mode0, 8)
	if e != nil {
		return fmt.Errorf("failed to connect to display: %w", e)
	}

	resPin := gpioreg.ByName(res)
	dcPin := gpioreg.ByName(dc)
	csPin := gpioreg.ByName(cs)
	blkPin := gpioreg.ByName(blk)

	dev := st7789.New(conn, resPin, dcPin, csPin, blkPin)
	dev.Configure(st7789.Config{
		Width:      240,
		Height:     280,
		Rotation:   st7789.NO_ROTATION,
		RowOffset:  20,
		FrameRate:  st7789.FRAMERATE_60,
		VSyncLines: st7789.MAX_VSYNC_SCANLINES,
	})
	dev.EnableBacklight(true)

	d.mu.Lock()
	d.device = &linuxST7789Device{dev: dev, blkPin: blkPin}
	d.mu.Unlock()

	go func() {
		failing := false
		for range time.NewTicker(time.Millisecond * 330).C {
			if e := d.Render(); (e != nil) != failing {
				failing = e != nil
				if failing {
					log.Printf("display: %v", e)
				} else {
					log.Print("display: rendering recovered")
				}
			}
		}
	}()

	return nil
}
