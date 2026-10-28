// Package mnm talks to mini-net-man's HTTP API. mini-net-man owns the LAN
// bridge, firewall, NAT and VPN routing; this package describes what the
// router needs and makes mini-net-man's config match it.
package mnm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Meta is the part every managed item has.
type Meta struct {
	ID       string `json:"id,omitempty"`
	Comment  string `json:"comment,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

func (m *Meta) meta() *Meta { return m }

type Address struct {
	Meta
	Address   string `json:"address"`
	Interface string `json:"interface"`
}

type Route struct {
	Meta
	Dst     string `json:"dst"`
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev,omitempty"`
	Table   string `json:"table,omitempty"`
	Metric  int    `json:"metric,omitempty"`
	Type    string `json:"type,omitempty"`
}

type Rule struct {
	Meta
	Priority int    `json:"priority"`
	Src      string `json:"src,omitempty"`
	Dst      string `json:"dst,omitempty"`
	Iif      string `json:"iif,omitempty"`
	Oif      string `json:"oif,omitempty"`
	Family   string `json:"family,omitempty"`
	Action   string `json:"action,omitempty"`
	Table    string `json:"table,omitempty"`
	Check    string `json:"check,omitempty"`
}

type FirewallRule struct {
	Meta
	Chain     string `json:"chain"`
	Action    string `json:"action"`
	InIface   string `json:"in_iface,omitempty"`
	OutIface  string `json:"out_iface,omitempty"`
	Src       string `json:"src,omitempty"`
	Dst       string `json:"dst,omitempty"`
	Family    string `json:"family,omitempty"`
	Proto     string `json:"proto,omitempty"`
	SrcPort   string `json:"src_port,omitempty"`
	DstPort   string `json:"dst_port,omitempty"`
	ConnState string `json:"conn_state,omitempty"`
	MSS       int    `json:"mss,omitempty"`
}

type Bridge struct {
	Meta
	Name  string   `json:"name"`
	Ports []string `json:"ports,omitempty"`
	STP   bool     `json:"stp,omitempty"`
	MTU   int      `json:"mtu,omitempty"`
}

type Check struct {
	Meta
	Name      string `json:"name"`
	Target    string `json:"target"`
	Interface string `json:"interface"`
	Table     string `json:"table,omitempty"`
	// Zero values mean mini-net-man's defaults.
	IntervalSecs int `json:"interval_secs,omitempty"`
	TimeoutMS    int `json:"timeout_ms,omitempty"`
	FailAfter    int `json:"fail_after,omitempty"`
	RecoverAfter int `json:"recover_after,omitempty"`
}

// CheckStatus is the latest result of a health check.
type CheckStatus struct {
	State     string    `json:"state"` // up, down or unknown
	Since     time.Time `json:"since"`
	LastProbe time.Time `json:"last_probe"`
	RTTMS     float64   `json:"rtt_ms"`
	Error     string    `json:"error,omitempty"`
}

// Issue is something mini-net-man saved but could not apply to the kernel.
type Issue struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

func (i Issue) String() string {
	if i.Kind == "" {
		return i.Message
	}
	return i.Kind + ": " + i.Message
}

// APIError is an error answer from mini-net-man.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("mini-net-man: %s (HTTP %d)", e.Message, e.Status)
}

// Client calls mini-net-man's API with a bearer token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimSuffix(baseURL, "/"),
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Do sends one request. in is encoded as JSON (when not nil) and the answer is
// decoded into out (when not nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, e := json.Marshal(in)
		if e != nil {
			return e
		}
		body = bytes.NewReader(data)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := c.http.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if e != nil {
		return e
	}
	if res.StatusCode >= 300 {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.Error == "" {
			msg.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: res.StatusCode, Message: msg.Error}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Ping checks that the API answers and the token is accepted.
func (c *Client) Ping(ctx context.Context) error {
	return c.Do(ctx, "GET", "/api/me", nil, nil)
}

// Unauthorized reports whether e means the token was refused.
func Unauthorized(e error) bool {
	var api *APIError
	return errors.As(e, &api) && api.Status == http.StatusUnauthorized
}

// writeResult is the answer to every change.
type writeResult struct {
	ID     string  `json:"id"`
	Issues []Issue `json:"issues"`
}
