package collector

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	dwr910m "github.com/alirezasn3/dlink-dwr-910m-api"
	"golang.zx2c4.com/wireguard/wgctrl"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/sysinfo"
)

// Start keeps store up to date until ctx is cancelled.
func Start(ctx context.Context, cfg config.Config, store *state.Store) error {
	wgc, e := newWGClient()
	if e != nil {
		log.Printf("collector: wgctrl unavailable: %v", e)
	} else if wgc != nil {
		go func() {
			<-ctx.Done()
			wgc.Close()
		}()
	}

	dwr := dwr910m.New("http://"+cfg.Modem.Address, 2*time.Second)
	links := &linkCollector{cfg: cfg, prev: map[string]counterSample{}}

	if wgc != nil {
		wg := &wireguardCollector{name: cfg.WireGuard.Interface, client: wgc, prev: map[string]counterSample{}}
		go every(ctx, time.Second, func() { wg.collect(store) })
	}

	go every(ctx, 2*time.Second, func() { links.collect(store) })
	go every(ctx, 3*time.Second, func() { collectModem(dwr, store) })
	sms := newSMSCollector(dwr)
	go every(ctx, smsInterval, func() { sms.collect(store) })
	go every(ctx, 5*time.Second, func() { collectClients(cfg, store) })
	go every(ctx, 5*time.Second, func() { collectSystem(store) })
	return nil
}

func every(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type counterSample struct {
	at     time.Time
	rx, tx uint64
}

func rates(prev, cur counterSample) (float64, float64) {
	dt := cur.at.Sub(prev.at).Seconds()
	if prev.at.IsZero() || dt <= 0 || cur.rx < prev.rx || cur.tx < prev.tx {
		return 0, 0
	}
	return float64(cur.rx-prev.rx) / dt, float64(cur.tx-prev.tx) / dt
}

type linkCollector struct {
	cfg  config.Config
	prev map[string]counterSample
}

type wireguardCollector struct {
	name   string
	client *wgctrl.Client
	prev   map[string]counterSample
}

func cleanModemText(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "&nbsp;", " ")), " ")
}

func collectModem(dwr *dwr910m.DWR910M, store *state.Store) {
	var m state.Modem
	st, e := dwr.GetStatus()
	if e != nil {
		m.Error = e.Error()
	} else {
		m = state.Modem{
			Reachable:        true,
			NetworkType:      cleanModemText(st.NetworkType),
			SignalStrength:   cleanModemText(st.SignalStrength),
			SignalLevel:      st.SignalStrengthLevel,
			IPAddress:        cleanModemText(st.IPAddress),
			Uptime:           cleanModemText(st.Uptime),
			ConnectedDevices: st.ConnectedDevices,
			SoftwareVersion:  cleanModemText(st.SoftwareVersion),
		}
	}
	store.Update(func(s *state.Snapshot) { s.Modem = m })
}

const smsInterval = 20 * time.Second
const smsLimit = 50
const smsMaxPages = 30

type smsCollector struct {
	modem    *dwr910m.DWR910M
	seen     map[string]time.Time
	primed   bool
	lastErr  string
	badDates map[string]bool
}

func newSMSCollector(modem *dwr910m.DWR910M) *smsCollector {
	return &smsCollector{modem: modem, seen: map[string]time.Time{}, badDates: map[string]bool{}}
}

var modemDateLayouts = []string{
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"2006-01-02T15:04:05",
	"02/01/2006 15:04:05",
	"2006-01-02 15:04",
	"02/01/2006 15:04",
}

func parseModemDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range modemDateLayouts {
		if t, e := time.ParseInLocation(layout, s, time.Local); e == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func dateShape(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '0'
		}
		return r
	}, s)
}

func smsKey(m dwr910m.Message) string {
	return m.Sender + "\x00" + m.Date + "\x00" + m.Content
}

