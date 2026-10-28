# opiz3-router

A VPN router for the Orange Pi Zero 3. Clients on the LAN bridge (`br0`: the
Wi-Fi access point, plus `end0` in server mode) are routed through a WireGuard
tunnel (`wg0`) that runs over Ethernet or a D-Link DWR-910M 4G modem. Status is
shown on an ST7789 SPI display, the onboard LEDs and a web panel.

It runs on the stock Orange Pi Ubuntu image: `opiz3-router -install` makes
every change the image needs. The bridge, firewall, NAT and VPN routing are
held by [mini-net-man](https://github.com/alirezasn3/mini-net-man), which the
router installs and configures through its HTTP API.

## Build

You need Go 1.25 or newer on your computer. The binary is static, so nothing
else is needed on the device.

```sh
make build      # CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ... -o opiz3-router ./cmd/opiz3-router
make test
```

`make build` stamps the git tag into the binary; check it with `opiz3-router -version`.

## Install on a device

Start from an unmodified Orange Pi Zero 3 Ubuntu image, log in over SSH
through Ethernet and run the commands on the device as root.

### 1. Put your WireGuard config in place

Put your tunnel config at `/etc/wireguard/wg0.conf`.
- Add `Table = off` to the `[Interface]` section: mini-net-man adds the routes
  for LAN traffic itself.
- Add `PersistentKeepalive = 25` to the `[Peer]` section: this prevents 4G/LTE
  mobile carriers and NAT routers from dropping the connection during quiet
  periods.

### 2. Install

Download the binary from the [latest release](https://github.com/alirezasn3/opiz3-router/releases/latest) into its own folder (the config is created beside it), then run `-install` as root. That is the whole setup:

```sh
mkdir -p ~/opiz3-router && cd ~/opiz3-router
curl -fL -o opiz3-router https://github.com/alirezasn3/opiz3-router/releases/latest/download/opiz3-router-linux-arm64
chmod +x opiz3-router
sudo ./opiz3-router -install
```

Each release also has a `checksums.txt` to verify the download against
(`sha256sum -c`). To build from source instead, run `make build` on your
computer and copy `opiz3-router` over.

`-install` creates `config.json` **next to the binary** (unless you pass
`-config`) and puts mini-net-man in `mini-net-man` next to that folder, so you
end up with `~/opiz3-router` and `~/mini-net-man`, each with its own config.
The service runs the binary in place, so keep it where it is; if you move it,
run `-install` again.

`-install` needs the internet, can be run again at any time and only changes
what is not already right. It:

| Step | What it does |
|---|---|
| Config | Creates `config.json` next to the binary and, if there is none, a random Wi-Fi passphrase, which it prints at the end. |
| Packages | `apt install`s whichever of `wireguard-tools hostapd dnsmasq iw netplan.io nftables` is missing. APT sources are left alone, and no service starts before it is configured. |
| Display | Adds `overlay_prefix=sun50i-h616` and `overlays=spi1-cs0-spidev` to `/boot/orangepiEnv.txt`, keeping the other lines. This needs a reboot. |
| Wi-Fi and DHCP | Writes `/etc/hostapd/hostapd.conf` (access point `wlan0`, bridged to `br0`) and `/etc/dnsmasq.d/br0.conf` (DHCP on `br0`), and enables both services. They wait for `br0`, which mini-net-man creates, and retry if it is late. |
| WireGuard | Enables `wg-quick@wg0` and has systemd retry it if it fails at boot. |
| mini-net-man | Downloads the latest release for the CPU into `~/mini-net-man` (its `config.yaml` is created beside it), checks it against the release's `checksums.txt`, installs its service, and creates an API token for the router, which it stores in `config.json`. |
| Network | Moves the image's own files out of `/etc/netplan` (to `netplan-backup/` next to the binary), writes the small fallback `/etc/netplan/opiz3-router.yaml` (DHCP on `end0` and the modem), masks NetworkManager, and lets systemd-networkd take over. The SSH session may drop for a moment. |
| Service | Installs and starts the `opiz3-router` service, after mini-net-man. |

Then reboot if it says so, and check that `/dev/spidev1.0` exists.

Options:

- `-config /path/to/config.json` for a config that is not next to the binary.
- `-mini-net-man /path/to/binary` to use a binary you already copied instead
  of downloading.

If mini-net-man was already installed before, its generated login is not shown
again, so `-install` cannot make the token. It says so; make a token in
mini-net-man's panel (Settings, API tokens) and put it in `config.json` as
`mini_net_man.token`.

The 4G modem shows up as `enx<MAC>`. Plug it in and run `ip link` to see its
name. If it is not `enxd236db4ff68a`, set `interfaces.usb_modem` in the config
and run `-install` again.

### 3. Check it

```sh
journalctl -u opiz3-router -f            # logs
journalctl -u mini-net-man -f
```

The screen shows the startup steps and then the status screen (see below). The
panel is at `http://172.16.0.1/` from a device on the router's Wi-Fi.

`-uninstall` removes the `opiz3-router` service. It leaves what `-install` set
up: mini-net-man keeps holding the bridge, firewall and routing.

## Updating

From the folder the binary is in:

```sh
cd ~/opiz3-router
./opiz3-router -check-update       # is there a newer release?
sudo ./opiz3-router -update        # install the latest release and restart the service
sudo ./opiz3-router -update-to v1.0.1   # install that release; it may be older
sudo ./opiz3-router -rollback      # go back to the version before the last update
```

`-update` downloads the release's binary, checks it against `checksums.txt`,
test-runs it (it has to report the release's version), and only then swaps it
in. The previous binary stays next to it as `opiz3-router.old`, which is what
`-rollback` uses. `-force` installs even when the version is already current,
or when the running build is a local one that has no release version, and
`-no-restart` leaves the restart to you. The update replaces the binary in
its own folder, and the restart needs root.

Updating replaces the program only. If a release's notes mention changes to the
system setup, run `sudo ./opiz3-router -install` once more.

To try a build from your computer without a release: `make deploy HOST=root@<device-ip>`
(it goes to `~/opiz3-router` of the user you log in as; `DIR=` changes the folder).

## Releases

Releases are built by GitHub Actions. To publish one, from a clean checkout of
the commit to release:

```sh
make release TAG=v1.0.1
```

This runs the tests, tags the commit and pushes the tag. The
[release workflow](.github/workflows/release.yml) then tests it again, builds
`opiz3-router-linux-arm64` with the tag as its version, and publishes a GitHub
release with that binary and `checksums.txt` attached and notes
generated from the commits. A tag with a dash (`v1.0.1-rc1`) is published as a
pre-release, which `-update` does not pick as the latest.
The asset names and `checksums.txt` are what `-update` looks for, so keep them
as they are. The repository has to be public for it to download without a
login.

## Config

Every setting has a default that matches the reference hardware, and
`deploy/config.example.json` lists them all. `config.json` next to the binary
(`~/opiz3-router/config.json`) only needs the values you change. For example, to turn off the kill switch and
put a password on the panel:

```json
{
  "wireguard": { "kill_switch": false },
  "panel": { "password": "choose-one" }
}
```

The file holds the Wi-Fi passphrase and the mini-net-man token, so it is only
readable by root. The panel's Wi-Fi card rewrites it (with every setting
spelled out).

## Status screen

After startup the display shows:

- **Banner:** the VPN state. Green `PROTECTED`; yellow `NO HANDSHAKE` (tunnel
  up but no recent handshake) or `VPN DOWN - BLOCKED` (kill switch holding LAN
  traffic); red `UNPROTECTED` (VPN down, kill switch off) or `NOT ENFORCED`
  (mini-net-man is not holding the router's setup).
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

## Network discovery (Ethernet uplink)

The router discovers devices on the IPv4 subnet connected through `interfaces.ethernet`
(`end0` by default). It sends raw ARP requests on the Ethernet interface and
collects replies with their MAC addresses. Names are resolved using:
dnsmasq DHCP leases, reverse DNS, mDNS, LLMNR, and NetBIOS name service. If no
name is available, it falls back to the MAC manufacturer from the Wireshark `manuf` database embedded
in the binary. Discovered devices are displayed live in the web panel's **Network discovery** card.

## LCD status display & power control

The ST7789 display is dedicated to a clean, focused **Status screen**:
- Top banner: VPN protection status (Protected, Blocked, Unprotected, Leaking)
- Uplinks: Ethernet (`ETH`) and 4G modem (`4G`) with signal meter
- WireGuard tunnel (`VPN`): live throughput and handshake health
- LAN clients (`BRIDGE`): online client count
- Footer: recent notification or SMS alert

The display backlight and rendering can be turned off or on directly from the web panel's **LCD display** card to save power when the router is running unattended.

## Wi-Fi access point

The panel's **Wi-Fi access point** card changes the network name, passphrase and
channel, and the range of addresses dnsmasq hands out. Saving writes
`config.json`, rewrites `hostapd.conf` and `br0.conf` and restarts what
changed. Wi-Fi devices, including the one you use the panel from, reconnect
within a few seconds. If a service does not restart with the new settings, the
old ones are put back. Leave the passphrase empty to keep it.

## Switching the Ethernet port

The panel's **Ethernet port** card switches `end0` between an **internet
uplink** (DHCP client, next to the 4G modem) and a **LAN port** on `br0`
(devices plugged in join the LAN and go through the VPN; the 4G modem is the
only uplink). The Wi-Fi access point stays as it is.

- The router writes `network.netplan_file` (`/etc/netplan/opiz3-router.yaml`)
  for the chosen mode (DHCP on `end0`, or nothing), runs `netplan apply`, and
  has mini-net-man add `end0` to `br0` or take it out. The port leaves the
  bridge before it asks for an address and joins it only after netplan has let
  go. If a step fails, the previous setup is put back. mini-net-man remembers
  the mode, so it survives a reboot.
- Like `netplan try`, the switch must be kept within 90 seconds with the
  **Keep** button, or it is undone. If the switch cuts off the device you made
  it from (for example a laptop on the Ethernet port), reconnect over Wi-Fi to
  keep it, or wait and it goes back by itself.
- Switching is only offered when `/etc/netplan` holds no other `.yaml` files,
  because netplan merges all of them and another file could undo the switch.
  The panel says which files are in the way. `-install` moves the image's own
  files out of the way.
- While switching from uplink to LAN port, the VPN moves to the 4G modem; the
  tunnel may pause for a few seconds.
- Changes are only accepted from the panel page itself, opened by IP address,
  so other websites cannot switch the port through your browser. Anyone on the
  LAN who can open the panel can switch it; set `panel.password` to prevent
  that.

## Routing and firewall

The router describes what it needs to mini-net-man and keeps it there. Every
item it manages has a comment starting with `opiz3-router:` in mini-net-man's
panel; items without it are never touched. The router compares the two every
30 seconds and puts back anything that was removed or edited. mini-net-man
itself keeps its config in the kernel across restarts of the router, so all of
this stays in place when the `opiz3-router` service stops.

- **Bridge:** `br0` with the address `network.bridge_address`. `end0` is a port
  of it in LAN-port mode; `wlan0` is joined by hostapd.
- **Forwarding and tuning:** IP forwarding is on, and BBR (with `fq`, when the
  kernel offers it) is the congestion control.
- **VPN routing:** traffic arriving on `br0` is looked up in table 200 (rule
  `iif br0 lookup 200`). The router's own replies to LAN clients stay in the
  main table, so SSH and the panel work from the LAN. Table 200 has
  `default dev wg0`.
- **Kill switch** (`wireguard.kill_switch`, on by default): table 200 also has
  `blackhole default metric 4096`, so if `wg0` goes away LAN traffic is dropped
  instead of going out of the WAN unencrypted.
- **Health check:** mini-net-man pings `wireguard.health_target` (`1.1.1.1`)
  through `wg0`. The result shows in the panel and on the screen. With the kill
  switch off, the VPN rule is tied to this check: while it fails, the rule is
  taken out and LAN traffic uses the WAN directly until the tunnel is back.
- **Firewall:** the panel ports (ours, and mini-net-man's) are open on
  `panel.allowed_interfaces` and loopback only. New connections into the LAN
  that did not start there are dropped. Traffic leaving `end0`, the modem and
  `wg0` is masqueraded, and TCP MSS is clamped to the path MTU.
- A port forward (DNAT) into the LAN has to be added above the router's
  `forward-to-lan` drop rule in mini-net-man's panel, or it is dropped too.
- Nothing restarts `wg-quick` any more. systemd retries it if it fails, and
  `PersistentKeepalive` brings the tunnel back after the uplink changes.

mini-net-man's own panel is at `http://172.16.0.1:8080/` from the LAN (the
login was printed by `-install`; change it with
`~/mini-net-man/mini-net-man passwd admin`). Check the setup with
`ip rule`, `ip route show table 200` and `nft list table inet mini-net-man`.

## Web panel

The panel listens on `http://172.16.0.1/` by default and is only reachable from
the LAN. It shows VPN protection state, live tunnel throughput, uplinks, the
4G modem, SMS messages from the modem's inbox, a log of the last 100
notifications, connected clients, whether mini-net-man holds the router's
setup, and system health. It also changes the Wi-Fi settings and the Ethernet
port mode. Everything else about the network is in mini-net-man's panel.

The notification log covers link changes of the router's interfaces, VPN state
changes, new SMS, and losing or regaining contact with mini-net-man. It is kept in memory, so it starts
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

The embedded MAC-vendor database (`internal/netscan/manuf`) is derived from the
[Wireshark](https://www.wireshark.org/) `manuf` file, which combines the public
IEEE OUI registry with Wireshark's own additions, and is used only to show a
device's likely manufacturer. If you redistribute your own build, keep this
attribution; Wireshark's data carries its own license terms.
