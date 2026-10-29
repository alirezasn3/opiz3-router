"use strict";

// Everything that comes from the router (DHCP hostnames in particular) is
// untrusted, so the DOM is only ever built with textContent, never innerHTML.

const HANDSHAKE_STALE_SECONDS = 180;
const $ = (id) => document.getElementById(id);

function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c == null) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

// ---- formatting ----

function fmtBytes(b) {
  if (!Number.isFinite(b)) return "–";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return (i === 0 ? b.toFixed(0) : b.toFixed(b < 10 ? 2 : 1)) + " " + units[i];
}

// Rates arrive in bytes/s and are shown in bits/s, the way links are sold.
function fmtRate(bytesPerSec) {
  if (!Number.isFinite(bytesPerSec)) return "–";
  let v = bytesPerSec * 8;
  const units = ["b/s", "kb/s", "Mb/s", "Gb/s"];
  let i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  return (i === 0 ? v.toFixed(0) : v.toFixed(v < 10 ? 1 : 0)) + " " + units[i];
}

function fmtDuration(sec) {
  if (!Number.isFinite(sec) || sec < 0) return "–";
  sec = Math.floor(sec);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function parseTime(s) {
  const t = Date.parse(s);
  // Go's zero time (year 1) means "never".
  return Number.isFinite(t) && t > Date.UTC(2000, 0, 1) ? t : null;
}

function fmtAgo(t, now) {
  if (t == null) return "never";
  const sec = (now - t) / 1000;
  return sec < 2 ? "just now" : fmtDuration(sec) + " ago";
}

function fmtClock(t) {
  return new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function statusBadge(level, label) {
  const icons = { good: "✓", warning: "!", critical: "✕", neutral: "–", info: "i" };
  return el("span", { class: "status", "data-level": level },
    el("span", { class: "status-icon", "aria-hidden": "true", text: icons[level] }),
    el("span", { text: label }));
}

function fillFacts(dl, rows) {
  dl.replaceChildren(...rows.filter(Boolean).flatMap(([k, v]) => [
    el("dt", { text: k }),
    el("dd", {}, v instanceof Node ? v : String(v ?? "–")),
  ]));
}

function emptyRow(cols, text) {
  return el("tr", { class: "empty-row" }, el("td", { colspan: String(cols), text }));
}

// ---- rendering ----

let latest = null;

function render(snap) {
  latest = snap;
  const now = parseTime(snap.time) ?? Date.now();
  const hostEl = $("hostname");
  if (hostEl) hostEl.textContent = snap.system.hostname || "";
  renderVPN(snap, now);
  renderWAN(snap);
  renderClients(snap, now);
  renderSystem(snap);
  renderWireGuard(snap, now);
  renderModem(snap);
  renderRouting(snap);
  renderRules(snap);
  renderUpdate(snap, now);
  renderInterfaces(snap);
  renderSMS(snap, now);
  renderLog(snap, now);
  renderEthernet(snap);
  renderNetworkScan(snap, now);
  renderDisplay(snap);
}

function vpnState(snap, now) {
  const wg = snap.wireguard, r = snap.routing;
  if (!r.synced) {
    return ["warning", "Not enforced", r.error || "Waiting for mini-net-man to take over the routing and firewall."];
  }
  const peer = wg.peers[0];
  const hs = peer ? parseTime(peer.last_handshake) : null;
  const fresh = hs != null && (now - hs) / 1000 < HANDSHAKE_STALE_SECONDS;
  if (r.vpn === "down" || !wg.up) {
    return r.kill_switch
      ? ["warning", "Blocked", "The VPN is down. LAN traffic is blocked to prevent leaks."]
      : ["critical", "Unprotected", "The VPN is down. LAN clients are using the normal internet."];
  }
  if (fresh) return ["good", "Protected", `Last handshake ${fmtAgo(hs, now)}.`];
  return ["warning", "No recent handshake", `Last handshake ${fmtAgo(hs, now)}. Traffic stays in the tunnel but may not get through.`];
}

function renderVPN(snap, now) {
  const [level, label, sub] = vpnState(snap, now);
  $("vpn-status").replaceWith(Object.assign(statusBadge(level, label), { id: "vpn-status" }));
  $("vpn-sub").textContent = sub;
}

function renderWAN(snap) {
  const labels = { "wan-ethernet": "Ethernet", "wan-4g": "4G modem", "wan-wifi": "Wi-Fi" };
  const items = snap.interfaces.filter((i) => labels[i.role]).map((i) => {
    let up = i.up;
    if (i.role === "wan-4g") up = up && snap.modem.reachable;
    return el("li", {},
      el("span", {}, labels[i.role] + " ", el("span", { class: "muted mono", text: i.name })),
      statusBadge(up ? "good" : "critical", up ? "Up" : "Down"));
  });
  $("wan-list").replaceChildren(...(items.length ? items : [el("li", { class: "muted", text: "No data yet" })]));
}

function renderClients(snap, now) {
  const online = snap.clients.filter((c) => c.online);
  const wifi = online.filter((c) => c.wireless).length;
  $("clients-count").textContent = String(online.length);
  $("clients-sub").textContent = snap.wireless_info
    ? `${wifi} on Wi-Fi, ${online.length - wifi} wired · ${snap.clients.length} known`
    : `${snap.clients.length} known`;

  const rows = snap.clients.map((c) => {
    const expiry = parseTime(c.lease_expiry);
    return el("tr", {},
      el("td", {}, clientName(c)),
      el("td", { class: "mono", text: c.ip || "–" }),
      el("td", { class: "mono", text: c.mac }),
      el("td", { text: !snap.wireless_info ? "–" : c.wireless ? "Wi-Fi" : "Wired" }),
      el("td", { class: "num", text: c.wireless && c.signal ? `${c.signal} dBm` : "–" }),
      el("td", { class: "num", text: c.connected_seconds ? fmtDuration(c.connected_seconds) : "–" }),
      el("td", { class: "num", text: expiry == null ? (c.ip ? "never" : "–") : "in " + fmtDuration((expiry - now) / 1000) }));
  });
  $("clients-table").replaceChildren(...(rows.length ? rows : [emptyRow(7, "No clients yet")]));
}

function clientName(c) {
  const badge = statusBadge(c.online ? "good" : "neutral", c.hostname || "Unknown device");
  badge.classList.add("plain");
  badge.title = c.online ? "Online" : "Offline";
  badge.firstElementChild.setAttribute("aria-label", badge.title);
  badge.firstElementChild.removeAttribute("aria-hidden");
  badge.firstElementChild.setAttribute("role", "img");
  return badge;
}

function renderSystem(snap) {
  const s = snap.system;
  $("uptime").textContent = fmtDuration(s.uptime_seconds);
  $("uptime-sub").textContent = s.temperature_c != null ? `SoC at ${s.temperature_c.toFixed(1)} °C` : "";
  fillFacts($("system-facts"), [
    ["Hostname", s.hostname],
    s.kernel && ["Kernel", s.kernel],
    ["Load", s.load.map((l) => l.toFixed(2)).join("  ")],
    s.temperature_c != null && ["Temperature", `${s.temperature_c.toFixed(1)} °C`],
  ]);
  const used = s.mem_total ? (s.mem_total - s.mem_available) / s.mem_total * 100 : 0;
  $("mem-meter").firstElementChild.style.setProperty("width", used.toFixed(1) + "%");
  $("mem-meter").setAttribute("aria-valuenow", used.toFixed(0));
  $("mem-label").textContent = s.mem_total ? `${fmtBytes(s.mem_total - s.mem_available)} of ${fmtBytes(s.mem_total)}` : "–";
}

function renderWireGuard(snap, now) {
  const wg = snap.wireguard;
  const wgName = $("wg-name");
  if (wgName) wgName.textContent = wg.interface || "wg0";

  fillFacts($("wg-facts"), [
    ["Interface", el("span", {}, el("span", { class: "mono", text: (wg.interface || "wg0") + " " }), statusBadge(wg.up ? "good" : "critical", wg.up ? "Up" : "Down"))],
    wg.error && ["Error", wg.error],
    wg.public_key && ["Public key", el("span", { class: "mono", text: wg.public_key })],
    wg.listen_port && ["Listen port", wg.listen_port],
  ]);
  $("wg-peers").replaceChildren(...wg.peers.map((p, i) => {
    const dl = el("dl", { class: "facts" });
    fillFacts(dl, [
      ["Endpoint", el("span", { class: "mono", text: p.endpoint || "–" })],
      ["Handshake", fmtAgo(parseTime(p.last_handshake), now)],
      ["Received", fmtBytes(p.rx_bytes)],
      ["Sent", fmtBytes(p.tx_bytes)],
      ["Allowed IPs", el("span", { class: "mono", text: p.allowed_ips.join(", ") || "–" })],
    ]);
    return el("div", { class: "peer" }, el("h3", { text: wg.peers.length > 1 ? `Peer ${i + 1}` : "Peer" }), dl);
  }));
}

function renderModem(snap) {
  const m = snap.modem;
  fillFacts($("modem-facts"), [
    ["Status", statusBadge(m.reachable ? "good" : "critical", m.reachable ? "Reachable" : "Unreachable")],
    m.error && !m.reachable && ["Error", m.error],
    m.reachable && ["Network", m.network_type || "–"],
    m.reachable && ["Signal", `${m.signal_strength || "–"}${m.signal_level ? ` (level ${m.signal_level})` : ""}`],
    m.reachable && ["IP address", el("span", { class: "mono", text: m.ip_address || "–" })],
    m.reachable && ["Uptime", m.uptime || "–"],
    m.reachable && ["Firmware", m.software_version || "–"],
  ]);
}

function renderRouting(snap) {
  const r = snap.routing;
  const vpn = { up: ["good", "VPN health check passing", "Pings sent through the tunnel are answered."],
    down: ["critical", "VPN health check failing", "Pings sent through the tunnel are not answered."],
    unknown: ["neutral", "VPN health check pending", "Waiting for the first result."] }[r.vpn] || ["neutral", "VPN health check pending", ""];
  const items = [
    el("li", {},
      r.synced ? statusBadge("good", "mini-net-man") : statusBadge("critical", "mini-net-man"),
      el("span", { class: "check-desc", text: r.synced
        ? "Holds the router's bridge, firewall, NAT and VPN routing."
        : (r.error || "Not set up yet.") })),
    el("li", {}, statusBadge(vpn[0], vpn[1]), el("span", { class: "check-desc", text: vpn[2] })),
    el("li", {},
      statusBadge(r.kill_switch ? "good" : "neutral", r.kill_switch ? "Kill switch on" : "Kill switch off"),
      el("span", { class: "check-desc", text: r.kill_switch
        ? "LAN traffic is dropped while the tunnel is missing."
        : "LAN traffic uses the normal internet while the VPN health check fails." })),
  ];
  for (const issue of r.issues || []) {
    items.push(el("li", {}, statusBadge("warning", "Not applied yet"), el("span", { class: "check-desc", text: issue })));
  }
  $("routing-checks").replaceChildren(...items);
}

// ---- mini-net-man rules ----

let rulesBusy = false;

function renderRules(snap) {
  const enforce = snap.routing.enforce !== false;
  const box = $("rules-enforce");
  if (box && !rulesBusy) box.checked = enforce;
  $("rules-status").textContent = enforce ? "The router keeps its rules in place" : "Hands off: edit the rules in mini-net-man's panel";
  $("rules-restore").disabled = rulesBusy;
}

// ---- software update ----

let bootVersion = null;

function safeLink(url) {
  return typeof url === "string" && url.startsWith("https://") ? url : null;
}

function renderUpdate(snap, now) {
  const u = snap.update;
  if (bootVersion === null) bootVersion = u.current;
  // The service restarted into another version; the panel's own files changed too.
  if (u.current !== bootVersion) { location.reload(); return; }

  let badge;
  if (u.busy) badge = statusBadge("neutral", { checking: "Checking…", installing: "Installing…", "rolling back": "Going back…" }[u.busy] || u.busy);
  else if (u.installed) badge = statusBadge("warning", `Installed ${u.installed}, restarting`);
  else if (!u.comparable) badge = statusBadge("neutral", "Local build");
  else if (!u.latest) badge = statusBadge("neutral", "Not checked yet");
  else if (u.available) badge = statusBadge("info", `${u.latest} is available`);
  else badge = statusBadge("good", "Up to date");

  const release = safeLink(u.url);
  fillFacts($("update-facts"), [
    ["Running", el("span", { class: "mono", text: u.current || "–" })],
    ["Status", badge],
    u.latest && ["Latest release", release ? el("a", { href: release, target: "_blank", rel: "noopener noreferrer", class: "mono", text: u.latest }) : el("span", { class: "mono", text: u.latest })],
    ["Last checked", fmtAgo(parseTime(u.checked_at), now)],
  ]);
  $("update-status").textContent = "";

  const note = $("update-note");
  let noteText = u.error || "";
  if (!noteText && !u.comparable) noteText = "This is a local build with no release version, so it can't tell if a release is newer. Update it with: opiz3-router -update -force";
  if (!noteText && u.installed) noteText = "The router restarts for a few seconds; this page reloads by itself. Routing and the firewall stay in place meanwhile.";
  note.hidden = !noteText;
  note.textContent = noteText;

  const idle = !u.busy && !u.installed && !updateAction;
  $("update-check").disabled = !idle;
  const install = $("update-install");
  install.hidden = !(u.available && !u.installed);
  install.disabled = !idle;
  install.textContent = u.latest ? `Install ${u.latest}` : "Install";
  const back = $("update-rollback");
  back.hidden = !(u.can_rollback && !u.installed);
  back.disabled = !idle;

  const notes = $("update-notes");
  notes.hidden = !(u.available && u.notes && !u.installed);
  notes.textContent = u.notes || "";
}

let updateAction = null;

async function runUpdateAction(label, path, confirmText) {
  if (confirmText && !confirm(confirmText)) return;
  updateAction = label;
  if (latest) renderUpdate(latest, parseTime(latest.time) ?? Date.now());
  try {
    await postAction(path, {});
  } catch (e) {
    // The service may already be restarting; the live connection brings the new state.
    if (!(e instanceof TypeError)) {
      const note = $("update-note");
      note.hidden = false;
      note.textContent = e.message;
    }
  }
  updateAction = null;
  if (latest) renderUpdate(latest, parseTime(latest.time) ?? Date.now());
}

function feedTime(t, now) {
  if (t == null) return "";
  const sameDay = new Date(t).toDateString() === new Date(now).toDateString();
  const clock = sameDay ? fmtClock(t) : new Date(t).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  return now - t < 3600e3 ? `${clock} · ${fmtAgo(t, now)}` : clock;
}

function feedEmpty(text) {
  return el("li", { class: "empty", text });
}

function renderSMS(snap, now) {
  const sms = snap.sms;
  const note = $("sms-note");
  note.hidden = true;
  if (!sms.available) {
    note.hidden = false;
    note.textContent = sms.error ? `Can't read the modem's inbox: ${sms.error}` : "Reading the modem's inbox…";
  } else if (sms.error) {
    note.hidden = false;
    note.textContent = "The modem isn't answering; showing the messages read last.";
  }
  $("sms-count").textContent = sms.available ? `${sms.messages.length} message${sms.messages.length === 1 ? "" : "s"}` : "";
  const items = sms.messages.map((m) => el("li", {},
    el("div", { class: "feed-head" },
      el("span", { class: "sms-sender" }, m.sender || "Unknown sender",
        parseTime(m.received_at) != null ? el("span", { class: "tag-new", text: "New" }) : null),
      el("span", { class: "feed-time", text: feedTime(parseTime(m.time) ?? parseTime(m.received_at), now) })),
    el("div", { class: "sms-text", dir: "auto", text: m.text })));
  $("sms-list").replaceChildren(...(items.length ? items : sms.available ? [feedEmpty("No messages")] : []));
}

function renderLog(snap, now) {
  const sources = { link: "Link", vpn: "VPN", sms: "SMS", guard: "Routing", router: "Router" };
  const items = [...snap.notifications].reverse().map((n) => el("li", { class: "log-item" },
    statusBadge(n.level, n.text),
    el("span", { class: "feed-time", text: feedTime(parseTime(n.time), now) }),
    el("span", { class: "log-source", text: sources[n.source] || n.source })));
  $("log-count").textContent = snap.notifications.length ? `last ${snap.notifications.length}` : "";
  $("log-list").replaceChildren(...(items.length ? items : [feedEmpty("Nothing yet")]));
}

// ---- network discovery ----

let scanAction = null;

function deviceName(d) {
  const badge = statusBadge(d.reachable ? "good" : "neutral", d.name || "Unknown device");
  badge.classList.add("plain");
  badge.title = d.reachable ? "Reachable" : "Offline";
  badge.firstElementChild.setAttribute("aria-label", badge.title);
  badge.firstElementChild.removeAttribute("aria-hidden");
  badge.firstElementChild.setAttribute("role", "img");
  return badge;
}

function renderNetworkScan(snap, now) {
  const scan = snap.network_scan || {};
  const active = scan.active;
  const updated = parseTime(scan.updated_at);
  const devCount = scan.devices ? scan.devices.length : 0;

  $("netscan-status").textContent = scan.error
    ? "Scan error"
    : active
    ? "Scanning…"
    : updated
    ? `Updated ${fmtAgo(updated, now)}`
    : "Idle";

  const scanBtn = $("netscan-btn");
  if (scanBtn) {
    scanBtn.disabled = active || !!scanAction;
    scanBtn.textContent = active ? "Scanning…" : scanAction || "Scan";
  }

  const sub = [];
  if (scan.interface) sub.push(`via ${scan.interface}`);
  if (scan.subnet) sub.push(scan.subnet);
  if (devCount > 0) sub.push(`${devCount} device${devCount === 1 ? "" : "s"} found`);
  $("netscan-sub").textContent = sub.join(" · ");

  const note = $("netscan-note");
  if (scan.error) {
    note.hidden = false;
    note.textContent = scan.error;
  } else {
    note.hidden = true;
  }

  const devices = scan.devices || [];
  const rows = devices.map((d) => {
    const reachable = d.reachable;
    return el("tr", {},
      el("td", {}, deviceName(d)),
      el("td", { class: "mono", text: d.ip || "–" }),
      el("td", { class: "mono", text: d.mac || "–" }),
      el("td", { text: d.manufacturer || "–" }),
      el("td", {}, statusBadge(reachable ? "good" : "neutral", reachable ? "Reachable" : "Offline"))
    );
  });

  $("netscan-table").replaceChildren(
    ...(rows.length ? rows : [emptyRow(5, active ? "Scanning network…" : "Click Scan to discover devices on the Ethernet network")])
  );
}

// ---- LCD display power ----

let displayAction = null;
let displayMessage = null;

function renderDisplay(snap) {
  const d = snap.display || {};
  const powered = d.powered !== false;
  $("display-status").textContent = displayAction || (powered ? "Powered on" : "Powered off");

  const note = $("display-note");
  note.hidden = !displayMessage;
  note.textContent = displayMessage || "";

  const btn = $("display-toggle-btn");
  if (btn) {
    btn.disabled = !!displayAction;
    btn.textContent = displayAction ? "Updating…" : powered ? "Turn display off" : "Turn display on";
  }

  const wakeBox = $("display-wake-checkbox");
  if (wakeBox && document.activeElement !== wakeBox) {
    wakeBox.checked = d.wake_on_notification !== false;
  }

  fillFacts($("display-facts"), [
    ["Power", statusBadge(powered ? "good" : "neutral", powered ? "On" : "Off")],
    ["Screen", "Status screen (ST7789 240*280)"],
    ["Wake on event", d.wake_on_notification !== false ? "15s on notification" : "Disabled"],
  ]);
}

// ---- Ethernet port mode ----

const ETH_MODES = { uplink: "an internet uplink", lan: "a LAN port" };
let receivedAt = Date.now();
let ethAction = null;
let ethMessage = null;

async function postAction(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (!res.ok) {
    let msg = `The router answered ${res.status}.`;
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    throw new Error(msg);
  }
}

async function runEthAction(label, path, body) {
  ethAction = label;
  ethMessage = null;
  if (latest) renderEthernet(latest);
  try {
    await postAction(path, body);
  } catch (e) {
    ethMessage = e instanceof TypeError
      ? "Lost the connection to the router. If this device is connected by Ethernet, reconnect over Wi-Fi and keep the change before the time runs out."
      : e.message;
  }
  ethAction = null;
  if (latest) renderEthernet(latest);
}

function secondsLeft(deadline, snap) {
  const serverNow = (parseTime(snap.time) ?? receivedAt) + (Date.now() - receivedAt);
  return Math.max(0, Math.round((parseTime(deadline) - serverNow) / 1000));
}

function renderEthernet(snap) {
  const n = snap.network;
  const current = n.ethernet_mode;
  $("eth-status").textContent = n.busy ? "Applying…" : ethAction || (current ? `Now ${ETH_MODES[current]}` : "");

  const note = $("eth-note");
  const noteText = ethMessage || (n.error ? `The last change failed: ${n.error}` : "") || (!n.switchable && n.unavailable ? `Switching is not available: ${n.unavailable}` : "");
  note.hidden = !noteText;
  note.textContent = noteText;

  const pending = n.pending;
  $("eth-pending").hidden = !pending;
  if (pending) {
    $("eth-pending-text").textContent = `The Ethernet port is now ${ETH_MODES[pending.mode]}. Keep this setting? Otherwise it goes back to ${ETH_MODES[pending.previous]} in ${secondsLeft(pending.deadline, snap)} s.`;
  }
  const locked = n.busy || !!ethAction;
  $("eth-keep").disabled = locked;
  $("eth-undo").disabled = locked;

  for (const opt of document.querySelectorAll(".mode-option")) {
    opt.setAttribute("aria-checked", String(opt.dataset.mode === current));
    opt.disabled = locked || !!pending || !n.switchable || opt.dataset.mode === current;
  }
}

function renderInterfaces(snap) {
  const roles = { "wan-ethernet": "Ethernet uplink", "wan-4g": "4G uplink", "wan-wifi": "Wi-Fi uplink", vpn: "VPN tunnel", lan: "LAN bridge", "lan-port": "LAN port", wifi: "Wi-Fi access point" };
  const rows = snap.interfaces.map((i) => el("tr", {},
    el("td", { class: "mono", text: i.name }),
    el("td", {}, roles[i.role] || i.role, i.mac ? el("span", { class: "sub mono", text: i.mac }) : null),
    el("td", {}, statusBadge(!i.present ? "neutral" : i.up ? "good" : "critical", !i.present ? "Missing" : i.up ? "Up" : "Down")),
    el("td", { class: "mono" }, i.addresses.length ? i.addresses.map((a) => el("div", { text: a })) : "–"),
    el("td", { class: "num", text: i.present ? fmtRate(i.rx_rate) : "–" }),
    el("td", { class: "num", text: i.present ? fmtRate(i.tx_rate) : "–" }),
    el("td", { class: "num", text: i.present ? fmtBytes(i.rx_bytes) : "–" }),
    el("td", { class: "num", text: i.present ? fmtBytes(i.tx_bytes) : "–" })));
  $("ifaces-table").replaceChildren(...(rows.length ? rows : [emptyRow(8, "No data yet")]));
}

// ---- Event Listeners ----

const scanBtn = $("netscan-btn");
if (scanBtn) {
  scanBtn.addEventListener("click", async () => {
    scanAction = "Starting…";
    if (latest) renderNetworkScan(latest, Date.now());
    try {
      await postAction("api/network/scan", {});
    } catch (e) {
      alert("Scan failed: " + e.message);
    }
    scanAction = null;
    if (latest) renderNetworkScan(latest, Date.now());
  });
}

const displayToggleBtn = $("display-toggle-btn");
if (displayToggleBtn) {
  displayToggleBtn.addEventListener("click", async () => {
    const powered = latest && latest.display ? latest.display.powered !== false : true;
    const next = !powered;
    displayAction = next ? "Turning on…" : "Turning off…";
    displayMessage = null;
    if (latest) renderDisplay(latest);
    try {
      await postAction("api/display/power", { powered: next });
    } catch (e) {
      displayMessage = e.message;
    }
    displayAction = null;
    if (latest) renderDisplay(latest);
  });
}

const wakeBox = $("display-wake-checkbox");
if (wakeBox) {
  wakeBox.addEventListener("change", async () => {
    try {
      await postAction("api/display/wake-on-notification", { enabled: wakeBox.checked });
    } catch (e) {
      alert("Failed to update wake setting: " + e.message);
      if (latest) renderDisplay(latest);
    }
  });
}

document.querySelectorAll(".mode-option").forEach((opt) => opt.addEventListener("click", () => {
  const mode = opt.dataset.mode;
  const warning = mode === "lan"
    ? "Switch the Ethernet port to a LAN port?\n\nIt stops being an internet connection; the 4G modem will be the only one. Devices plugged into the port join the LAN."
    : "Switch the Ethernet port to an internet uplink?\n\nDevices plugged into the port lose their connection to the LAN.";
  if (!confirm(`${warning}\n\nIf the switch cuts off this device, it is undone automatically after 90 seconds unless you keep it.`)) return;
  runEthAction("Switching…", "api/network/ethernet-mode", { mode });
}));

$("eth-keep").addEventListener("click", () => runEthAction("Keeping…", "api/network/keep"));
$("eth-undo").addEventListener("click", () => runEthAction("Undoing…", "api/network/undo"));
setInterval(() => { if (latest && latest.network.pending) renderEthernet(latest); }, 1000);

$("update-check").addEventListener("click", () => runUpdateAction("Checking…", "api/update/check"));
$("update-install").addEventListener("click", () => runUpdateAction("Installing…", "api/update/install",
  `Install ${latest && latest.update.latest}?\n\nThe router downloads it, checks it and restarts, which takes a few seconds. Routing and the firewall stay in place while it restarts, but the panel and the display go away for a moment. The previous version is kept so you can go back.`));
$("update-rollback").addEventListener("click", () => runUpdateAction("Going back…", "api/update/rollback",
  "Go back to the previous version? The router restarts for a few seconds."));

$("rules-enforce").addEventListener("change", async (ev) => {
  const on = ev.target.checked;
  const text = on
    ? "Keep the router's rules in place?\n\nWithin 30 seconds the router puts back any of its rules that you removed or edited in mini-net-man. Your own rules stay."
    : "Let the router leave mini-net-man's rules alone?\n\nYou can then edit them in mini-net-man's panel. Nothing is put back until you turn this on again or restore them.";
  if (!confirm(text)) { ev.target.checked = !on; return; }
  rulesBusy = true;
  const note = $("rules-note");
  note.hidden = true;
  try {
    await postAction("api/rules/enforce", { enabled: on });
  } catch (e) {
    ev.target.checked = !on;
    note.hidden = false;
    note.textContent = e.message;
  }
  rulesBusy = false;
  if (latest) renderRules(latest);
});

$("rules-restore").addEventListener("click", async () => {
  if (!confirm("Put the router's rules back in mini-net-man now?\n\nIt removes your edits to them and recreates any that you deleted. Rules you added yourself stay.")) return;
  rulesBusy = true;
  const note = $("rules-note");
  note.hidden = true;
  try {
    await postAction("api/rules/restore", {});
    note.hidden = false;
    note.textContent = "The router's rules are back in place.";
  } catch (e) {
    note.hidden = false;
    note.textContent = e.message;
  }
  rulesBusy = false;
  if (latest) renderRules(latest);
});

// ---- Wi-Fi access point ----

const wifiFields = { ssid: "wifi-ssid", channel: "wifi-channel", dhcp_start: "wifi-dhcp-start", dhcp_end: "wifi-dhcp-end" };

function showWiFiNote(text) {
  $("wifi-note").hidden = !text;
  $("wifi-note").textContent = text || "";
}

async function loadWiFi() {
  try {
    const res = await fetch("api/wifi", { cache: "no-store" });
    if (!res.ok) throw new Error(`The router answered ${res.status}.`);
    const w = await res.json();
    for (const [key, id] of Object.entries(wifiFields)) $(id).value = w[key] ?? "";
    $("wifi-passphrase").value = "";
    $("wifi-passphrase").placeholder = w.passphrase_set ? "Leave empty to keep" : "Required, 8 to 63 characters";
    $("wifi-passphrase").required = !w.passphrase_set;
    $("wifi-status").textContent = "";
  } catch (e) {
    $("wifi-status").textContent = "Could not load the settings";
    showWiFiNote(e.message);
  }
}

$("wifi-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if (!confirm("Save the Wi-Fi settings? The access point restarts and Wi-Fi devices reconnect.")) return;
  const body = { ssid: $("wifi-ssid").value, passphrase: $("wifi-passphrase").value, channel: Number($("wifi-channel").value),
    dhcp_start: $("wifi-dhcp-start").value.trim(), dhcp_end: $("wifi-dhcp-end").value.trim() };
  $("wifi-save").disabled = true;
  $("wifi-status").textContent = "Saving…";
  showWiFiNote("");
  try {
    await postAction("api/wifi", body);
    $("wifi-status").textContent = "Saved";
    await loadWiFi();
    $("wifi-status").textContent = "Saved";
  } catch (e) {
    $("wifi-status").textContent = "";
    showWiFiNote(e instanceof TypeError ? "Lost the connection while the access point restarted. Reconnect and check that the settings were saved." : e.message);
  }
  $("wifi-save").disabled = false;
});

loadWiFi();

// ---- live connection ----

function setConn(state, label) {
  $("conn").dataset.state = state;
  $("conn-label").textContent = label;
}

let lastEvent = 0;

function onSnapshot(snap) {
  lastEvent = Date.now();
  receivedAt = lastEvent;
  setConn("live", "Live");
  render(snap);
}

function connect() {
  if (!("EventSource" in window)) {
    const poll = () => fetch("api/status").then((r) => r.json()).then(onSnapshot).catch(() => setConn("offline", "Offline"));
    poll();
    setInterval(poll, 2000);
    return;
  }
  const es = new EventSource("api/events");
  es.addEventListener("status", (e) => onSnapshot(JSON.parse(e.data)));
  es.onerror = () => setConn("offline", "Reconnecting…");
}

setInterval(() => {
  if (lastEvent && Date.now() - lastEvent > 10000 && $("conn").dataset.state === "live") setConn("stale", "No updates");
}, 2000);

// ---- theme ----

function storedTheme() {
  try { return localStorage.getItem("theme"); } catch { return null; }
}

function applyTheme(theme) {
  if (theme) document.documentElement.dataset.theme = theme;
  else delete document.documentElement.dataset.theme;
}

applyTheme(storedTheme());
$("theme-toggle").addEventListener("click", () => {
  const dark = getComputedStyle(document.documentElement).colorScheme.includes("dark");
  const next = dark ? "light" : "dark";
  applyTheme(next);
  try { localStorage.setItem("theme", next); } catch { /* storage unavailable */ }
  if (latest) render(latest);
});

connect();
