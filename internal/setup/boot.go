package setup

import (
	"slices"
	"strings"
)

const (
	overlayPrefix  = "sun50i-h616"
	displayOverlay = "spi1-cs0-spidev"
)

// UpdateBootEnv returns the contents of /boot/orangepiEnv.txt with the SPI
// display overlay enabled, and whether that changed anything. Other lines,
// such as the root filesystem UUID, are kept as they are.
func UpdateBootEnv(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	prefix, overlays := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "overlay_prefix="):
			prefix = i
		case strings.HasPrefix(l, "overlays="):
			overlays = i
		}
	}

	if prefix < 0 {
		lines = append(lines, "overlay_prefix="+overlayPrefix)
	} else {
		lines[prefix] = "overlay_prefix=" + overlayPrefix
	}
	if overlays < 0 {
		lines = append(lines, "overlays="+displayOverlay)
	} else if tokens := strings.Fields(strings.TrimPrefix(lines[overlays], "overlays=")); !slices.Contains(tokens, displayOverlay) {
		lines[overlays] = "overlays=" + strings.Join(append(tokens, displayOverlay), " ")
	}
	out := strings.Join(lines, "\n") + "\n"
	return out, out != content
}
