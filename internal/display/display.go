package display

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"time"

	"opiz3-router/internal/state"
)

//go:embed fonts/UbuntuMono-Regular.ttf
var fontBytes []byte

const (
	ScreenWidth  = 240
	ScreenHeight = 280
)

type screenDevice interface {
	fillRectangleWithBuffer(x, y, w, h int, buf []color.RGBA) error
	enableBacklight(on bool) error
}

type Display struct {
	x          int
	y          int
	px         int
	py         int
	fontHeight int
	drawer     *font.Drawer
	large      font.Face
	small      font.Face
	image      *image.RGBA
	frame      *image.RGBA
	buffer     []color.RGBA
	dirty      bool
	sent       bool
	powered    bool
	mu         sync.Mutex

	device screenDevice
}

func (d *Display) initFrame() error {
	d.image = image.NewRGBA(image.Rect(0, 0, ScreenHeight, ScreenWidth))
	d.frame = image.NewRGBA(d.image.Rect)
	d.dirty = true
	d.powered = true
	d.px = 12
	d.py = 12
	d.buffer = make([]color.RGBA, ScreenWidth*ScreenHeight)

	ttf, e := opentype.Parse(fontBytes)
	if e != nil {
		return fmt.Errorf("failed to parse font file: %w", e)
	}

	face, e := opentype.NewFace(ttf, &opentype.FaceOptions{
		Size:    18,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if e != nil {
		return fmt.Errorf("failed to create font face: %w", e)
	}
	d.fontHeight = face.Metrics().Ascent.Ceil() + face.Metrics().Descent.Ceil()

	d.large, e = opentype.NewFace(ttf, &opentype.FaceOptions{Size: 26, DPI: 72, Hinting: font.HintingFull})
	if e != nil {
		return fmt.Errorf("failed to create large font face: %w", e)
	}

	d.small, e = opentype.NewFace(ttf, &opentype.FaceOptions{Size: 14, DPI: 72, Hinting: font.HintingFull})
	if e != nil {
		return fmt.Errorf("failed to create small font face: %w", e)
	}

	d.x = d.px
	d.y = d.py + d.fontHeight

	d.drawer = &font.Drawer{
		Dst:  d.image,
		Src:  image.NewUniform(color.White),
		Face: face,
		Dot:  fixed.P(d.x, d.y),
	}

	return nil
}

// NewDisplay creates a display instance initialized in memory without touching hardware.
func NewDisplay() (*Display, error) {
	d := &Display{}
	if err := d.initFrame(); err != nil {
		return nil, err
	}
	return d, nil
}

// SetPower turns the display on or off. When turned off, the backlight is disabled
// and frame transmission is skipped.
func (d *Display) SetPower(on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.powered == on {
		return nil
	}
	d.powered = on
	if d.device != nil {
		if err := d.device.enableBacklight(on); err != nil {
			return err
		}
	}
	d.dirty = true
	return nil
}

// IsPowered reports whether the display is turned on.
func (d *Display) IsPowered() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.powered
}

// Controller adapts Display to update state.Store and satisfy web.DisplayController.
type Controller struct {
	d                  *Display
	store              *state.Store
	mu                 sync.Mutex
	wakeOnNotification bool
	wakeTimer          *time.Timer
}

// NewController creates a display controller that sets hardware power and updates the state store.
func NewController(d *Display, store *state.Store) *Controller {
	c := &Controller{
		d:                  d,
		store:              store,
		wakeOnNotification: true,
	}
	if store != nil {
		store.Update(func(s *state.Snapshot) {
			s.Display.WakeOnNotification = true
		})
		store.OnNotification(func(n state.Notification) {
			c.handleNotification(n)
		})
	}
	return c
}

// SetDisplayPower sets the display power and notifies the state store.
func (c *Controller) SetDisplayPower(on bool) error {
	c.mu.Lock()
	if c.wakeTimer != nil {
		c.wakeTimer.Stop()
		c.wakeTimer = nil
	}
	c.mu.Unlock()

	if err := c.d.SetPower(on); err != nil {
		return err
	}
	if c.store != nil {
		c.store.Update(func(s *state.Snapshot) {
			s.Display.Powered = on
		})
	}
	return nil
}

// SetWakeOnNotification enables or disables temporarily waking the display on notifications.
func (c *Controller) SetWakeOnNotification(enabled bool) error {
	c.mu.Lock()
	c.wakeOnNotification = enabled
	c.mu.Unlock()
	if c.store != nil {
		c.store.Update(func(s *state.Snapshot) {
			s.Display.WakeOnNotification = enabled
		})
	}
	return nil
}

func (c *Controller) handleNotification(n state.Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wakeOnNotification || c.store == nil {
		return
	}
	snap := c.store.Snapshot()
	// If the display was manually turned off, turn it on temporarily for 15 seconds.
	if !snap.Display.Powered {
		_ = c.d.SetPower(true)
		if c.wakeTimer != nil {
			c.wakeTimer.Stop()
		}
		c.wakeTimer = time.AfterFunc(15*time.Second, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.wakeTimer = nil
			// If still configured to be off in the state store, turn hardware off
			if !c.store.Snapshot().Display.Powered {
				_ = c.d.SetPower(false)
			}
		})
	}
}

