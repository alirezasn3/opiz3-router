//go:build linux

package main

import "testing"

func TestMacOUI(t *testing.T) {
	for _, test := range []struct {
		mac, want string
	}{
		{"", ""},
		{"00:1B:C5:12:34:56", "001BC5123456"},
		{"00-1B-C5-12-34-56", "001BC5123456"},
	} {
		if got := macOUI(test.mac); got != test.want {
			t.Errorf("macOUI(%q) = %q, want %q", test.mac, got, test.want)
		}
	}
}
