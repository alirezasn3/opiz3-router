package setup

import "testing"

func TestUpdateBootEnv(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"adds both lines": {
			"verbosity=1\nrootdev=UUID=abc\n",
			"verbosity=1\nrootdev=UUID=abc\noverlay_prefix=sun50i-h616\noverlays=spi1-cs0-spidev\n",
		},
		"no trailing newline": {
			"rootdev=UUID=abc",
			"rootdev=UUID=abc\noverlay_prefix=sun50i-h616\noverlays=spi1-cs0-spidev\n",
		},
		"appends to existing overlays": {
			"overlay_prefix=sun50i-h616\noverlays=i2c0 uart5\nrootdev=UUID=abc\n",
			"overlay_prefix=sun50i-h616\noverlays=i2c0 uart5 spi1-cs0-spidev\nrootdev=UUID=abc\n",
		},
		"fixes a wrong prefix": {
			"overlay_prefix=sun50i-h6\noverlays=spi1-cs0-spidev\n",
			"overlay_prefix=sun50i-h616\noverlays=spi1-cs0-spidev\n",
		},
		"empty overlays line": {
			"overlay_prefix=sun50i-h616\noverlays=\n",
			"overlay_prefix=sun50i-h616\noverlays=spi1-cs0-spidev\n",
		},
		"commented lines are not settings": {
			"#overlays=i2c0\n",
			"#overlays=i2c0\noverlay_prefix=sun50i-h616\noverlays=spi1-cs0-spidev\n",
		},
	} {
		got, changed := UpdateBootEnv(tc.in)
		if got != tc.want || !changed {
			t.Errorf("%s:\n got %q (changed %v)\nwant %q", name, got, changed, tc.want)
		}
	}
}

func TestUpdateBootEnvIsIdempotent(t *testing.T) {
	once, _ := UpdateBootEnv("rootdev=UUID=abc\n")
	twice, changed := UpdateBootEnv(once)
	if changed || twice != once {
		t.Fatalf("second run changed the file: %q", twice)
	}
}
