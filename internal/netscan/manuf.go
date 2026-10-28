package netscan

import (
	"bufio"
	"bytes"
	_ "embed"
	"strings"
)

//go:embed manuf
var manufBytes []byte

// loadOUIDatabase reads the Wireshark manuf database embedded in the package.
func loadOUIDatabase() map[string]string {
	ouis := make(map[string]string)
	s := bufio.NewScanner(bytes.NewReader(manufBytes))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		prefix := strings.SplitN(fields[0], "/", 2)[0]
		prefix = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(prefix))
		if len(prefix) >= 6 {
			name := fields[1]
			if len(fields) >= 3 {
				name = strings.Join(fields[2:], " ")
			}
			ouis[prefix] = name
		}
	}
	return ouis
}

func macOUI(mac string) string {
	mac = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(mac)))
	if len(mac) < 6 {
		return ""
	}
	return mac
}

func manufacturer(ouis map[string]string, mac string) string {
	hex := macOUI(mac)
	for n := min(len(hex), 12); n >= 6; n-- {
		if name := ouis[hex[:n]]; name != "" {
			return name
		}
	}
	return ""
}
