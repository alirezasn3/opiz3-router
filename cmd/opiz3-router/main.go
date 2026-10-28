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
	"opiz3-router/internal/collector"
	"opiz3-router/internal/config"
	"opiz3-router/internal/display"
	"opiz3-router/internal/led"
	"opiz3-router/internal/mnm"
	"opiz3-router/internal/netscan"
	"opiz3-router/internal/router"
	"opiz3-router/internal/setup"
	"opiz3-router/internal/state"
	"opiz3-router/internal/update"
	"opiz3-router/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func must(e error) {
	if e != nil {
		panic(e)
	}
}

// fail shows msg on the display, stops LEDs and exits so systemd restarts the service.
func fail(d *display.Display, redLED *led.LED, msg string) {
	log.Print(msg)
	d.Text(msg, true)
	d.NewLine(true)
	d.Text("restarting...", true)
	if redLED != nil {
		redLED.StopBlink()
	}
	time.Sleep(time.Second * 3)
	os.Exit(1)
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func main() {
	configPath := flag.String("config", "", "path to the JSON config file (default: config.json next to the binary)")
	install := flag.Bool("install", false, "prepare the system, install mini-net-man and the systemd service, then exit")
	uninstall := flag.Bool("uninstall", false, "remove the systemd service and exit (the system changes made by -install stay)")
	mnmBinary := flag.String("mini-net-man", "", "with -install: use this mini-net-man binary instead of downloading the latest release")
	showVersion := flag.Bool("version", false, "print the version and exit")
	doUpdate := flag.Bool("update", false, "install the latest release from GitHub, restart the service and exit")
	checkUpdate := flag.Bool("check-update", false, "report whether a newer release exists and exit")
	updateTo := flag.String("update-to", "", "install this release (such as v1.2.3) instead of the latest, which can go back")
	rollback := flag.Bool("rollback", false, "go back to the version before the last update, restart the service and exit")
	force := flag.Bool("force", false, "with -update: install even if already up to date")
	noRestart := flag.Bool("no-restart", false, "with -update or -rollback: do not restart the service")
	flag.Parse()
	if *configPath == "" {
		*configPath = config.DefaultPath()
	}

	if *showVersion {
		fmt.Println("opiz3-router", version)
		return
	}

	if *doUpdate || *checkUpdate || *updateTo != "" || *rollback {
		e := update.Run(context.Background(), update.Options{
			Current: version, Version: *updateTo, Check: *checkUpdate, Force: *force,
			Rollback: *rollback, NoRestart: *noRestart, Service: setup.ServiceName, Out: os.Stdout,
		})
		if e != nil {
			fmt.Println("update failed:", e)
			os.Exit(1)
		}
		return
	}
	log.Printf("opiz3-router %s starting", version)

	if *install {
		cfg, e := config.Load(*configPath)
		if e != nil {
			fmt.Println("config:", e)
			os.Exit(1)
		}
		in := setup.NewInstaller(cfg, *configPath, os.Stdout)
		in.MiniNetManBinary = *mnmBinary
		if e := in.Install(context.Background()); e != nil {
			fmt.Println("install failed:", e)
			os.Exit(1)
		}
		fmt.Println("\nopiz3-router is installed and running")
		for _, n := range in.Notes {
			fmt.Println(" -", n)
		}
		if in.RebootNeeded {
			fmt.Println(" - Reboot to enable the display: reboot")
		}
		return
	}
	if *uninstall {
		if e := goSystemd.DeleteService(setup.ServiceName); e != nil {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// setup onboard leds
	redLED, e := led.NewLED("/sys/class/leds/red_led")
	must(e)
	greenLED, e := led.NewLED("/sys/class/leds/green_led")
	must(e)

	must(redLED.Off())
	must(greenLED.Off())

	// blink red led while waiting for wan interface
	redLED.Blink(time.Millisecond*250, time.Millisecond*250)

	// create and setup display instance
	d, e := display.NewDisplay()
	must(e)
	must(d.Setup(cfg.Display.SPIPort, cfg.Display.ResetPin, cfg.Display.DCPin, cfg.Display.CSPin, cfg.Display.BacklightPin))

	// subscribe to netlink updates (before using netlink elsewhere)
	ch := make(chan netlink.LinkUpdate)
	subDone := make(chan struct{})
	must(netlink.LinkSubscribe(ch, subDone))
	defer close(subDone)

	usbModemConnected := false
	ethernetConnected := false

	// check ethernet interface
	if link, e := netlink.LinkByName(ifaces.Ethernet); e == nil && link.Attrs().MasterIndex != 0 {
		d.Text(ifaces.Ethernet+" is a LAN port", true)
		d.NewLine(true)
	} else if _, _, e = router.GetCIDRFromInterfaceName(ifaces.Ethernet); e != nil {
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

	if !ethernetConnected && !usbModemConnected {
		fail(d, redLED, "failed to get wan interfaces")
	}

	store := state.NewStore()

	redLED.StopBlink()
	greenLED.Blink(time.Millisecond*10, time.Second)

	// mini-net-man holds the bridge, firewall, NAT and VPN routing. It may
	// still be starting, so the syncer keeps trying.
	if cfg.MiniNetMan.Token == "" {
		log.Print("mini_net_man.token is not set; run opiz3-router -install")
	}
	syncer := mnm.NewSyncer(cfg, mnm.NewClient(cfg.MiniNetMan.URL, cfg.MiniNetMan.Token), store)
	go syncer.Run(ctx)
	d.Text("syncing with mini-net-man", true)
	d.NewLine(true)

	must(collector.Start(ctx, cfg, store))

	scanner := netscan.NewScanner(cfg, store)

	webDone := make(chan struct{})
	if cfg.Panel.Enabled {
		d.Text("panel: "+strings.TrimSuffix(cfg.Panel.Listen, ":80"), true)
		d.NewLine(true)
		switcher := router.NewNetSwitcher(cfg, store, syncer)
		go every(ctx, 10*time.Second, switcher.Publish)
		displayCtrl := display.NewController(d, store)
		handler := web.NewHandler(store, web.Options{
			Username: cfg.Panel.Username,
			Password: cfg.Panel.Password,
			Network:  switcher,
			Display:  displayCtrl,
			WiFi:     setup.NewAP(cfg, *configPath),
			Scanner:  scanner,
		})
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
	display.StartStatusScreen(ctx, d, store, cfg)
	links := router.NewLinkWatcher(cfg)

	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			greenLED.StopBlink()
			redLED.StopBlink()
			<-webDone
			return
		case update, ok := <-ch:
			if !ok {
				log.Print("netlink subscription closed")
				return
			}
			if label, status, changed := links.Update(update); changed {
				level := state.Good
				if status != "up" {
					level = state.Warning
				}
				store.Notify(level, "link", label+" "+status)
			}
		}
	}
}
