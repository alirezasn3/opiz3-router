"use strict";

// Everything that comes from the router (DHCP hostnames in particular) is
// untrusted, so the DOM is only ever built with textContent, never innerHTML.

const HISTORY_SECONDS = 180;
const HANDSHAKE_STALE_SECONDS = 180;
const SVG_NS = "http://www.w3.org/2000/svg";

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

function svg(tag, attrs) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, v);
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
  $("hostname").textContent = snap.system.hostname || "";
  renderVPN(snap, now);
  renderWAN(snap);
  renderClients(snap, now);
  renderSystem(snap);
  renderTraffic(snap, now);
  renderWireGuard(snap, now);
  renderModem(snap);
  renderRouting(snap);
  renderInterfaces(snap);
  renderSMS(snap, now);
  renderLog(snap, now);
  renderEthernet(snap);
  renderMonitor(snap);
}

function vpnState(snap, now) {
  const wg = snap.wireguard, r = snap.routing;
  const peer = wg.peers[0];
  const hs = peer ? parseTime(peer.last_handshake) : null;
  const fresh = hs != null && (now - hs) / 1000 < HANDSHAKE_STALE_SECONDS;
  if (!r.rule) return ["critical", "Not routed", "LAN traffic is not steered into the VPN."];
  if (wg.up && r.vpn_route && fresh) return ["good", "Protected", `Last handshake ${fmtAgo(hs, now)}.`];
  if (wg.up && r.vpn_route) return ["warning", "No recent handshake", `Last handshake ${fmtAgo(hs, now)}. Traffic stays in the tunnel but may not get through.`];
  if (r.kill_switch) return ["warning", "Blocked", "The VPN is down. The kill switch is holding LAN traffic back."];
  if (!r.kill_switch_enabled) return ["critical", "Unprotected", "The VPN is down and the kill switch is off, so LAN traffic goes out unencrypted."];
  return ["critical", "Leaking", "The VPN is down and the kill switch route is missing."];
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
  // Without per-station Wi-Fi data every client would look wired, so the
  // split is only shown when that data is available.
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
  $("wg-name").textContent = wg.interface || "wg0";
  fillFacts($("wg-facts"), [
    ["Interface", el("span", {}, el("span", { class: "mono", text: wg.interface + " " }), statusBadge(wg.up ? "good" : "critical", wg.up ? "Up" : "Down"))],
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
  const checks = [
    [r.ip_forward, "IP forwarding", "The kernel forwards packets between interfaces."],
    [r.rule, "VPN policy rule", "Traffic arriving from the LAN bridge uses the VPN table."],
    [r.vpn_route, "VPN default route", "The VPN table sends traffic into the WireGuard tunnel."],
    [r.kill_switch_enabled ? r.kill_switch : null, "Kill switch", r.kill_switch_enabled
      ? "A blackhole route stops LAN traffic if the tunnel goes away."
      : "Turned off in the config. LAN traffic goes out of the WAN unencrypted while the tunnel is down."],
    [r.firewall, "Firewall", "The panel is only reachable from the LAN; unsolicited inbound traffic to the LAN is dropped."],
  ];
  // ok is null for a check that is turned off on purpose.
  const items = checks.map(([ok, label, desc]) => el("li", {},
    statusBadge(ok == null ? "neutral" : ok ? "good" : "critical", ok == null ? `${label} off` : label),
    el("span", { class: "check-desc", text: desc })));
  if (r.restores > 0) {
    const when = parseTime(r.last_restore);
    items.push(el("li", {},
      statusBadge("warning", `Restored ${r.restores} time${r.restores === 1 ? "" : "s"}`),
      el("span", { class: "check-desc", text: `Something removed part of the setup and the router put it back. Last ${fmtAgo(when, parseTime(snap.time) ?? Date.now())}: ${r.last_restore_reason || "unknown"}.` })));
  }
  if (r.error) items.push(el("li", {}, statusBadge("warning", "Check failed"), el("span", { class: "check-desc", text: r.error })));
  $("routing-checks").replaceChildren(...items);
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
      // Fall back to when the router saw it if the modem's date is unreadable.
      el("span", { class: "feed-time", text: feedTime(parseTime(m.time) ?? parseTime(m.received_at), now) })),
    // dir=auto lets right-to-left messages (Persian, Arabic) read correctly.
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

// ---- display page and Wi-Fi channels ----

let pageAction = null;
let pageMessage = null;

function renderMonitor(snap) {
  const m = snap.monitor;
  const adapter = m.interface ? `${m.interface}${m.driver ? ` (${m.driver})` : ""}` : "Wi-Fi adapter";
  const dwell = m.hop_interval_ms ? `, ${m.hop_interval_ms / 1000} s per channel` : "";
  $("monitor-status").textContent = pageAction || (!m.present ? `${adapter} not plugged in` : m.active ? `${adapter} listening${dwell}` : `${adapter} starting…`);

  const note = $("monitor-note");
  const noteText = pageMessage || (m.present && !m.active && m.error ? `The adapter failed: ${m.error}` : "") ||
    (!m.present ? `Plug in the USB Wi-Fi adapter${m.interface ? ` (${m.interface})` : ""} to see the Wi-Fi channels.` : "");
  note.hidden = !noteText;
  note.textContent = noteText;

  for (const opt of document.querySelectorAll(".page-option")) {
    const page = opt.dataset.page;
    opt.setAttribute("aria-checked", String(page === m.page));
    opt.disabled = !!pageAction || page === m.page || (page === "channels" && !m.present);
  }

  const show = m.active && m.channels.length > 0;
  $("monitor-data").hidden = !show;
  if (!show) return;
  $("own-channel").textContent = m.own_channel || "unknown";
  $("best-channel").textContent = m.recommended || "–";
  const scale = Math.max(3, ...m.channels.map((c) => c.networks));
  const cols = m.channels.map((c) => {
    const kind = c.channel === m.recommended ? "best" : c.channel === m.own_channel ? "own" : c.networks ? "plain" : "empty";
    const bar = el("div", { class: "channel-bar", "data-kind": kind });
    bar.style.setProperty("height", `${c.networks ? Math.max(4, c.networks / scale * 120) : 3}px`);
    const tip = `Channel ${c.channel}: ${c.networks} network${c.networks === 1 ? "" : "s"}` + (c.networks ? `, strongest ${c.strongest} dBm` : "");
    return el("div", { class: "channel-col", title: tip },
      el("span", { class: "channel-count", text: c.networks ? String(c.networks) : "" }), bar);
  });
  $("channel-chart").replaceChildren(...cols);
  let labels = document.querySelector(".channel-labels");
  if (!labels) {
    labels = el("div", { class: "channel-labels", "aria-hidden": "true" });
    $("channel-chart").after(labels);
  }
  labels.replaceChildren(...m.channels.map((c) =>
    el("span", { class: c.channel === m.own_channel || c.channel === m.recommended ? "strong" : "", text: String(c.channel) })));

  $("networks-summary").textContent = `Networks nearby (${m.networks.length})`;
  const rows = m.networks.map((n) => el("tr", {},
    el("td", { text: n.ssid || "Hidden network" }),
    el("td", { class: "num", text: String(n.channel) }),
    el("td", { class: "num", text: n.signal ? `${n.signal} dBm` : "–" })));
  $("networks-table").replaceChildren(...(rows.length ? rows : [emptyRow(3, "None heard yet")]));
}

document.querySelectorAll(".page-option").forEach((opt) => opt.addEventListener("click", async () => {
  pageAction = "Switching…";
  pageMessage = null;
  if (latest) renderMonitor(latest);
  try {
    await postAction("api/display/page", { page: opt.dataset.page });
  } catch (e) {
    pageMessage = e.message;
  }
  pageAction = null;
  if (latest) renderMonitor(latest);
}));

// ---- Ethernet port mode ----

const ETH_MODES = { uplink: "an internet uplink", lan: "a LAN port" };
let receivedAt = Date.now();
let ethAction = null; // text shown while a request is in flight
let ethMessage = null; // result of the last request, if it failed

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
      // fetch rejects with a TypeError when the connection itself fails,
      // which is expected when the switch cuts off this device.
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
// Keep the countdown moving between updates.
setInterval(() => { if (latest && latest.network.pending) renderEthernet(latest); }, 1000);

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

function renderTraffic(snap, now) {
  const samples = snap.traffic.map((s) => ({ t: parseTime(s.t), rx: s.rx, tx: s.tx })).filter((s) => s.t != null);
  const last = samples[samples.length - 1];
  $("hero-rx").textContent = last ? fmtRate(last.rx) : "–";
  $("legend-rx").textContent = last ? fmtRate(last.rx) : "–";
  $("legend-tx").textContent = last ? fmtRate(last.tx) : "–";
  chart.update(samples, last ? last.t : now);

  const rows = [];
  for (let i = samples.length - 1; i >= 0 && rows.length < 18; i -= 10) {
    const s = samples[i];
    rows.push(el("tr", {}, el("td", { text: fmtClock(s.t) }), el("td", { class: "num", text: fmtRate(s.rx) }), el("td", { class: "num", text: fmtRate(s.tx) })));
  }
  $("traffic-table").replaceChildren(...(rows.length ? rows : [emptyRow(3, "No samples yet")]));
}

// ---- throughput chart ----

// niceStep rounds up to 1, 2, 2.5 or 5 times a power of ten so ticks are clean.
function niceStep(raw) {
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  for (const m of [1, 2, 2.5, 5, 10]) if (raw <= m * pow) return m * pow;
  return 10 * pow;
}

const chart = (() => {
  const root = $("chart");
  const tooltip = $("chart-tooltip");
  const M = { top: 8, right: 10, bottom: 24, left: 64 };
  let samples = [], end = Date.now(), hoverX = null, geom = null;
  const node = svg("svg", { role: "img", "aria-label": "VPN download and upload rate over the last three minutes", tabindex: "0" });
  root.prepend(node);

  function draw() {
    const w = root.clientWidth, h = root.clientHeight;
    if (!w || !h) return;
    node.setAttribute("viewBox", `0 0 ${w} ${h}`);
    node.replaceChildren();
    const pw = w - M.left - M.right, ph = h - M.top - M.bottom;
    const start = end - HISTORY_SECONDS * 1000;
    // Work in bits/s so the axis matches the displayed units.
    const peak = Math.max(1000, ...samples.map((s) => Math.max(s.rx, s.tx) * 8));
    const step = niceStep(peak / 4);
    const yMax = Math.ceil(peak / step) * step;
    const x = (t) => M.left + (t - start) / (end - start) * pw;
    const y = (bytes) => M.top + ph - (bytes * 8) / yMax * ph;
    geom = { x, y, start, pw };

    for (let v = 0; v <= yMax + step / 2; v += step) {
      const yy = M.top + ph - v / yMax * ph;
      node.append(svg("line", { class: v === 0 ? "baseline" : "gridline", x1: M.left, x2: M.left + pw, y1: yy, y2: yy }));
      const label = svg("text", { class: "tick", x: M.left - 8, y: yy + 4, "text-anchor": "end" });
      label.textContent = fmtRate(v / 8);
      node.append(label);
    }
    for (const [secAgo, text] of [[180, "3 min ago"], [120, "2 min"], [60, "1 min"], [0, "now"]]) {
      const label = svg("text", { class: "tick", x: x(end - secAgo * 1000), y: h - 6, "text-anchor": secAgo === 180 ? "start" : secAgo === 0 ? "end" : "middle" });
      label.textContent = text;
      node.append(label);
    }

    if (samples.length < 2) {
      const t = svg("text", { class: "empty", x: M.left + pw / 2, y: M.top + ph / 2, "text-anchor": "middle" });
      t.textContent = "Collecting samples…";
      node.append(t);
      return;
    }
    for (const key of ["tx", "rx"]) {
      let d = "", prev = null;
      for (const s of samples) {
        if (s.t < start) continue;
        // Break the line where samples are missing instead of bridging the gap.
        d += (prev && s.t - prev.t < 3500 ? "L" : "M") + x(s.t).toFixed(1) + "," + y(s[key]).toFixed(1);
        prev = s;
      }
      node.append(svg("path", { class: `line line-${key}`, d }));
    }
    if (hoverX != null) drawHover();
  }

  function nearest(px) {
    const t = geom.start + (px - M.left) / geom.pw * HISTORY_SECONDS * 1000;
    let best = null;
    for (const s of samples) if (!best || Math.abs(s.t - t) < Math.abs(best.t - t)) best = s;
    return best;
  }

  function drawHover() {
    node.querySelectorAll(".hover").forEach((n) => n.remove());
    const s = nearest(hoverX);
    if (!s || !geom) { tooltip.hidden = true; return; }
    const xx = geom.x(s.t);
    const g = svg("g", { class: "hover" });
    g.append(svg("line", { class: "crosshair", x1: xx, x2: xx, y1: M.top, y2: root.clientHeight - M.bottom }));
    for (const key of ["tx", "rx"]) g.append(svg("circle", { class: `dot dot-${key}`, cx: xx, cy: geom.y(s[key]), r: 4 }));
    node.append(g);

    const row = (key, label, value) => el("div", { class: "tt-row" }, el("span", { class: `key key-${key}` }), el("strong", { text: fmtRate(value) }), el("span", { text: label }));
    tooltip.replaceChildren(
      el("div", { class: "tt-time", text: `${fmtClock(s.t)} · ${fmtAgo(s.t, end)}` }),
      row("rx", "Download", s.rx),
      row("tx", "Upload", s.tx));
    tooltip.hidden = false;
    const tw = tooltip.offsetWidth;
    const left = xx + 12 + tw > root.clientWidth ? xx - 12 - tw : xx + 12;
    tooltip.style.setProperty("left", Math.max(0, left) + "px");
  }

  function hide() { hoverX = null; tooltip.hidden = true; node.querySelectorAll(".hover").forEach((n) => n.remove()); }

  node.addEventListener("pointermove", (e) => {
    const rect = node.getBoundingClientRect();
    hoverX = e.clientX - rect.left;
    if (samples.length) drawHover();
  });
  node.addEventListener("pointerleave", hide);
  node.addEventListener("blur", hide);
  node.addEventListener("focus", () => { if (samples.length) { hoverX = geom.x(samples[samples.length - 1].t); drawHover(); } });
  node.addEventListener("keydown", (e) => {
    if (!samples.length || hoverX == null || (e.key !== "ArrowLeft" && e.key !== "ArrowRight")) return;
    e.preventDefault();
    const i = samples.indexOf(nearest(hoverX)) + (e.key === "ArrowLeft" ? -1 : 1);
    hoverX = geom.x(samples[Math.max(0, Math.min(samples.length - 1, i))].t);
    drawHover();
  });
  new ResizeObserver(draw).observe(root);

  return { update(s, e) { samples = s; end = e; draw(); } };
})();

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
  // EventSource reconnects by itself; only reflect the state.
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
