//go:build !linux

package display

// Setup is a stub for non-Linux environments where no physical ST7789 display is attached.
func (d *Display) Setup(port, res, dc, cs, blk string) error {
	return nil
}
