package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"opiz3-router/internal/mnm"
)

const tokenName = "opiz3-router"

var loginLine = regexp.MustCompile(`login:\s+(\S+) / (\S+)`)

// installMiniNetMan puts mini-net-man's binary in its own folder, installs and
// starts its service, and gets an API token for the router.
func (in *Installer) installMiniNetMan(ctx context.Context) error {
	dir := in.MiniNetManDir()
	bin := filepath.Join(dir, "mini-net-man")
	if e := os.MkdirAll(dir, 0o755); e != nil {
		return e
	}
	switch {
	case in.MiniNetManBinary != "":
		in.say("using %s", in.MiniNetManBinary)
		data, e := os.ReadFile(in.MiniNetManBinary)
		if e != nil {
			return e
		}
		if _, e = writeIfChanged(bin, data, 0o755); e != nil {
			return e
		}
	case fileExists(bin):
		in.say("binary already at %s (update it with: %s update)", bin, bin)
	default:
		if e := in.downloadMiniNetMan(ctx, bin); e != nil {
			return e
		}
	}

	_, subnet, e := net.ParseCIDR(in.Cfg.Network.BridgeAddress)
	if e != nil {
		return e
	}
	// "install" creates config.yaml (with a generated login) the first time,
	// writes the service and starts it; later runs only restart it.
	args := []string{"install"}
	if !fileExists(filepath.Join(dir, "config.yaml")) {
		args = append(args, "-allow", subnet.String(), "-port", strconv.Itoa(in.Cfg.MiniNetManPort()))
	}
	out, e := in.Run.Run(ctx, bin, args...)
	if e != nil {
		return e
	}

	if in.tokenWorks(ctx) {
		in.say("API token already works")
		return nil
	}
	m := loginLine.FindStringSubmatch(out)
	if m == nil {
		in.note("mini-net-man was already set up, so its login was not shown. Make an API token in its panel (Settings, API tokens) and put it in %s as mini_net_man.token, or run: %s passwd admin", in.CfgPath, bin)
		return nil
	}
	if e = in.waitForPort(ctx); e != nil {
		return e
	}
	token, e := in.createToken(ctx, m[1], m[2])
	if e != nil {
		return fmt.Errorf("create API token: %w", e)
	}
	in.Cfg.MiniNetMan.Token = token
	if e = in.Cfg.Save(in.CfgPath); e != nil {
		return e
	}
	in.say("created an API token for the router")
	in.note("mini-net-man's panel is at %s (login %s / %s); change the password with: %s passwd %s", in.Cfg.MiniNetMan.URL, m[1], m[2], bin, m[1])
	return nil
}

func fileExists(p string) bool {
	_, e := os.Stat(p)
	return e == nil
}

func (in *Installer) tokenWorks(ctx context.Context) bool {
	if in.Cfg.MiniNetMan.Token == "" {
		return false
	}
	return mnm.NewClient(in.Cfg.MiniNetMan.URL, in.Cfg.MiniNetMan.Token).Ping(ctx) == nil
}

func (in *Installer) waitForPort(ctx context.Context) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Cfg.MiniNetManPort()))
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, e := net.DialTimeout("tcp", addr, time.Second)
		if e == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mini-net-man does not answer on %s: %w", addr, e)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// createToken logs in with the generated password and makes an API token,
// which cannot be made with a token.
func (in *Installer) createToken(ctx context.Context, user, password string) (string, error) {
	jar, e := cookiejar.New(nil)
	if e != nil {
		return "", e
	}
	c := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	post := func(path string, body, out any) error {
		data, _ := json.Marshal(body)
		req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(in.Cfg.MiniNetMan.URL, "/")+path, bytes.NewReader(data))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := c.Do(req)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if res.StatusCode >= 300 {
			return fmt.Errorf("%s: HTTP %d: %s", path, res.StatusCode, strings.TrimSpace(string(raw)))
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	if e = post("/api/login", map[string]string{"username": user, "password": password}, nil); e != nil {
		return "", e
	}
	var res struct {
		Token string `json:"token"`
	}
	if e = post("/api/tokens", map[string]any{"name": tokenName, "read_only": false, "password": password}, &res); e != nil {
		return "", e
	}
	if res.Token == "" {
		return "", errors.New("the answer holds no token")
	}
	return res.Token, nil
}

// downloadMiniNetMan fetches the latest release for this CPU and checks it
// against the release's checksums.txt before using it.
func (in *Installer) downloadMiniNetMan(ctx context.Context, bin string) error {
	asset := "mini-net-man-linux-" + runtime.GOARCH
	in.say("downloading %s", asset)
	sums, e := in.get(ctx, in.ReleaseBase+"/checksums.txt", 1<<20)
	if e != nil {
		return e
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}
	data, e := in.get(ctx, in.ReleaseBase+"/"+asset, 128<<20)
	if e != nil {
		return e
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != strings.ToLower(want) {
		return fmt.Errorf("%s does not match its checksum", asset)
	}
	_, e = writeIfChanged(bin, data, 0o755)
	return e
}

func (in *Installer) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", url, nil)
	if e != nil {
		return nil, e
	}
	res, e := in.HTTP.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, res.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s: too large", url)
	}
	return data, nil
}
