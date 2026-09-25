# opiz3-router

A VPN router for the Orange Pi Zero 3. Clients on the LAN bridge (`br0`: the
Wi-Fi access point, plus `end0` in server mode) are routed through a WireGuard
tunnel (`wg0`) that runs over Ethernet or a D-Link DWR-910M 4G modem. Status is
shown on an ST7789 SPI display, the onboard LEDs and a web panel.

## Build

You need Go 1.25 or newer on your computer. The binary is static, so nothing
else is needed on the device.

```sh
make build      # CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ... -o opiz3-router ./cmd/opiz3-router
make test
```

`make build` stamps the git tag into the binary; check it with `opiz3-router -version`.

## Prepare the Orange Pi Ubuntu image

The two `orangepi-image-console*.sh` scripts let you edit an official Orange
Pi Zero 3 Ubuntu image from your computer through a Linux VM. The image file
must be accessible at the path you pass to the script from inside that VM.
The computer needs Bash, `ssh` and `scp`; the VM needs Linux, `sudo`, loop
device and mount support, and an SSH server.

From the directory containing the scripts, run:

```sh
./orangepi-image-console.sh -h <vm-host> -u <vm-user> -i <image-path-in-vm>
```

The wrapper copies `orangepi-image-console-remote.sh` to the VM, opens an
interactive SSH session with a terminal, and runs the helper with `sudo` (enter
the VM's sudo password when prompted). The helper attaches the image with a
loop device, mounts its root partition (partition 2 by default) at
`/mnt/orangepi-image`, optionally mounts a separate boot partition there, and
opens an interactive Bash shell inside the image with `chroot`. If the selected
root partition is not present, it falls back to the first partition it finds.
If ARM64 chroot support is missing, the helper installs it in the VM using
`apt-get`; it also sets up the chroot's device, proc, sys, run and DNS mounts.
It copies the QEMU emulator into the image and copies the VM's resolver
configuration to the image's `/etc/resolv.conf` so commands in the chroot can
use the network. On exit, it normally unmounts the image and detaches the loop
device. Use `-r <partition-number>` to select a different root partition,
`-b <partition-number>` for a separate boot partition, `-p <ssh-port>` for a
non-default SSH port, or `-k` to keep the mounts and loop device after exiting.
The helper mounts the image and provides a shell; it does not apply the
configuration below automatically.

The image preparation performed in that shell was:

- Changed the APT source configuration to use Ubuntu's default source, then
  ran `apt update` and installed WireGuard.
- Enabled BBR congestion control and IPv4 forwarding.
- Disabled NetworkManager.
- Added `/etc/dnsmasq.d/br0.conf` and `/etc/hostapd/hostapd.conf`.
- Added the required overlays to `/boot/orangepiEnv.txt`.
- Removed the default netplan file and added
  `/etc/netplan/opiz3-router.yaml`.

The checked-in examples for these router settings are under `deploy/`. In
particular, replace the example Wi-Fi credentials in the hostapd configuration
before using it on a device.

## Set up a new device

These steps assume an Orange Pi Ubuntu or Debian image (or Armbian), with you
logged in over SSH through Ethernet. Run the commands on the device as root.

### 1. Enable the SPI display

Add these two lines to `/boot/orangepiEnv.txt` (they are also in
`deploy/boot/orangepi-env.txt`). Edit the file; do not replace it, because it
holds the root filesystem UUID of your board. If there is already an
`overlays=` line, add `spi1-cs0-spidev` to it, separated by a space.

```
overlay_prefix=sun50i-h616
overlays=spi1-cs0-spidev
```

Reboot, then check that `/dev/spidev1.0` exists.

### 2. Install packages

```sh
apt update
apt install wireguard-tools hostapd dnsmasq iptables iw netplan.io
```

### 3. Network (netplan)

Pick one file from `deploy/netplan/` and copy it to
`/etc/netplan/opiz3-router.yaml`. Move any other files in `/etc/netplan/` out of
the way. With an `ap` file, the Ethernet port can later be switched between an
uplink and a LAN port from the panel (see "Switching the Ethernet port" below),
which needs the router's file to be the only one there.

| File | end0 (Ethernet) | wlan0 (Wi-Fi) |
|---|---|---|
| `end0-client-wlan0-ap.yaml` | uplink (DHCP client) | access point |
| `end0-server-wlan0-ap.yaml` | LAN port on `br0` | access point |
| `end0-client-wlan0-sta.yaml` | uplink | joins another Wi-Fi |
| `end0-server-wlan0-sta.yaml` | LAN port on `br0` | joins another Wi-Fi |

- The 4G modem shows up as `enx<MAC>`. Plug it in and run `ip link` to see
  its name. If it is not `enxd236db4ff68a`, change it in the netplan file and
  set `interfaces.usb_modem` in the config (step 7).
- For the `sta` files, fill in `YOUR_WIFI_SSID` and `YOUR_WIFI_PASSWORD`.
- The files use systemd-networkd. If your image manages `end0` with
  NetworkManager, disable NetworkManager first.

Stop systemd-networkd from deleting the router's routing rules. Without this,
plugging in the modem (or any other link change) removes the VPN routing
until the router notices and restores it a few seconds later:

```sh
mkdir -p /etc/systemd/networkd.conf.d
cp deploy/systemd/networkd-opiz3-router.conf /etc/systemd/networkd.conf.d/opiz3-router.conf
systemctl restart systemd-networkd
```

Apply with `netplan try`: it rolls the change back after two minutes unless you
confirm, so a mistake can't lock you out. It may warn that bridge changes
can't be fully reverted. Having a serial console or keyboard and HDMI at hand
the first time helps.

#### Using the Ethernet port for LAN devices

With an `end0-server-*` file, `end0` becomes a port of `br0` instead of an
uplink. Devices plugged into it (directly or through a switch) are on the same
LAN as the Wi-Fi clients: they get a `172.16.0.x` address from dnsmasq, go
through the VPN and kill switch, and can open the panel. The 4G modem is then
the only uplink: until the modem answers, the router restarts every 10 seconds
waiting for it. The screen shows `ETH` as `LAN port` (gray while nothing is
plugged in).

Your SSH session over `end0` drops when you apply the change, because `end0`
loses its own address. Connect to the router's Wi-Fi first and SSH to
`172.16.0.1`, or use a serial console.

### 4. Wi-Fi access point (hostapd)

Skip this step with the `sta` netplan files.

```sh
cp deploy/hostapd/hostapd.conf /etc/hostapd/hostapd.conf
# set ssid= and wpa_passphrase= (8-63 characters)
systemctl unmask hostapd
systemctl enable --now hostapd
```

### 5. DHCP for the LAN (dnsmasq)

```sh
cp deploy/dnsmasq/br0.conf /etc/dnsmasq.d/br0.conf
systemctl enable --now dnsmasq
```

This hands out `172.16.0.2`–`172.16.0.99` on `br0`, with `1.1.1.1` and
`8.8.8.8` as DNS servers. dnsmasq's own DNS server is turned off.

### 6. WireGuard

Put your tunnel config at `/etc/wireguard/wg0.conf`. Add `Table = off` to the
`[Interface]` section: the router adds the routes for LAN traffic itself.
You don't need to enable `wg-quick@wg0`; the router starts it.

### 7. Config (optional)

Every setting has a default that matches the reference hardware. To change
some, copy `deploy/config.example.json` to `/etc/opiz3-router/config.json` and
keep only the values you change. For example, to turn off the kill switch and
put a password on the panel:

```json
{
  "wireguard": { "kill_switch": false },
  "panel": { "password": "choose-one" }
}
```

### 8. Copy the binary and run it

On your computer:

```sh
make build
scp opiz3-router root@<device-ip>:/usr/local/bin/
```

On the device, run it in the foreground first to check that everything works
(stop it with Ctrl-C):

```sh
/usr/local/bin/opiz3-router
```

The screen shows the startup steps and then the status screen (see below). The
panel is at `http://172.16.0.1/`
from a device on the router's Wi-Fi. Then install it as a service, which also
enables and starts it:

```sh
/usr/local/bin/opiz3-router -install     # add -config /path/to/config.json for a non-default path
journalctl -u opiz3-router -f            # logs
```

To update later, from your computer:

```sh
make deploy HOST=root@<device-ip>
```

`-uninstall` removes the service.

## Status screen

After startup the display shows:

- **Banner:** the VPN state. Green `PROTECTED`; yellow `NO HANDSHAKE` (tunnel
  up but no recent handshake) or `VPN DOWN - BLOCKED` (kill switch holding LAN
  traffic); red `UNPROTECTED` (VPN down, kill switch off), `LEAKING` or
  `NOT ROUTED`.
- **One row per link** with a colored dot: green is working, yellow needs
  attention, red is down, gray is off on purpose (unplugged modem, idle LAN port).
  - `ETH`: address, or `no cable`. In server mode it is a LAN port.
  - `4G`: network type, signal in dBm and a 5-bar signal meter, `no response`
    or `unplugged`.
  - `VPN`: bytes received and sent through `wg0` since it came up,
    `no handshake` or `down`.
  - `BRIDGE`: the LAN bridge (Wi-Fi access point and, in server mode, the
    Ethernet port), with the number of clients online.
- **Footer:** the latest notification with a colored dot, such as a link
  change, a VPN state change or `SMS from ...` (SMS stay for 5 minutes, others
  for 30 seconds), otherwise the panel address.

## Network scan (Ethernet uplink)

The display can scan the IPv4 subnet connected through `interfaces.ethernet`
(`end0` by default). It sends raw ARP requests on the Ethernet interface and
collects replies with their MAC addresses. Names are tried in this order:
dnsmasq DHCP leases, reverse DNS, mDNS, LLMNR, and NetBIOS name service. If no
name is available, the display falls back to the MAC manufacturer from the Wireshark `manuf` database embedded
in the binary. The scan page can be selected in the panel's
**Display pages & network discovery** card; the selection is remembered with
the other display pages.

## Wi-Fi channels (USB Wi-Fi adapter)

A USB Wi-Fi adapter that supports monitor mode (`monitor.interface`, default
`wlx10feed10c013`) shows how busy the 2.4 GHz channels are, so you can pick
the best one for the access point.

- While the adapter is plugged in, the router puts it into monitor mode and
  moves it through channels 1 to 13, listening on each for
  `monitor.hop_interval_ms` (default 1000 ms, so a full pass takes about 13 s;
  100 to 5000 allowed). Longer hears more beacons per visit; shorter notices
  new networks sooner. Networks are forgotten 90 s after their last beacon. It is handed back in managed
  mode when unplugged or when the router stops. `iw` must be installed, and
  nothing else (NetworkManager, wpa_supplicant) may manage the adapter.
- Only beacons are read: each network's name, channel and signal. A kernel
  filter drops all other traffic, including anything sent by phones and
  laptops, before it reaches the router.
- The panel's **Display & Wi-Fi channels** card chooses what the display
  shows: the router status or the channels page. The choice is remembered in
  `/var/lib/opiz3-router/display-page`. The card also shows the channel chart
  and the networks heard.
- The channels page shows the 2.4 GHz spectrum like a Wi-Fi analyzer: each
  network is a hump over the 22 MHz it occupies (its channel and two on either
  side), as high as its signal is strong, against -40/-60/-80 dBm gridlines.
  Your access point is blue and labelled "you"; the three strongest neighbours
  get their own colors and names; the rest are gray. Under the axis, the best
  channel's number is green, yours is blue, and a marker shows the channel
  being listened to right now. The recommended channel is the least busy of
  1, 6 and 11, the three that do not overlap. Notifications such as a new SMS
  still show in the footer.
- To use the recommendation, set `channel=` in `/etc/hostapd/hostapd.conf` and
  run `systemctl restart hostapd`.

## Switching the Ethernet port

The panel's **Ethernet port** card switches `end0` between an **internet
uplink** (DHCP client, next to the 4G modem) and a **LAN port** on `br0`
(devices plugged in join the LAN and go through the VPN; the 4G modem is the
only uplink). The Wi-Fi access point stays as it is.

- The router writes `network.netplan_file` (`/etc/netplan/opiz3-router.yaml`)
  for the chosen mode, checks it with `netplan generate`, and runs
  `netplan apply`. If that fails, the previous file is put back.
- Like `netplan try`, the switch must be kept within 90 seconds with the
  **Keep** button, or it is undone. If the switch cuts off the device you made
  it from (for example a laptop on the Ethernet port), reconnect over Wi-Fi to
  keep it, or wait and it goes back by itself.
- Switching is only offered when `/etc/netplan` holds no other `.yaml` files,
  because netplan merges all of them and another file could undo the switch.
  The panel says which files are in the way. On an existing setup, rename the
  file you installed to `opiz3-router.yaml`.
- While switching from uplink to LAN port, the VPN moves to the 4G modem; the
  tunnel may pause for a few seconds.
- Changes are only accepted from the panel page itself, opened by IP address,
  so other websites cannot switch the port through your browser. Anyone on the
  LAN who can open the panel can switch it; set `panel.password` to prevent
  that.

## Routing and firewall

- Traffic arriving on `br0` is looked up in table 200 (`ip rule iif br0 lookup 200`).
  The router's own replies to LAN clients stay in the main table, so SSH and
  the panel work from the LAN.
- Table 200 has `default dev wg0`. With `wireguard.kill_switch` on (the
  default) it also has `blackhole default metric 4096`: if `wg0` goes away, LAN
  traffic is dropped instead of going out of the WAN unencrypted. With it off,
  LAN traffic uses the WAN directly until the tunnel is back.
- The `OPIZ3-INPUT` chain only allows the panel port on `panel.allowed_interfaces`
  (and loopback). The `OPIZ3-FORWARD` chain drops new connections into the LAN
  that did not start there, except port forwards (DNAT).
- Traffic leaving every uplink (`end0`, the modem, `wlan0`), `wg0` and `br0` is
  masqueraded.
- The router checks the routing every 3 seconds, the firewall every 15 seconds
  and both 2 seconds after any link change, and puts back anything that is
  missing. The panel shows how often that happened and why; the log says
  `guard: missing ...; restoring`.
- These rules stay in place when the service stops, so the kill switch keeps
  working. Check them with `ip rule`, `ip route show table 200` and
  `iptables -S OPIZ3-INPUT`.

## Web panel

The panel listens on `http://172.16.0.1/` by default and is only reachable from
the LAN. It shows VPN protection state, live tunnel throughput, uplinks, the
4G modem, SMS messages from the modem's inbox, a log of the last 100
notifications, connected clients, routing and firewall checks, and system
health.

The notification log covers link changes of the router's interfaces, VPN state
changes (logged once a state has lasted 10 seconds), new SMS, and routing or
firewall rules that had to be restored. It is kept in memory, so it starts
empty when the router restarts. SMS already in the inbox at startup are not
reported as new.

The whole modem inbox is read every 20 seconds and messages are recognised by
sender, date and text. The log shows `sms: N messages in the modem's inbox` at
startup, `sms: new message from ...` for each new one, and
`sms: cannot read the modem's inbox: ...` if reading fails.

Set `panel.password` in the config to require HTTP basic auth (user
`panel.username`, default `admin`).

API: `GET /api/status` returns the current status as JSON and
`GET /api/events` streams it as Server-Sent Events.

## License

opiz3-router is released under the MIT License; see [LICENSE](LICENSE).

The embedded MAC-vendor database (`internal/assets/manuf`) is derived from the
[Wireshark](https://www.wireshark.org/) `manuf` file, which combines the public
IEEE OUI registry with Wireshark's own additions, and is used only to show a
device's likely manufacturer. If you redistribute your own build, keep this
attribution; Wireshark's data carries its own license terms.