// Render sends the image to the display if it changed since the last call.
// The image is drawn in landscape (280x240) and the panel is portrait (240x280).
func (d *Display) Render() error {
	d.mu.Lock()
	if !d.powered {
		d.mu.Unlock()
		return nil
	}
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

	if d.device != nil {
		if err := d.device.fillRectangleWithBuffer(0, 0, ScreenWidth, ScreenHeight, d.buffer); err != nil {
			d.mu.Lock()
			d.dirty, d.sent = true, false
			d.mu.Unlock()
			return fmt.Errorf("failed to write image to display buffer: %w", err)
		}
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

type Canvas struct {
	d *Display
}

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

func (c *Canvas) Blend(x, y, w, h int, col color.RGBA, alpha uint8) {
	draw.DrawMask(c.d.image, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, image.NewUniform(color.Alpha{A: alpha}), image.Point{}, draw.Over)
}

func (c *Canvas) Clear(col color.RGBA) {
	c.FillRect(0, 0, c.Width(), c.Height(), col)
}

func (c *Canvas) Circle(cx, cy, r int, col color.RGBA) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r+r/2 {
				c.d.image.SetRGBA(cx+dx, cy+dy, col)
			}
		}
	}
}

func (c *Canvas) Triangle(x, y, size int, up bool, col color.RGBA) {
	for row := range size {
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

func (c *Canvas) TextWidth(s string, large bool) int {
	return font.MeasureString(c.face(large), s).Ceil()
}

func (c *Canvas) Text(x, y int, s string, large bool, col color.RGBA) int {
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.face(large), Dot: fixed.P(x, y)}
	dr.DrawString(s)
	return dr.Dot.X.Ceil()
}

func (c *Canvas) SmallTextWidth(s string) int {
	return font.MeasureString(c.d.small, s).Ceil()
}

func (c *Canvas) SmallTextCentered(x, y int, s string, col color.RGBA) int {
	w := font.MeasureString(c.d.small, s).Ceil()
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.d.small, Dot: fixed.P(x-w/2, y)}
	dr.DrawString(s)
	return w
}

func (c *Canvas) SmallText(x, y int, s string, col color.RGBA) int {
	dr := font.Drawer{Dst: c.d.image, Src: image.NewUniform(col), Face: c.d.small, Dot: fixed.P(x, y)}
	dr.DrawString(s)
	return dr.Dot.X.Ceil()
}

func (c *Canvas) Ascent(large bool) int {
	return c.face(large).Metrics().Ascent.Ceil()
}

func (c *Canvas) SmallAscent() int {
	return c.d.small.Metrics().Ascent.Ceil()
}
