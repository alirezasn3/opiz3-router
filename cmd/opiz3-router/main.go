//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	goSystemd "github.com/alirezasn3/go-systemd"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"opiz3-router/internal/assets"
	"opiz3-router/internal/config"
	"opiz3-router/internal/state"
	"opiz3-router/internal/web"
)

const screenWidth = 240
const screenHeight = 280
const serviceName = "opiz3-router"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func must(e error) {
	if e != nil {
		panic(e)
	}
}

func installService(configPath string) error {
	exec, e := os.Executable()
	if e != nil {
		return e
	}
	execStart := exec
	if configPath != config.DefaultPath {
		execStart += " -config " + configPath
	}
	return goSystemd.CreateService(&goSystemd.Service{
		Name:        serviceName,
		Description: "Orangepi Zero 3 Router",
		ExecStart:   execStart,
		Restart:     "on-failure",
		RestartSec:  "10s",
		After:       "network-online.target",
		Wants:       "network-online.target",
		WantedBy:    "multi-user.target",
	})
}

// fail shows msg on the display, turns the LEDs off and exits so systemd
// restarts the service.
func fail(d *Display, redLED *LED, msg string) {
	log.Print(msg)
	d.Text(msg, true)
	d.NewLine(true)
	d.Text("restarting...", true)
	redLED.StopBlink()
	time.Sleep(time.Second * 3)
	os.Exit(1)
}

func main() {
	configPath := flag.String("config", config.DefaultPath, "path to the JSON config file (optional)")
	install := flag.Bool("install", false, "install the systemd service and exit")
	uninstall := flag.Bool("uninstall", false, "remove the systemd service and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("opiz3-router", version)
		return
	}
	log.Printf("opiz3-router %s starting", version)

	if *install {
		if e := installService(*configPath); e != nil {
			fmt.Println(e)
			os.Exit(1)
		}
		fmt.Println("opiz3-router service created")
		return
	}
	if *uninstall {
		if e := goSystemd.DeleteService(serviceName); e != nil {
			fmt.Println(e)
			os.Exit(1)
		}
		fmt.Println("opiz3-router service deleted")
		return
	}

	cfg, e := config.Load(*configPath)
	if e != nil {
		log.Fatalf("config: %v", e)
	}
	ifaces := cfg.Interfaces
	wgName := cfg.WireGuard.Interface

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// setup onboard leds
	redLED, e := NewLED("/sys/class/leds/red_led")
	must(e)
	greenLED, e := NewLED("/sys/class/leds/green_led")
	must(e)

	// turn leds off
	must(redLED.Off())
	must(greenLED.Off())

	// blink red led, means waiting for wan interface
	redLED.Blink(time.Millisecond*250, time.Millisecond*250)

	// create and setup display instance
	d := &Display{}
	must(
		d.Setup(cfg.Display.SPIPort, cfg.Display.ResetPin, cfg.Display.DCPin, cfg.Display.CSPin, cfg.Display.BacklightPin, func() []byte {
			data, err := assets.Files.ReadFile(assets.FontPath)
			must(err)
			return data
		}()),
	)

	// subscribe to netlink updates (should be called before using netlink elsewhere, discards changes made before this call)
	ch := make(chan netlink.LinkUpdate)
	subDone := make(chan struct{})
	must(netlink.LinkSubscribe(ch, subDone))
	defer close(subDone)

	usbModemConnected := false
	ethernetConnected := false

	// check ethernet interface; as a port of the LAN bridge it is no uplink
	if link, e := netlink.LinkByName(ifaces.Ethernet); e == nil && link.Attrs().MasterIndex != 0 {
		d.Text(ifaces.Ethernet+" is a LAN port", true)
		d.NewLine(true)
	} else if _, _, e = getCIDRFromInterfaceName(ifaces.Ethernet); e != nil {
		d.Text("failed to get "+ifaces.Ethernet, true)
		d.NewLine(true)
	} else {
		d.Text(ifaces.Ethernet+" is connected", true)
		d.NewLine(true)
		ethernetConnected = true
	}

	// check usb modem interface
	c, e := net.DialTimeout("tcp4", net.JoinHostPort(cfg.Modem.Address, "80"), time.Second*2)
	if e != nil {
		log.Println(e)
		d.Text("failed to get "+ifaces.USBModem, true)
		d.NewLine(true)
	} else {
		c.Close()
		usbModemConnected = true
		d.Text(ifaces.USBModem+" is connected", true)
		d.NewLine(true)
	}

	// check if any wan interfaces are present
	if !ethernetConnected && !usbModemConnected {
		fail(d, redLED, "failed to get wan interfaces")
	}

	// check if wireguard interface is up
	wireguardService := "wg-quick@" + wgName
	status, statusErr := goSystemd.StatusService(wireguardService)
	if statusErr != nil || status == nil || !status.Active {
		if e = goSystemd.StartService(wireguardService); e != nil {
			fail(d, redLED, "failed to start "+wireguardService)
		}
		time.Sleep(time.Second)
		status, statusErr = goSystemd.StatusService(wireguardService)
		if statusErr != nil || status == nil || !status.Active {
			fail(d, redLED, "failed to start "+wireguardService)
		}
	}

	redLED.StopBlink()
	greenLED.Blink(time.Millisecond*10, time.Second)

	panelPort := 0
	if cfg.Panel.Enabled {
		panelPort, _ = cfg.PanelPort() // validated by config.Load
	}

	// set up forwarding, masquerading, the firewall and the VPN routing
	store := state.NewStore()
	g := newGuard(cfg, panelPort, store)
	must(g.Apply())
	d.Text("routing LAN via "+wgName, true)
	d.NewLine(true)

	must(startCollectors(ctx, cfg, store))
	linkChanged := make(chan struct{}, 1)
	go g.Run(ctx, linkChanged)

	monitor := newMonitorController(cfg, store)
	go monitor.Run(ctx)
	startNetworkScan(ctx, cfg, store)

	webDone := make(chan struct{})
	if cfg.Panel.Enabled {
		d.Text("panel: "+strings.TrimSuffix(cfg.Panel.Listen, ":80"), true)
		d.NewLine(true)
		switcher := newNetSwitcher(cfg, store)
		// The mode can also change outside the panel, e.g. by hand with netplan.
		go every(ctx, 10*time.Second, switcher.Publish)
		handler := web.NewHandler(store, web.Options{Username: cfg.Panel.Username, Password: cfg.Panel.Password, Network: switcher, Display: monitor})
		go func() {
			defer close(webDone)
			if e := web.ListenAndServe(ctx, cfg.Panel.Listen, handler); e != nil {
				log.Printf("web: %v", e)
			}
		}()
	} else {
		close(webDone)
	}

	store.Notify(state.Info, "router", "Router started")
	startStatusScreen(ctx, d, store, cfg)
	links := newLinkWatcher(cfg)

	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			greenLED.StopBlink()
			redLED.StopBlink()
			// Routing and firewall rules stay in place on purpose so the kill
			// switch, if enabled, keeps protecting the LAN while the service
			// is down.
			<-webDone
			return
		case update, ok := <-ch:
			if !ok {
				log.Print("netlink subscription closed")
				return
			}
			if label, status, changed := links.update(update); changed {
				level := state.Good
				if status != "up" {
					level = state.Warning
				}
				store.Notify(level, "link", label+" "+status)
			}
			// Tell the guard; networkd may be about to remove our rules.
			select {
			case linkChanged <- struct{}{}:
			default:
			}
		}
	}
}