func readInbox(modem *dwr910m.DWR910M) ([]dwr910m.Message, error) {
	var all []dwr910m.Message
	for page := 1; page <= smsMaxPages; page++ {
		res, e := modem.C.Get(fmt.Sprintf("%s/PageList?pageIndex=%d", modem.Address, page))
		if e != nil {
			return nil, e
		}
		var p dwr910m.Messages
		e = json.NewDecoder(res.Body).Decode(&p)
		res.Body.Close()
		if e != nil {
			return nil, fmt.Errorf("inbox page %d: %w", page, e)
		}
		all = append(all, p.Messages...)
		if len(p.Messages) == 0 || int64(page) >= p.TotalPages {
			break
		}
	}
	return all, nil
}

func (c *smsCollector) collect(store *state.Store) {
	msgs, e := readInbox(c.modem)
	if e != nil {
		if e.Error() != c.lastErr {
			log.Printf("sms: cannot read the modem's inbox: %v", e)
			c.lastErr = e.Error()
		}
		store.Update(func(s *state.Snapshot) { s.SMS.Error = e.Error() })
		return
	}
	c.lastErr = ""

	var inbox, fresh []state.SMS
	listed := map[string]bool{}
	for _, m := range msgs {
		key := smsKey(m)
		if listed[key] {
			continue
		}
		listed[key] = true
		t, ok := parseModemDate(m.Date)
		if shape := dateShape(m.Date); !ok && !c.badDates[shape] {
			c.badDates[shape] = true
			log.Printf("sms: cannot read the modem's date %q; ordering by arrival instead", m.Date)
		}
		received, known := c.seen[key]
		if !known {
			if c.primed {
				received = time.Now()
			}
			c.seen[key] = received
		}
		sms := state.SMS{ID: m.ID, Sender: m.Sender, Time: t, ReceivedAt: received, Text: m.Content}
		inbox = append(inbox, sms)
		if !known && c.primed {
			fresh = append(fresh, sms)
		}
	}
	if !c.primed {
		log.Printf("sms: %d messages in the modem's inbox", len(inbox))
	}
	c.primed = true

	slices.SortStableFunc(inbox, func(a, b state.SMS) int {
		return cmp.Or(b.ReceivedAt.Compare(a.ReceivedAt), b.Time.Compare(a.Time), cmp.Compare(b.ID, a.ID))
	})
	if len(inbox) > smsLimit {
		inbox = inbox[:smsLimit]
	}
	store.Update(func(s *state.Snapshot) {
		s.SMS = state.SMSInbox{Available: true, Messages: inbox}
	})

	slices.SortStableFunc(fresh, func(a, b state.SMS) int { return a.Time.Compare(b.Time) })
	for _, m := range fresh {
		log.Printf("sms: new message from %s", m.Sender)
		store.Notify(state.Info, "sms", "SMS from "+m.Sender)
	}
}

func collectSystem(store *state.Store) {
	var sys state.System
	sys.Hostname, _ = os.Hostname()
	if data, e := os.ReadFile("/proc/sys/kernel/osrelease"); e == nil {
		sys.Kernel = strings.TrimSpace(string(data))
	}
	if data, e := os.ReadFile("/proc/uptime"); e == nil {
		sys.UptimeSeconds, _ = sysinfo.ParseUptime(string(data))
	}
	if data, e := os.ReadFile("/proc/loadavg"); e == nil {
		sys.Load, _ = sysinfo.ParseLoadavg(string(data))
	}
	if f, e := os.Open("/proc/meminfo"); e == nil {
		sys.MemTotal, sys.MemAvailable, _ = sysinfo.ParseMeminfo(f)
		f.Close()
	}
	if data, e := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); e == nil {
		if t, e := sysinfo.ParseMilliCelsius(string(data)); e == nil {
			sys.TemperatureC = &t
		}
	}
	store.Update(func(s *state.Snapshot) { s.System = sys })
}
