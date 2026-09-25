//go:build linux

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"
	"sync"
	"time"

	st7789 "github.com/nwhirschfeld/periph.io-st7789"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

type Display struct {
	x          int
	y          int
	px         int
	py         int
	fontHeight int
	display    st7789.Device
	drawer     *font.Drawer
	// large is a bigger face for the status screen banner.
	large font.Face
	// small is for chart labels.
	small font.Face
	image *image.RGBA
	// frame is a copy of image taken under mu, so the slow SPI transfer in
	// Render does not block drawing.
	frame  *image.RGBA
	buffer []color.RGBA
	// dirty is set by every drawing method; Render skips unchanged frames.
	dirty bool
	// sent is true when frame is what the panel shows. Until then the frame
	// must be sent even if it equals the image.
	sent bool
	mu   sync.Mutex
}

// initFrame creates the frame buffers and font faces. It needs no hardware.
func (d *Display) initFrame(fontBytes []byte) error {
	// create empty image
	d.image = image.NewRGBA(image.Rect(0, 0, screenHeight, screenWidth))
	d.frame = image.NewRGBA(d.image.Rect)
	d.dirty = true // clear whatever the panel showed before

	// load font file
	// parse font
	ttf, e := opentype.Parse(fontBytes)
	if e != nil {
		return fmt.Errorf("Failed to parse font file: %s\n", e.Error())
	}

	// create font face
	face, e := opentype.NewFace(ttf, &opentype.FaceOptions{
		Size:    18,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if e != nil {
		return fmt.Errorf("Failed to create font face: %s\n", e.Error())
	}
	d.fontHeight = face.Metrics().Ascent.Ceil() + face.Metrics().Descent.Ceil()
	d.large, e = opentype.NewFace(ttf, &opentype.FaceOptions{Size: 26, DPI: 72, Hinting: font.HintingFull})
	if e != nil {
		return fmt.Errorf("Failed to create font face: %s\n", e.Error())
	}
	d.small, e = opentype.NewFace(ttf, &opentype.FaceOptions{Size: 14, DPI: 72, Hinting: font.HintingFull})
	if e != nil {
		return fmt.Errorf("Failed to create font face: %s\n", e.Error())
	}

	d.x = d.px
	d.y = d.py + d.fontHeight

	// create text drawer
	d.drawer = &font.Drawer{
		Dst:  d.image,
		Src:  image.NewUniform(color.White),
		Face: face,
		Dot:  fixed.P(d.x, d.y),
	}

	return nil
}

// create and setup display instance
func (d *Display) Setup(port, res, dc, cs, blk string, fontBytes []byte) error {
	d.px = 12
	d.py = 12
	d.buffer = make([]color.RGBA, screenWidth*screenHeight)

	// init host machine drivers
	if _, e := host.Init(); e != nil {
		return fmt.Errorf("Failed to initialize host: %s\n", e.Error())
	}

	// open spi port
	spiPort, e := spireg.Open(port)
	if e != nil {
		return fmt.Errorf("Failed to open spi port: %s\n", e.Error())
	}
	// defer spiPort.Close()

	// connect to st7789 display through spi
	conn, e := spiPort.Connect(30*physic.MegaHertz, spi.Mode0, 8)
	if e != nil {
		return fmt.Errorf("Failed to connect to display: %s\n", e.Error())
	}

	// setup display
	d.display = st7789.New(conn, gpioreg.ByName(res), gpioreg.ByName(dc), gpioreg.ByName(cs), gpioreg.ByName(blk))
	d.display.Configure(st7789.Config{
		Width:      240,
		Height:     280,
		Rotation:   st7789.NO_ROTATION,
		RowOffset:  20,
		FrameRate:  st7789.FRAMERATE_60,
		VSyncLines: st7789.MAX_VSYNC_SCANLINES,
	})
	d.display.EnableBacklight(true)

	if e := d.initFrame(fontBytes); e != nil {
		return e
	}

	go func() {
		failing := false
		for range time.NewTicker(time.Millisecond * 330).C {
			// Log only when the display starts or stops failing, not every frame.
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

// Render sends the image to the display if it changed since the last call.
//
// The image is drawn in landscape (280x240) and the panel is portrait
// (240x280), so the frame is rotated on the way out: panel row x, column y
// shows image pixel (w-1-x, y).
func (d *Display) Render() error {
	d.mu.Lock()
	// The status screen is redrawn every second; only send real changes.
	if !d.dirty || (d.sent && bytes.Equal(d.frame.Pix, d.image.Pix)) {
		d.dirty = false
		d.mu.Unlock()
		return nil
	}
	copy(d.frame.Pix, d.image.Pix)
	d.dirty = false
	d.mu.Unlock()

	w, h := d.frame.Rect.Dx(), d.frame.Rect.Dy()
	i := 0
	for x := range w {
		for y := range h {
			o := d.frame.PixOffset(w-1-x, y)
			p := d.frame.Pix[o : o+4 : o+4]
			d.buffer[i] = color.RGBA{p[0], p[1], p[2], p[3]}
			i++
		}
	}
	if e := d.display.FillRectangleWithBuffer(0, 0, screenWidth, screenHeight, d.buffer); e != nil {
		d.mu.Lock()
		// Retry on the next tick; the frame buffer already matches the
		// image, so clear sent to stop the comparison skipping it.
		d.dirty, d.sent = true, false
		d.mu.Unlock()
		return fmt.Errorf("failed to write image to display buffer: %w", e)
	}
	d.mu.Lock()
	d.sent = true
	d.mu.Unlock()
	return nil
}

func (d *Display) NewLine(block bool) {
	if block {
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	d.drawer.Dot = fixed.P(d.px, d.y+d.fontHeight)
	d.y += d.fontHeight
	d.x = d.px
}

func (d *Display) Text(text string, block bool) {
	if block {
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	d.drawer.Dot = fixed.P(d.x, d.y)
	d.drawer.DrawString(text)
	d.x += len(text) * 11
	d.dirty = true
}

// Canvas draws on the display image. Get one from Display.Draw.
type Canvas struct {
	d *Display
}

// Draw runs fn with the display locked, so a whole frame is drawn at once and
// never sent half finished.
func (d *Display) Draw(fn func(c *Canvas)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(&Canvas{d: d})
	d.dirty = true
}

func (c *Canvas) Width() int  { return c.d.image.Rect.Dx() }
func (c *Canvas) Height() int { return c.d.image.Rect.Dy() }

func (c *Canvas) FillRect(x, y, w, h int, col color.RGBA) {
	draw.Draw(c.d.image, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, draw.Src)
}

// Blend paints col over the rectangle with the given opacity (0-255), so
// what is underneath shows through.
func (c *Canvas) Blend(x, y, w, h int, col color.RGBA, alpha uint8) {
	draw.DrawMask(c.d.image, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, image.NewUniform(color.Alpha{A: alpha}), image.Point{}, draw.Over)
}

func (c *Canvas) Clear(col color.RGBA) {
	c.FillRect(0, 0, c.Width(), c.Height(), col)
}

// Circle fills a circle of radius r centred on (cx, cy).
func (c *Canvas) Circle(cx, cy, r int, col color.RGBA) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r+r/2 {
				c.d.image.SetRGBA(cx+dx, cy+dy, col)
			}
		}
	}
}

// Triangle fills an arrow head of the given size with its top-left corner at
// (x, y), pointing up or down.
func (c *Canvas) Triangle(x, y, size int, up bool, col color.RGBA) {
	for row := range size {
		// The half-width grows from the tip to the base.
		half := row / 2
		if !up {
			half = (size - 1 - row) / 2
		}
		c.FillRect(x+size/2-half, y+row, 2*half+1, 1, col)
	}
}

func (c *Canvas) face(large bool) font.Face {
	if large {
		return c.d.large
	}
	return c.d.drawer.Face
}

// TextWidth returns the width of s in pixels.
func (c *Canvas) TextWidth(s string, large bool) int {
	return font.MeasureString(c.face(large), s).Ceil()
}

// Text draws s with its baseline at y and returns the x after it.
func (c *Canvas) Text(x, y int, s string, large bool, col color.RGBA) int {
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.face(large), Dot: fixed.P(x, y)}
	dr.DrawString(s)
	return dr.Dot.X.Ceil()
}

// SmallTextWidth returns the width of s in the small face.
func (c *Canvas) SmallTextWidth(s string) int {
	return font.MeasureString(c.d.small, s).Ceil()
}

// SmallTextCentered draws s in the small face with its baseline at y,
// centred on x, and returns its width.
func (c *Canvas) SmallTextCentered(x, y int, s string, col color.RGBA) int {
	w := font.MeasureString(c.d.small, s).Ceil()
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.d.small, Dot: fixed.P(x-w/2, y)}
	dr.DrawString(s)
	return w
}

// SmallText draws s in the small face with its baseline at (x, y).
func (c *Canvas) SmallText(x, y int, s string, col color.RGBA) int {
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.d.small, Dot: fixed.P(x, y)}
	dr.DrawString(s)
	return dr.Dot.X.Ceil()
}

// Ascent returns the height of the face above the baseline.
func (c *Canvas) Ascent(large bool) int {
	return c.face(large).Metrics().Ascent.Ceil()
}

// SmallAscent returns the height of the small face above its baseline.
func (c *Canvas) SmallAscent() int {
	return c.d.small.Metrics().Ascent.Ceil()
}