// linkWatcher turns netlink updates into up/down changes of the router's own
// interfaces. Netlink sends several updates for one change (flags, carrier,
// operational state), so only a change of the summarised status counts.
type linkWatcher struct {
	cfg    config.Config
	status map[string]string
}

func newLinkWatcher(cfg config.Config) *linkWatcher {
	w := &linkWatcher{cfg: cfg, status: map[string]string{}}
	links, _ := netlink.LinkList()
	for _, l := range links {
		w.status[l.Attrs().Name] = linkStatus(l.Attrs(), false)
	}
	return w
}

// update returns the link's label and new status, and whether it changed.
func (w *linkWatcher) update(u netlink.LinkUpdate) (string, string, bool) {
	a := u.Attrs()
	label := linkLabel(w.cfg, a.Name)
	if label == a.Name {
		return "", "", false // not one of the router's interfaces
	}
	status := linkStatus(a, u.Header.Type == unix.RTM_DELLINK)
	if w.status[a.Name] == status {
		return "", "", false
	}
	w.status[a.Name] = status
	return label, status, true
}

func linkStatus(a *netlink.LinkAttrs, removed bool) string {
	switch {
	case removed:
		return "removed"
	// Tunnels such as WireGuard report an "unknown" operational state.
	case a.Flags&net.FlagUp != 0 && (a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown):
		return "up"
	}
	return "down"
}

// linkLabel names an interface the way the status screen does.
func linkLabel(cfg config.Config, name string) string {
	switch name {
	case cfg.Interfaces.Ethernet:
		return "ETH"
	case cfg.Interfaces.USBModem:
		return "4G"
	case cfg.Interfaces.Wireless:
		return "WIFI"
	case cfg.WireGuard.Interface:
		return "VPN"
	case cfg.Interfaces.Bridge:
		return "BRIDGE"
	}
	return name
}
