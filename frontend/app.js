// swarm-net dashboard.
//
// Plain browser JavaScript: no framework, no bundler, no CDN. The file is
// served as-is by the frontend's nginx, which proxies api/ and ws on the same
// origin to the control center, so every URL here is relative.
//
// Data flow (contracts: docs/architecture/control-center.md and
// docs/architecture/drone-simulation.md):
//   server -> browser  {"type":"snapshot", sim:{...}, nodes:[...], tasks:[...]}  every 1s
//   server -> browser  {"type":"event", kind, node, detail}                      as they happen
//   browser -> server  {"type":"task", kind, body, count}
//   browser -> server  {"type":"chaos", node, action, delay_ms}
//   browser -> server  {"type":"sim", base_ms, per_unit_ms, ..., positions}
//
// Every field read from the server goes through a defensive accessor: a
// missing or null field must degrade to "-", never throw and freeze the page.
// The simulation fields (sim, pos, flows, threshold, hysteresis, sim_version)
// are all optional, so the page also works against an older control center.
// All server text is inserted with textContent, never innerHTML.
//
// CSP: script-src 'self' and style-src 'self'. Nothing here sets a style
// attribute; geometry goes through SVG attributes, visibility through the
// SVG `display` attribute or the `hidden` property, and colours through CSS
// classes or the SVG `stroke` / `fill` presentation attributes.

(function () {
  "use strict";

  // ---------------------------------------------------------------- config

  var SVGNS = "http://www.w3.org/2000/svg";
  var VIEW_W = 800;
  var VIEW_H = 560;
  var EVENT_CAP = 200;
  var STALE_WARN_MS = 4000;   // snapshots arrive every 1s; 4 missed = suspicious
  var STALE_DROP_MS = 8000;   // treat the socket as dead and reconnect
  var BACKOFF_MIN_MS = 500;
  var BACKOFF_MAX_MS = 10000;
  var DEFAULT_DELAY_MS = 300;
  var MAX_DELAY_MS = 5000;

  // 3D view. World coordinates are divided by the airspace size, so the
  // camera works in a unit cube whatever `sim.size` is.
  var DEFAULT_SIZE = 100;     // geo.Size
  var CAM_DIST = 2.6;         // camera distance from its target, in cube sides
  var FOCAL = 850;            // pixels per unit at depth 1 and zoom 1
  var R0 = 10;                // drone glyphs are drawn at this radius, then scaled
  var ZOOM_MIN = 0.25;
  var ZOOM_MAX = 10;
  var PITCH_MIN = 0.03;
  var PITCH_MAX = 1.55;
  var HOME_3D = { yaw: -2.25, pitch: 0.55, zoom: 1, panX: 0, panY: 0, tx: 0.5, ty: 0.5, tz: 0.25 };
  var HOME_FLAT = { zoom: 1, panX: 0, panY: 0 };  // 2D map and grouped layout
  // 2D map: an orthographic top-down camera. One airspace side is MAP_SCALE
  // view-box pixels at zoom 1, so the whole square fits with room for ticks.
  var MAP_SCALE = 470;
  // Persisted view choice. A first visit (or blocked storage) gets the map.
  var VIEW_KEY = "swarm-net.view";
  var LAYOUT_KEY = "swarm-net.layout2d";

  // Message animation.
  var FLOW_WINDOW_MS = 1000;  // telemetry interval the counts cover
  var FLOW_DOT_MS = 750;      // travel time of one dot
  var FLOW_PER_LINK_CAP = 4;  // dots per (sender, receiver, type) per sample
  var FLOW_MAX_DOTS = 600;    // global cap, so a big swarm stays smooth

  // Simulation panel.
  var SIM_THROTTLE_MS = 150;
  var SLIDER_HOLD_MS = 1500;  // after the last touch, the user owns the slider

  var TASK_DEFAULTS = {
    echo: '{\n  "msg": "hello swarm"\n}',
    sleep: '{\n  "ms": 200\n}',
    hash: '{\n  "data": "abc"\n}'
  };

  var KNOWN_STATES = { alive: 1, suspect: 1, dead: 1 };
  var FLASH_EVENTS = { leader_change: 1, node_down: 1, node_up: 1, state_change: 1, chaos: 1 };

  // Message types, grouped for colour, shape and the per-type toggles. Colour
  // and shape both differ, so the legend works without colour vision.
  var SHAPES = {
    circle: "M-3.6,0a3.6,3.6 0 1,0 7.2,0a3.6,3.6 0 1,0 -7.2,0z",
    square: "M-3.2,-3.2h6.4v6.4h-6.4z",
    diamond: "M0,-4.6L4.6,0L0,4.6L-4.6,0z",
    triangle: "M0,-4.6L4.3,3.6H-4.3z"
  };
  var MSG_GROUPS = [
    { key: "ping", label: "PING / PONG", types: ["PING", "PONG"], shape: "circle" },
    { key: "hb", label: "HEARTBEAT / ACK", types: ["HEARTBEAT", "HEARTBEAT_ACK"], shape: "square" },
    { key: "member", label: "MEMBERSHIP_DELTA", types: ["MEMBERSHIP_DELTA"], shape: "diamond" },
    { key: "elect", label: "ELECTION_RESULT", types: ["ELECTION_RESULT"], shape: "triangle" },
    { key: "join", label: "JOIN_CLUSTER / JOIN_ACK", types: ["JOIN_CLUSTER", "JOIN_ACK"], shape: "diamond" },
    { key: "sync", label: "STATE_SYNC", types: ["STATE_SYNC"], shape: "triangle" },
    { key: "task", label: "TASK / TASK_RESULT", types: ["TASK", "TASK_RESULT"], shape: "square" },
    { key: "other", label: "other", types: [], shape: "circle" }
  ];
  var MSG_GROUP_OF = {};
  MSG_GROUPS.forEach(function (g) {
    g.on = true;
    g.types.forEach(function (t) { MSG_GROUP_OF[t] = g; });
  });
  function msgGroup(type) { return MSG_GROUP_OF[type] || MSG_GROUPS[MSG_GROUPS.length - 1]; }

  // Cluster colours (Okabe-Ito based). A leader keeps its colour for as long
  // as it leads, so a new leader never repaints the others.
  var NEUTRAL = "#8a929c";
  var PALETTE = ["#0072b2", "#e69f00", "#cc79a7", "#009e73", "#d55e00", "#56b4e9", "#8e6cbf", "#b8860b"];

  var SIM_FIELDS = [
    { key: "base_ms", label: "base", min: 0, max: 500, step: 1, unit: "ms" },
    { key: "per_unit_ms", label: "per unit", min: 0, max: 10, step: 0.1, unit: "ms/u" },
    { key: "jitter_ms", label: "jitter", min: 0, max: 200, step: 1, unit: "ms" },
    { key: "threshold", label: "threshold", min: 0.05, max: 1, step: 0.05, unit: "" },
    { key: "hysteresis", label: "hysteresis", min: 0, max: 10, step: 0.1, unit: "" }
  ];

  // ---------------------------------------------------------------- helpers

  function $(id) { return document.getElementById(id); }

  function num(v) { return typeof v === "number" && isFinite(v) ? v : null; }
  function str(v) {
    if (v === null || v === undefined) return "";
    return typeof v === "string" ? v : String(v);
  }
  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === "object" && !Array.isArray(v) ? v : null; }
  function clamp(v, lo, hi) { return v < lo ? lo : v > hi ? hi : v; }

  function h(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  function s(tag, attrs) {
    var e = document.createElementNS(SVGNS, tag);
    if (attrs) for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }

  // setAttribute only when the value changed: most frames move nothing.
  function attr(e, k, v) {
    v = String(v);
    if (e.getAttribute(k) !== v) e.setAttribute(k, v);
  }

  function show(e, on) { attr(e, "display", on ? "inline" : "none"); }

  function setText(e, t) {
    t = t === null || t === undefined ? "" : String(t);
    if (e.textContent !== t) e.textContent = t;
  }

  function dash(v) { return v === null || v === undefined || v === "" ? "-" : String(v); }

  function pad2(n) { return n < 10 ? "0" + n : String(n); }

  function fmtClock(ms) {
    var d = num(ms) !== null ? new Date(ms) : new Date();
    return pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + ":" + pad2(d.getSeconds());
  }

  function fmtAgo(ms) {
    if (num(ms) === null) return "-";
    if (ms < 1000) return Math.round(ms) + " ms";
    if (ms < 60000) return (ms / 1000).toFixed(1) + " s";
    return Math.floor(ms / 60000) + " m " + Math.round((ms % 60000) / 1000) + " s";
  }

  function fmtDuration(ms) {
    if (num(ms) === null) return "-";
    if (ms < 1) return ms.toFixed(2) + " ms";
    if (ms < 1000) return ms.toFixed(ms < 10 ? 1 : 0) + " ms";
    return (ms / 1000).toFixed(2) + " s";
  }

  function fmtMs(v) {
    if (num(v) === null) return "n/a";
    return (v < 10 ? v.toFixed(1) : Math.round(v)) + " ms";
  }

  function fmtNum(v, digits) { return num(v) === null ? "-" : v.toFixed(digits); }

  // Trim float noise from slider steps (0.1 + 0.2).
  function roundStep(v, step) {
    var d = String(step).indexOf(".") >= 0 ? String(step).split(".")[1].length : 0;
    return Number(v.toFixed(d));
  }

  // Compose names replicas "swarm-net-node-3"; the project prefix is noise on
  // a crowded graph. The full id stays in tooltips and the table.
  function shortId(id) {
    return id.indexOf("swarm-net-") === 0 ? id.slice("swarm-net-".length) : id;
  }

  function badge(text, cls) {
    return h("span", "badge " + cls, text);
  }

  // Replace a cell's badges only when their content changed, so a 1 Hz
  // refresh does not churn the DOM.
  function setBadges(td, list) {
    var sig = list.map(function (b) { return b[0] + "|" + b[1]; }).join(",");
    if (td.getAttribute("data-sig") === sig) return;
    td.setAttribute("data-sig", sig);
    td.textContent = "";
    list.forEach(function (b) { td.appendChild(badge(b[0], b[1])); });
  }

  function flash(elm) {
    if (!elm) return;
    elm.classList.remove("flash");
    // Force a reflow so re-adding the class restarts the animation.
    void elm.getBoundingClientRect();
    elm.classList.add("flash");
  }

  function reducedMotion() {
    return !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  }

  function now() {
    return (window.performance && performance.now) ? performance.now() : Date.now();
  }

  // Mirror of geo.DefaultPosition (backend/pkg/geo/geo.go): FNV-1a 64 of the
  // id, the splitmix64 finalizer, three 21-bit fields, each mapped to [5, 95]. Used only when a node
  // reports no position, so the picture matches what the backend would pick.
  var MASK21 = (1 << 21) - 1;
  function defaultPos(id) {
    var ux, uy, uz;
    if (typeof BigInt === "function") {
      var hv = BigInt("0xcbf29ce484222325");
      var prime = BigInt("0x100000001b3");
      var m64 = (BigInt(1) << BigInt(64)) - BigInt(1);
      // UTF-8 bytes, as Go's []byte(id).
      var bytes = unescape(encodeURIComponent(id));
      for (var i = 0; i < bytes.length; i++) {
        hv = ((hv ^ BigInt(bytes.charCodeAt(i))) * prime) & m64;
      }
      // splitmix64 finalizer, as geo.mix64.
      hv ^= hv >> BigInt(30);
      hv = (hv * BigInt("0xbf58476d1ce4e5b9")) & m64;
      hv ^= hv >> BigInt(27);
      hv = (hv * BigInt("0x94d049bb133111eb")) & m64;
      hv ^= hv >> BigInt(31);
      var field = function (shift) { return Number((hv >> BigInt(shift)) & BigInt(MASK21)) / MASK21; };
      ux = field(0); uy = field(21); uz = field(42);
    } else {
      var a = 2166136261;
      for (var j = 0; j < id.length; j++) { a ^= id.charCodeAt(j); a = Math.imul(a, 16777619) >>> 0; }
      ux = (a & 1023) / 1023; uy = ((a >>> 10) & 1023) / 1023; uz = ((a >>> 20) & 1023) / 1023;
    }
    var place = function (u) { return 5 + u * (DEFAULT_SIZE - 10); };
    return { x: place(ux), y: place(uy), z: place(uz) };
  }

  // ---------------------------------------------------------------- state

  var model = {
    nodes: [],          // normalised, sorted by id
    byId: new Map(),
    addr: new Map(),    // node id -> advertise address, learnt from peer lists
    tasks: [],          // normalised, newest first
    sim: null,          // normalised snapshot.sim, or null (older backend)
    atMs: null,         // server time of the last snapshot
    recvAt: 0,          // local time the last snapshot arrived
    prevRole: new Map() // id -> role, to flash role changes between snapshots
  };

  function normPos(p) {
    p = obj(p);
    if (!p) return null;
    var x = num(p.x), y = num(p.y), z = num(p.z);
    if (x === null || y === null || z === null) return null;
    return { x: x, y: y, z: z };
  }

  function normNode(n) {
    if (!n || typeof n !== "object") return null;
    var id = str(n.id);
    if (!id) return null;
    // A missing "connected" is treated as connected: older servers may omit it,
    // and hiding every node would be worse than showing one stale one.
    var connected = n.connected !== false;
    var st = str(n.state).toLowerCase() || "unknown";
    // "killed" wins over "disconnected": the CC knows it killed this node.
    var eff = st === "killed" ? "killed" : !connected ? "disconnected" : (KNOWN_STATES[st] ? st : "unknown");
    var role = str(n.role).toLowerCase() || "unknown";
    var leader = str(n.leader);
    var peerList = arr(n.peers).map(function (p) {
      p = obj(p);
      if (!p || !str(p.id)) return null;
      return { id: str(p.id), addr: str(p.advertise), role: str(p.role), state: str(p.state) };
    }).filter(Boolean);
    var scores = {};
    var sc = obj(n.scores);
    if (sc) for (var k in sc) { if (num(sc[k]) !== null) scores[k] = sc[k]; }
    var flows = arr(n.flows).map(function (f) {
      f = obj(f);
      if (!f) return null;
      var c = num(f.count);
      if (!str(f.to) || c === null || c <= 0) return null;
      return { to: str(f.to), type: str(f.type).toUpperCase(), count: Math.round(c) };
    }).filter(Boolean);
    var pos = normPos(n.pos);
    return {
      id: id,
      role: role,
      isLeader: role === "leader",
      state: st,
      eff: eff,
      term: num(n.term),
      leader: leader,
      degraded: n.degraded === true,
      connected: connected,
      lastSeen: num(n.last_seen_ms),
      dropped: num(n.dropped),
      ledger: num(n.ledger_size),
      peers: Array.isArray(n.peers) ? n.peers.length : null,
      peerList: peerList,
      scores: scores,
      pos: pos,
      threshold: num(n.threshold),
      hysteresis: num(n.hysteresis),
      simVersion: num(n.sim_version),
      flows: flows
    };
  }

  function normSim(o) {
    o = obj(o);
    if (!o) return null;
    var size = num(o.size);
    return {
      version: num(o.version),
      enabled: o.enabled === true ? true : (o.enabled === false ? false : null),
      base_ms: num(o.base_ms),
      per_unit_ms: num(o.per_unit_ms),
      jitter_ms: num(o.jitter_ms),
      threshold: num(o.threshold),
      hysteresis: num(o.hysteresis),
      size: size !== null && size > 0 ? size : DEFAULT_SIZE,
      maxDelay: num(o.max_delay_ms) !== null ? o.max_delay_ms : 1500
    };
  }

  function normTask(t) {
    if (!t || typeof t !== "object") return null;
    var id = str(t.task_id);
    if (!id) return null;
    var out = t.output;
    if (out !== null && out !== undefined && typeof out !== "string") {
      try { out = JSON.stringify(out); } catch (e) { out = String(out); }
    }
    return {
      id: id,
      kind: str(t.kind),
      leader: str(t.leader),
      worker: str(t.worker),
      state: str(t.state).toLowerCase() || "unknown",
      ok: t.ok === true ? true : (t.ok === false ? false : null),
      output: str(out),
      duration: num(t.duration_ms),
      submitted: num(t.submitted_unix_ms)
    };
  }

  function worldSize() { return model.sim ? model.sim.size : DEFAULT_SIZE; }

  // Position in world units: the reported one, else the hash placement.
  // A position the user just dragged wins for a moment, so the next snapshot
  // (which may predate the change) does not snap the drone back.
  var localPos = new Map(); // id -> {x,y,z,at}
  function posOf(n) {
    var lp = localPos.get(n.id);
    if (lp && Date.now() - lp.at < SLIDER_HOLD_MS) return { x: lp.x, y: lp.y, z: lp.z, guess: false };
    if (n.pos) return { x: n.pos.x, y: n.pos.y, z: n.pos.z, guess: false };
    var d = defaultPos(n.id);
    var k = worldSize() / DEFAULT_SIZE;
    return { x: d.x * k, y: d.y * k, z: d.z * k, guess: true };
  }

  // Measured RTT from node a to node b: a's own EWMA, keyed by b's advertise
  // address. A negative or missing score means "not measured".
  function rtt(a, bId) {
    if (!a) return null;
    var addr = "";
    for (var i = 0; i < a.peerList.length; i++) {
      if (a.peerList[i].id === bId) { addr = a.peerList[i].addr; break; }
    }
    if (!addr) addr = model.addr.get(bId) || "";
    if (!addr) return null;
    var v = a.scores[addr];
    return num(v) !== null && v >= 0 ? v : null;
  }

  function dist(a, b) {
    var pa = posOf(a), pb = posOf(b);
    return Math.sqrt((pa.x - pb.x) * (pa.x - pb.x) + (pa.y - pb.y) * (pa.y - pb.y) + (pa.z - pb.z) * (pa.z - pb.z));
  }

  // The model's delay for distance d, without jitter's randomness: its mean
  // is jitter / 2. Only the responder delays its PONG, so a measured RTT is
  // about one delay plus real network time. null = unknown, 0 = emulation off.
  function predicted(d) {
    var sm = model.sim;
    if (!sm || sm.base_ms === null || sm.per_unit_ms === null) return null;
    if (sm.enabled === false) return 0;
    var v = sm.base_ms + d * sm.per_unit_ms + (sm.jitter_ms || 0) / 2;
    return clamp(v, 0, sm.maxDelay);
  }

  function modelLines(d) {
    var sm = model.sim;
    if (!sm || sm.base_ms === null || sm.per_unit_ms === null) return ["model: n/a (no sim config reported)"];
    if (sm.enabled === false) return ["model: emulation off (RTT = network only)"];
    return [
      "model: base + d x per_unit + jitter/2",
      "  = " + sm.base_ms + " + " + d.toFixed(1) + " x " + sm.per_unit_ms +
        " + " + ((sm.jitter_ms || 0) / 2) + " = " + fmtMs(predicted(d)),
      "Only the responder delays its PONG,",
      "so RTT ~ one delay + network time."
    ];
  }

  // ---------------------------------------------------------------- leader colours

  var leaderColour = new Map();
  function assignColours(nodes) {
    var leaders = nodes.filter(function (n) { return n.isLeader && n.eff !== "killed"; }).map(function (n) { return n.id; });
    var keep = new Set(leaders);
    leaderColour.forEach(function (c, id) { if (!keep.has(id)) leaderColour.delete(id); });
    leaders.forEach(function (id) {
      if (leaderColour.has(id)) return;
      var used = new Set(leaderColour.values());
      var pick = null;
      for (var i = 0; i < PALETTE.length; i++) if (!used.has(PALETTE[i])) { pick = PALETTE[i]; break; }
      leaderColour.set(id, pick || PALETTE[leaderColour.size % PALETTE.length]);
    });
  }
  function clusterOf(n) {
    if (!n) return null;
    return n.isLeader ? n.id : (n.leader && leaderColour.has(n.leader) ? n.leader : null);
  }

  // ---------------------------------------------------------------- socket

  var ws = null;
  var attempt = 0;
  var reconnectAt = 0;
  var reconnectTimer = null;
  var lastMsgAt = 0;
  var everOpened = false;

  function wsURL() {
    // Resolve relative to the page so the dashboard also works behind a path
    // prefix; pick ws/wss to match http/https.
    var u = new URL("ws", location.href);
    u.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    return u.href;
  }

  function setConn(state, text) {
    var c = $("conn");
    if (c.getAttribute("data-state") !== state) c.setAttribute("data-state", state);
    setText($("conn-text"), text);
  }

  function connect() {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
    reconnectAt = 0;
    setConn("connecting", attempt > 0 ? "connecting (try " + (attempt + 1) + ")" : "connecting");
    var sock;
    try {
      sock = new WebSocket(wsURL());
    } catch (e) {
      scheduleReconnect();
      return;
    }
    ws = sock;
    sock.onopen = function () {
      if (ws !== sock) return;
      attempt = 0;
      lastMsgAt = Date.now();
      setConn("open", "live");
      logLocal(everOpened ? "reconnected to control center" : "connected to control center");
      everOpened = true;
    };
    sock.onmessage = function (ev) {
      if (ws !== sock) return;
      lastMsgAt = Date.now();
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      handle(msg);
    };
    sock.onclose = function () {
      if (ws !== sock) return; // a socket we already abandoned
      dropSocket(everOpened ? "connection lost" : null);
    };
    sock.onerror = function () { /* onclose follows and handles it */ };
  }

  function dropSocket(reason) {
    var old = ws;
    ws = null;
    if (old) {
      old.onopen = old.onmessage = old.onclose = old.onerror = null;
      try { old.close(); } catch (e) { /* already closed */ }
    }
    if (reason) logLocal(reason);
    scheduleReconnect();
  }

  // Exponential backoff with jitter, so a restarted Control Center is not hit
  // by every open dashboard in the same millisecond.
  function scheduleReconnect() {
    if (reconnectTimer) return;
    var base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * Math.pow(2, attempt));
    var delay = Math.round(base / 2 + Math.random() * base / 2);
    attempt++;
    reconnectAt = Date.now() + delay;
    reconnectTimer = setTimeout(connect, delay);
    updateConnCountdown();
  }

  function updateConnCountdown() {
    if (!reconnectTimer) return;
    var left = Math.max(0, Math.ceil((reconnectAt - Date.now()) / 1000));
    setConn("closed", "offline - retry in " + left + "s");
  }

  function reconnectNow() {
    if (ws) return;
    attempt = 0;
    connect();
  }

  // Once per second: connection watchdog, countdown, and age display.
  function tick() {
    if (ws && ws.readyState === WebSocket.OPEN) {
      var idle = Date.now() - lastMsgAt;
      if (idle > STALE_DROP_MS) {
        dropSocket("no data for " + Math.round(idle / 1000) + "s - reconnecting");
      } else if (idle > STALE_WARN_MS) {
        setConn("stale", "stale (" + Math.round(idle / 1000) + "s)");
      } else {
        setConn("open", "live");
      }
    }
    updateConnCountdown();
    renderAge();
  }

  var HTTP_PATHS = { task: "api/tasks", chaos: "api/chaos", sim: "api/sim" };

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(msg));
      return Promise.resolve("ws");
    }
    // Fall back to the HTTP API so controls still work while the socket is
    // reconnecting. /api/sim takes the same object without "type" (the CC
    // rejects unknown fields, and "type" is only defined for the WS form).
    var body = msg;
    if (msg.type === "sim") {
      body = {};
      for (var k in msg) if (k !== "type") body[k] = msg[k];
    }
    return fetch(HTTP_PATHS[msg.type] || "api/chaos", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (r.ok) return "http";
      return r.text().then(function (t) {
        throw new Error("HTTP " + r.status + (t ? ": " + t.slice(0, 160) : ""));
      });
    });
  }

  // ---------------------------------------------------------------- dispatch

  function handle(msg) {
    if (!msg || typeof msg !== "object") return;
    if (msg.type === "snapshot") onSnapshot(msg);
    else if (msg.type === "event") onEvent(msg);
  }

  function onSnapshot(msg) {
    var nodes = arr(msg.nodes).map(normNode).filter(Boolean);
    nodes.sort(function (a, b) { return a.id < b.id ? -1 : a.id > b.id ? 1 : 0; });
    var byId = new Map();
    nodes.forEach(function (n) { byId.set(n.id, n); });
    var addr = new Map();
    nodes.forEach(function (n) {
      n.peerList.forEach(function (p) { if (p.addr && !addr.has(p.id)) addr.set(p.id, p.addr); });
    });

    var tasks = arr(msg.tasks).map(normTask).filter(Boolean).reverse();

    // Flash nodes whose role flipped since the last snapshot. The server also
    // emits leader_change events; this covers a missed event across a
    // reconnect.
    var changed = [];
    nodes.forEach(function (n) {
      var prev = model.prevRole.get(n.id);
      if (prev !== undefined && prev !== n.role) changed.push(n.id);
    });
    model.prevRole = new Map(nodes.map(function (n) { return [n.id, n.role]; }));

    model.nodes = nodes;
    model.byId = byId;
    model.addr = addr;
    model.tasks = tasks;
    model.sim = normSim(msg.sim);
    model.atMs = num(msg.at_unix_ms);
    model.recvAt = Date.now();
    assignColours(nodes);

    renderSummary();
    renderTopology();
    sync3d();
    spawnFlows(nodes);
    renderNodeTable();
    renderTaskTable();
    renderSimPanel();
    renderInspector();
    changed.forEach(function (id) {
      flashNode(id);
      var n = byId.get(id);
      if (n && n.isLeader) promote(id);
    });
    autoFit();
    kick();
  }

  function onEvent(msg) {
    var kind = str(msg.kind) || "event";
    var node = str(msg.node);
    var detail = str(msg.detail);
    addEvent(kind, node, detail, num(msg.at_unix_ms), false);
    if (node && FLASH_EVENTS[kind]) flashNode(node);
    if (node && kind === "leader_change" && /->\s*leader/.test(detail)) promote(node);
  }

  // ---------------------------------------------------------------- summary

  function renderSummary() {
    var nodes = model.nodes;
    var leaders = nodes.filter(function (n) { return n.isLeader && n.connected; }).length;
    var healthy = nodes.filter(function (n) { return n.eff === "alive"; }).length;
    var pending = model.tasks.filter(function (t) { return t.state === "pending"; }).length;

    setText($("sum-nodes"), nodes.length);
    setText($("sum-leaders"), leaders);
    setText($("sum-healthy"), healthy + " / " + nodes.length);
    setText($("sum-pending"), pending);

    $("sum-leaders").parentNode.classList.toggle("bad", nodes.length > 0 && leaders === 0);
    $("sum-healthy").parentNode.classList.toggle("bad", healthy < nodes.length);
    $("topo-empty").hidden = nodes.length > 0;
    renderAge();
  }

  function renderAge() {
    var el = $("sum-age");
    if (!model.recvAt) { setText(el, "-"); return; }
    var age = Date.now() - model.recvAt;
    setText(el, age < 1500 ? "live" : Math.round(age / 1000) + "s");
    el.parentNode.classList.toggle("bad", age > STALE_WARN_MS);
  }

  // ---------------------------------------------------------------- view + camera

  // Two primary views, picked by the toggle at the top of the Airspace panel:
  //   "2d" -- the default. A top-down map, or the older grouped diagram.
  //   "3d" -- the perspective airspace.
  // `mode` is what is actually drawn: "map" | "grouped" | "3d". The map and
  // 3D share one renderer (render3d); only the projection differs. Each mode
  // keeps its own camera, so switching back returns to where the user was.
  var primary = "2d";
  var layout2d = "map";
  var mode = "map";
  var cams = {
    "3d": { cur: Object.assign({}, HOME_3D), goal: Object.assign({}, HOME_3D), fitted: false },
    // The map and grouped homes already frame everything: no auto-fit.
    map: { cur: Object.assign({}, HOME_FLAT), goal: Object.assign({}, HOME_FLAT), fitted: true },
    grouped: { cur: Object.assign({}, HOME_FLAT), goal: Object.assign({}, HOME_FLAT), fitted: true }
  };
  var CAM_KEYS = {
    "3d": ["yaw", "pitch", "zoom", "panX", "panY", "tx", "ty", "tz"],
    map: ["zoom", "panX", "panY"],
    grouped: ["zoom", "panX", "panY"]
  };

  function camSet(patch, animate) {
    var c = cams[mode];
    for (var k in patch) {
      var v = patch[k];
      if (k === "zoom") v = clamp(v, ZOOM_MIN, ZOOM_MAX);
      if (k === "pitch") v = clamp(v, PITCH_MIN, PITCH_MAX);
      c.goal[k] = v;
      if (!animate || reducedMotion()) c.cur[k] = v;
    }
    sceneDirty = true;
    kick();
  }

  // Ease the current camera toward the goal. Returns true while moving.
  function easeCamera() {
    var c = cams[mode];
    var moving = false;
    CAM_KEYS[mode].forEach(function (k) {
      var d = c.goal[k] - c.cur[k];
      var eps = (k === "panX" || k === "panY") ? 0.3 : 0.0005;
      if (Math.abs(d) <= eps) {
        if (d !== 0) { c.cur[k] = c.goal[k]; sceneDirty = true; }
      } else {
        c.cur[k] += d * 0.2;
        moving = true;
        sceneDirty = true;
      }
    });
    return moving;
  }

  // Zoom by factor, keeping the view point (vx, vy) fixed on screen.
  // Every mode maps screen = centre + pan + zoom * q, so the same maths works.
  function zoomAt(vx, vy, factor, animate) {
    var g = cams[mode][animate ? "goal" : "cur"];
    var z1 = g.zoom;
    var z2 = clamp(z1 * factor, ZOOM_MIN, ZOOM_MAX);
    var qx = (vx - VIEW_W / 2 - g.panX) / z1;
    var qy = (vy - VIEW_H / 2 - g.panY) / z1;
    camSet({ zoom: z2, panX: vx - VIEW_W / 2 - z2 * qx, panY: vy - VIEW_H / 2 - z2 * qy }, animate);
  }

  function panBy(dx, dy, animate) {
    var g = cams[mode][animate ? "goal" : "cur"];
    camSet({ panX: g.panX + dx, panY: g.panY + dy }, animate);
  }

  function orbitBy(dyaw, dpitch, animate) {
    if (mode !== "3d") return;
    var g = cams["3d"][animate ? "goal" : "cur"];
    camSet({ yaw: g.yaw + dyaw, pitch: g.pitch + dpitch }, animate);
  }

  // 3D: back to the home angle, then frame the drones around the home
  // target. 2D: back to the home framing (the whole airspace on the map).
  function resetView() {
    if (mode === "3d") {
      camSet(Object.assign({}, HOME_3D), true);
      fitView(true, true);
    } else {
      camSet(Object.assign({}, HOME_FLAT), true);
    }
  }

  // First visit to a mode with drones on screen: frame them once.
  function autoFit() {
    var c = cams[mode];
    if (c.fitted || !model.nodes.length) return;
    c.fitted = true;
    fitView(true);
  }

  // Frame every drone. keepTarget: keep the orbit centre (used by reset).
  function fitView(animate, keepTarget) {
    var pts = [];
    var S = worldSize();
    if (mode === "3d") {
      var g = cams["3d"].goal;
      var cam = { yaw: g.yaw, pitch: g.pitch, zoom: 1, panX: 0, panY: 0, tx: g.tx, ty: g.ty, tz: g.tz };
      var list = model.nodes.map(posOf);
      if (!keepTarget && list.length) {
        var cx = 0, cy = 0, cz = 0;
        list.forEach(function (p) { cx += p.x; cy += p.y; cz += p.z; });
        cam.tx = cx / list.length / S; cam.ty = cy / list.length / S; cam.tz = cz / list.length / S / 2;
      }
      var B = camBasis(cam);
      list.forEach(function (p) {
        var a = project(B, p.x / S, p.y / S, p.z / S);
        var b = project(B, p.x / S, p.y / S, 0);
        if (a) pts.push(a);
        if (b) pts.push(b);
      });
      if (!pts.length) {
        [0, 1].forEach(function (x) { [0, 1].forEach(function (y) { [0, 1].forEach(function (z) {
          var c = project(B, x, y, z); if (c) pts.push(c);
        }); }); });
      }
      var fit = fitBox(pts, 60);
      camSet({ tx: cam.tx, ty: cam.ty, tz: cam.tz, zoom: fit.zoom, panX: fit.panX, panY: fit.panY }, animate);
      return;
    }
    if (mode === "map") {
      var Bm = basisFor("map", HOME_FLAT);
      model.nodes.forEach(function (n) {
        var p = posOf(n);
        pts.push(project(Bm, p.x / S, p.y / S, p.z / S));
      });
    } else {
      drawn.forEach(function (d) { pts.push({ x: d.tx, y: d.ty }); });
    }
    if (!pts.length) { camSet(Object.assign({}, HOME_FLAT), animate); return; }
    var f2 = fitBox(pts, mode === "map" ? 60 : 50);
    camSet({ zoom: f2.zoom, panX: f2.panX, panY: f2.panY }, animate);
  }

  // pts are screen points at zoom 1 and pan 0.
  function fitBox(pts, margin) {
    var minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
    pts.forEach(function (p) {
      minX = Math.min(minX, p.x); maxX = Math.max(maxX, p.x);
      minY = Math.min(minY, p.y); maxY = Math.max(maxY, p.y);
    });
    var w = Math.max(40, maxX - minX), hh = Math.max(40, maxY - minY);
    var z = clamp(Math.min((VIEW_W - 2 * margin) / w, (VIEW_H - 2 * margin) / hh), ZOOM_MIN, ZOOM_MAX);
    var qx = (minX + maxX) / 2 - VIEW_W / 2;
    var qy = (minY + maxY) / 2 - VIEW_H / 2;
    return { zoom: z, panX: -qx * z, panY: -qy * z };
  }

  function focusOn(id) {
    var n = model.byId.get(id);
    if (!n) return;
    var p = posOf(n), S = worldSize();
    if (mode === "3d") {
      camSet({ tx: p.x / S, ty: p.y / S, tz: p.z / S, panX: 0, panY: 0 }, true);
    } else if (mode === "map") {
      // A map should hold still: pan only when the drone is near an edge.
      var now_ = project(basisFor("map", cams.map.goal), p.x / S, p.y / S, p.z / S);
      if (now_.x > VIEW_W * 0.12 && now_.x < VIEW_W * 0.88 && now_.y > VIEW_H * 0.12 && now_.y < VIEW_H * 0.88) return;
      var q = project(basisFor("map", { zoom: cams.map.goal.zoom, panX: 0, panY: 0 }), p.x / S, p.y / S, p.z / S);
      camSet({ panX: VIEW_W / 2 - q.x, panY: VIEW_H / 2 - q.y }, true);
    } else {
      var d = drawn.get(id);
      if (!d) return;
      var z = cams.grouped.goal.zoom;
      camSet({ panX: -(d.tx - VIEW_W / 2) * z, panY: -(d.ty - VIEW_H / 2) * z }, true);
    }
  }

  // Camera basis for a perspective projection. World: x, y on the ground,
  // z up. The eye orbits the target at CAM_DIST, at angle yaw around z and
  // pitch above the ground.
  function camBasis(c) {
    var cp = Math.cos(c.pitch), sp = Math.sin(c.pitch);
    var cy = Math.cos(c.yaw), sy = Math.sin(c.yaw);
    var fx = -cp * cy, fy = -cp * sy, fz = -sp;          // forward
    var rl = Math.sqrt(fx * fx + fy * fy) || 1;
    var rx = fy / rl, ry = -fx / rl, rz = 0;             // right = forward x up
    return {
      ex: c.tx - fx * CAM_DIST, ey: c.ty - fy * CAM_DIST, ez: c.tz - fz * CAM_DIST,
      fx: fx, fy: fy, fz: fz, rx: rx, ry: ry, rz: rz,
      ux: ry * fz - rz * fy, uy: rz * fx - rx * fz, uz: rx * fy - ry * fx, // up = right x forward
      zoom: c.zoom, panX: c.panX, panY: c.panY
    };
  }

  // The map's camera: orthographic, looking straight down. There is no eye
  // position, so it needs only zoom and pan.
  function basisFor(m, c) {
    if (m === "map") return { ortho: true, zoom: c.zoom, panX: c.panX, panY: c.panY };
    return camBasis(c);
  }

  // World point (unit cube) to view box coordinates. null when behind the eye.
  //
  // Orthographic (the 2D map): screen x from x, screen y from y (north up),
  // and z is ignored, so on-screen distance is ground distance times a
  // constant. `depth` still orders by altitude (higher drones paint on top),
  // and `k` keeps glyphs a steady size that grows gently with zoom.
  function project(B, x, y, z) {
    if (B.ortho) {
      var m = MAP_SCALE * B.zoom;
      return {
        x: VIEW_W / 2 + B.panX + (x - 0.5) * m,
        y: VIEW_H / 2 + B.panY - (y - 0.5) * m,
        depth: 2 - z,
        k: (FOCAL / CAM_DIST) * Math.sqrt(B.zoom)
      };
    }
    var vx = x - B.ex, vy = y - B.ey, vz = z - B.ez;
    var depth = vx * B.fx + vy * B.fy + vz * B.fz;
    if (depth < 0.05) return null;
    var xc = vx * B.rx + vy * B.ry + vz * B.rz;
    var yc = vx * B.ux + vy * B.uy + vz * B.uz;
    var k = FOCAL * B.zoom / depth;
    return { x: VIEW_W / 2 + B.panX + xc * k, y: VIEW_H / 2 + B.panY - yc * k, depth: depth, k: k };
  }

  function loadPref(key, allowed, fallback) {
    try {
      var v = window.localStorage.getItem(key);
      return allowed.indexOf(v) >= 0 ? v : fallback;
    } catch (e) {
      return fallback; // storage disabled or blocked
    }
  }

  function savePref(key, v) {
    try { window.localStorage.setItem(key, v); } catch (e) { /* not persisted; the page still works */ }
  }

  var MODE_TEXT = {
    map: {
      cap: "2D MAP -- top-down, altitude as z on each label",
      aria: "Swarm airspace, 2D top-down map. Screen position is ground position; altitude is written on each label. " +
        "Drag or arrows to pan, wheel, pinch, plus and minus zoom, 0 resets, f fits, v switches to 3D, Escape clears the selection.",
      hint: "Top-down map: on-screen distance is ground distance, altitude is the z on each label, and link labels give the " +
        "true 3D distance. Drag or arrows to pan. Wheel, pinch or + / - to zoom. Click a drone to select it. " +
        "Keys: f fit, 0 reset, v 3D, m messages, Esc."
    },
    grouped: {
      cap: "2D GROUPED -- arranged by cluster, not by position",
      aria: "Swarm clusters, grouped layout: drones arranged by cluster, not by position. " +
        "Drag or arrows to pan, wheel, pinch, plus and minus zoom, 0 resets, f fits, v switches to 3D, Escape clears the selection.",
      hint: "Grouped layout: each leader with its workers; positions are ignored. Drag or arrows to pan. " +
        "Wheel, pinch or + / - to zoom. Keys: f fit, 0 reset, v 3D, m messages, Esc."
    },
    "3d": {
      cap: "3D -- drop lines show altitude",
      aria: "Swarm airspace in 3D. Drag to orbit, shift-drag or right-drag to pan, wheel or pinch to zoom, arrows rotate, " +
        "plus and minus zoom, 0 resets, f fits, v switches to the 2D map, Escape clears the selection.",
      hint: "Drag to orbit. Shift-drag, right-drag or two fingers to pan. Wheel or pinch to zoom. " +
        "Click a drone to select and focus it. Keys: arrows, + / -, 0 reset, f fit, v 2D map, m messages, Esc."
    }
  };

  // Show the current mode: buttons, layers, help text. Idempotent.
  function applyMode() {
    var m = primary === "3d" ? "3d" : layout2d;
    if (m !== mode) {
      releaseAllDots();
      hideTip();
      mode = m;
      currentBasis = null;
      lastCamSig = "";
      lastOrder = "";
      // Snap the grouped positions: they were not animated while hidden.
      if (m === "grouped") drawn.forEach(function (d) { d.x = d.tx; d.y = d.ty; });
      sync3d(); // drone and link labels differ between the map and 3D
    }
    show($("v3d"), m !== "grouped");
    show($("v2d"), m === "grouped");
    $("view-2d").setAttribute("aria-pressed", String(primary === "2d"));
    $("view-3d").setAttribute("aria-pressed", String(primary === "3d"));
    $("layout-map").setAttribute("aria-pressed", String(layout2d === "map"));
    $("layout-grouped").setAttribute("aria-pressed", String(layout2d === "grouped"));
    $("layout-ctl").hidden = primary !== "2d";
    var t = MODE_TEXT[m];
    setText($("mode-cap"), t.cap);
    if ($("topo").getAttribute("aria-label") !== t.aria) $("topo").setAttribute("aria-label", t.aria);
    setText($("topo-hint"), t.hint);
    sceneDirty = true;
    autoFit();
    kick();
  }

  function setView(v) {
    primary = v === "3d" ? "3d" : "2d";
    savePref(VIEW_KEY, primary);
    applyMode();
  }

  function setLayout(l) {
    layout2d = l === "grouped" ? "grouped" : "map";
    savePref(LAYOUT_KEY, layout2d);
    applyMode();
  }

  // ---------------------------------------------------------------- frame loop

  var rafId = 0;
  var sceneDirty = true;

  function kick() {
    if (rafId || document.hidden || typeof requestAnimationFrame !== "function") return;
    rafId = requestAnimationFrame(frame);
  }

  function frame(t) {
    rafId = 0;
    var busy = easeCamera();
    if (mode === "grouped") busy = step2d() || busy;
    else busy = render3d() || busy;
    busy = stepDots(typeof t === "number" ? t : now()) || busy;
    if (busy) kick();
  }

  // ---------------------------------------------------------------- 2D grouped layout

  // Deterministic layout: same membership -> same picture, so a change on
  // screen always means a change in the cluster.
  //   1 leader    -> leader at the centre, its workers on a ring around it
  //   2+ leaders  -> leaders on an inner ring, each leader's workers fanned
  //                  out on an outer arc facing away from the centre
  //   no leader   -> everyone on one ring
  // Workers with no known leader sit in a row along the bottom.
  // Raw coordinates are then scaled uniformly to fit the view box.
  function layout(nodes) {
    var pos = new Map();
    var leaders = nodes.filter(function (n) { return n.isLeader; });
    var leaderSet = new Set(leaders.map(function (n) { return n.id; }));
    var groups = new Map();
    leaders.forEach(function (l) { groups.set(l.id, []); });
    var orphans = [];
    nodes.forEach(function (n) {
      if (n.isLeader) return;
      if (n.leader && leaderSet.has(n.leader)) groups.get(n.leader).push(n);
      else orphans.push(n);
    });

    var raw = new Map();
    var L = leaders.length;
    if (L === 0) {
      var all = nodes.slice();
      var R = Math.max(120, all.length * 14);
      all.forEach(function (n, i) {
        var a = -Math.PI / 2 + (2 * Math.PI * i) / all.length;
        raw.set(n.id, [R * Math.cos(a), R * Math.sin(a)]);
      });
      orphans = [];
    } else if (L === 1) {
      var lid = leaders[0].id;
      raw.set(lid, [0, 0]);
      var ws1 = groups.get(lid);
      var twoRings = ws1.length > 16;
      ws1.forEach(function (n, i) {
        var a = -Math.PI / 2 + (2 * Math.PI * i) / ws1.length;
        var r = 170 + (twoRings && i % 2 ? 60 : 0);
        raw.set(n.id, [r * Math.cos(a), r * Math.sin(a)]);
      });
    } else {
      var R1 = Math.max(110, L * 26);
      leaders.forEach(function (l, i) {
        var a = -Math.PI / 2 + (2 * Math.PI * i) / L;
        raw.set(l.id, [R1 * Math.cos(a), R1 * Math.sin(a)]);
        var ws2 = groups.get(l.id);
        var m = ws2.length;
        if (!m) return;
        var sector = (2 * Math.PI / L) * 0.85;
        var step = Math.min(sector / m, 0.5);
        var stagger = step < 0.22;
        ws2.forEach(function (n, k) {
          var b = a + (k - (m - 1) / 2) * step;
          var r = R1 + 120 + (stagger && k % 2 ? 55 : 0);
          raw.set(n.id, [r * Math.cos(b), r * Math.sin(b)]);
        });
      });
    }

    // Fit the main picture into the view box, leaving room for labels and,
    // if needed, the orphan row.
    var margin = 48;
    var bottom = orphans.length ? 70 : 0;
    var minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
    raw.forEach(function (p) {
      minX = Math.min(minX, p[0]); maxX = Math.max(maxX, p[0]);
      minY = Math.min(minY, p[1]); maxY = Math.max(maxY, p[1]);
    });
    var availW = VIEW_W - 2 * margin;
    var availH = VIEW_H - 2 * margin - bottom;
    var spanX = Math.max(1, maxX - minX);
    var spanY = Math.max(1, maxY - minY);
    var scale = raw.size > 1 ? Math.min(availW / spanX, availH / spanY, 1.4) : 1;
    var cx = (minX + maxX) / 2;
    var cy = (minY + maxY) / 2;
    raw.forEach(function (p, id) {
      pos.set(id, {
        x: VIEW_W / 2 + (p[0] - cx) * scale,
        y: margin + availH / 2 + (p[1] - cy) * scale
      });
    });

    if (orphans.length) {
      var gap = Math.min(90, (VIEW_W - 2 * margin) / orphans.length);
      var x0 = VIEW_W / 2 - (gap * (orphans.length - 1)) / 2;
      orphans.forEach(function (n, i) {
        pos.set(n.id, { x: x0 + i * gap, y: VIEW_H - 40 });
      });
    }
    return { pos: pos, groups: groups };
  }

  // id -> { g, parts..., x, y, tx, ty }
  var drawn = new Map();
  // worker id -> line
  var links = new Map();
  // leader id -> circle
  var hulls = new Map();
  var groupsNow = new Map();

  function nodeRadius(n, total) {
    if (n.isLeader) return total > 30 ? 16 : 22;
    return total > 30 ? 8 : total > 15 ? 11 : 13;
  }

  // Drone glyph shared by both views: drawn at radius R0 inside `body`, which
  // the caller scales. Strokes do not scale (vector-effect in style.css).
  function makeGlyph(id, cls) {
    var g = s("g", { "class": cls, "data-id": id });
    var body = s("g", { "class": "drone-body" });
    var parts = {
      flashRing: s("circle", { "class": "flash-ring", r: R0 + 6 }),
      promo: s("circle", { "class": "promo-ring", r: R0 + 10, display: "none" }),
      // Shown by CSS only when discs overlap on screen (class "stacked").
      edge: s("circle", { "class": "stack-edge", r: R0 + 1.5 }),
      sel: s("circle", { "class": "sel-ring", r: R0 + 8, display: "none" }),
      cring: s("circle", { "class": "cluster-ring", r: R0 + 3.5, display: "none" }),
      halo: s("circle", { "class": "halo", r: R0 + 6, display: "none" }),
      shape: s("circle", { "class": "n-shape", r: R0 }),
      glyph: s("text", { "class": "n-glyph" }),
      cross: s("g", { "class": "n-cross-g", display: "none" })
    };
    var k = R0 * 0.62;
    parts.c1 = s("line", { "class": "n-cross", x1: -k, y1: -k, x2: k, y2: k });
    parts.c2 = s("line", { "class": "n-cross", x1: -k, y1: k, x2: k, y2: -k });
    parts.cross.appendChild(parts.c1);
    parts.cross.appendChild(parts.c2);
    ["flashRing", "promo", "sel", "cring", "halo", "shape", "edge", "glyph", "cross"].forEach(function (p) {
      body.appendChild(parts[p]);
    });
    // The label is the short id plus, on the 2D map, the altitude ("z 42"):
    // a top-down view cannot show height any other way.
    var label = s("text", { "class": "n-label" });
    var name = s("tspan");
    var alt = s("tspan", { "class": "n-alt", dx: "5" });
    label.appendChild(name);
    label.appendChild(alt);
    var sub = s("text", { "class": "n-label n-sublabel" });
    g.appendChild(body);
    g.appendChild(label);
    g.appendChild(sub);
    parts.g = g;
    parts.body = body;
    parts.label = label;
    parts.name = name;
    parts.alt = alt;
    parts.sub = sub;
    parts.sig = "";
    parts.r = -1;
    return parts;
  }

  function altText(d, n) {
    if (mode !== "map" || !d.drop) return ""; // only the map's drones (not the grouped glyphs)
    return "z " + Math.round(posOf(n).z);
  }

  function styleGlyph(d, n) {
    var cl = clusterOf(n);
    var colour = cl ? leaderColour.get(cl) : "";
    var promoted = promoted_.has(n.id);
    var alt = altText(d, n);
    setText(d.alt, alt);
    var sig = [n.eff, n.isLeader, n.degraded, n.term, n.role, n.leader, colour, promoted].join("|");
    if (sig === d.sig) return;
    d.sig = sig;

    attr(d.shape, "class", "n-shape st-" + n.eff + (n.isLeader ? " leader-shape" : ""));
    show(d.halo, n.degraded);
    show(d.promo, promoted);
    if (colour && n.eff !== "killed") {
      attr(d.cring, "stroke", colour);
      show(d.cring, true);
    } else {
      show(d.cring, false);
    }
    // Leaders carry an "L" glyph; colour is never the only cue.
    setText(d.glyph, n.isLeader && n.eff !== "disconnected" && n.eff !== "killed" ? "L" : "");
    var crossed = n.eff === "dead" || n.eff === "killed";
    show(d.cross, crossed);
    attr(d.c1, "class", "n-cross" + (n.eff === "killed" ? " killed" : ""));
    attr(d.c2, "class", "n-cross" + (n.eff === "killed" ? " killed" : ""));

    setText(d.name, shortId(n.id));
    var parts = [];
    if (promoted) parts.push("NEW LEADER");
    if (n.eff !== "alive") parts.push(n.eff.toUpperCase());
    if (n.degraded) parts.push("DELAY");
    if (n.isLeader && n.term !== null) parts.push("t" + n.term);
    setText(d.sub, parts.join(" "));
    attr(d.sub, "class", "n-label n-sublabel" + (n.degraded ? " deg" : "") + (promoted ? " promo" : ""));
  }

  function sizeGlyph(d, r) {
    if (Math.abs(d.r - r) < 0.15) return;
    d.r = r;
    attr(d.body, "transform", "scale(" + (r / R0).toFixed(3) + ")");
    attr(d.label, "y", (r + 14).toFixed(1));
    attr(d.sub, "y", (r + 26).toFixed(1));
  }

  function renderTopology() {
    var nodes = model.nodes;
    var total = nodes.length;
    var L = layout(nodes);
    groupsNow = L.groups;

    var seen = new Set();
    nodes.forEach(function (n) {
      seen.add(n.id);
      var d = drawn.get(n.id);
      var p = L.pos.get(n.id) || { x: VIEW_W / 2, y: VIEW_H / 2 };
      if (!d) {
        d = makeGlyph(n.id, "topo-node");
        $("topo-nodes").appendChild(d.g);
        drawn.set(n.id, d);
        d.x = p.x;
        d.y = p.y;
      }
      d.tx = p.x;
      d.ty = p.y;
      styleGlyph(d, n);
      sizeGlyph(d, nodeRadius(n, total));
      attr(d.g, "class", "topo-node" + (n.id === selectedId ? " selected" : ""));
      show(d.sel, n.id === selectedId);
    });
    drawn.forEach(function (d, id) {
      if (!seen.has(id)) { d.g.remove(); drawn.delete(id); }
    });

    // Links: worker -> its leader.
    var want = new Set();
    nodes.forEach(function (n) {
      if (n.isLeader || !n.leader || !drawn.has(n.leader) || n.leader === n.id) return;
      want.add(n.id);
      var line = links.get(n.id);
      if (!line) {
        line = s("line", { "class": "link" });
        $("topo-links").appendChild(line);
        links.set(n.id, line);
      }
      var lead = model.byId.get(n.leader);
      var bad = n.eff !== "alive" || !lead || lead.eff !== "alive";
      attr(line, "class", "link" + (bad ? " bad" : ""));
      attr(line, "data-leader", n.leader);
      attr(line, "data-link", n.id);
      attr(line, "stroke", leaderColour.get(n.leader) || NEUTRAL);
    });
    links.forEach(function (line, id) {
      if (!want.has(id)) { line.remove(); links.delete(id); }
    });

    // Cluster boundaries: one dashed circle per leader enclosing its workers.
    groupsNow.forEach(function (members, lid) {
      if (!hulls.has(lid)) {
        var c = s("circle", { "class": "cluster-hull" });
        $("topo-clusters").appendChild(c);
        hulls.set(lid, c);
      }
      var col = leaderColour.get(lid) || NEUTRAL;
      attr(hulls.get(lid), "stroke", col);
      attr(hulls.get(lid), "fill", col);
    });
    hulls.forEach(function (c, lid) {
      if (!groupsNow.has(lid)) { c.remove(); hulls.delete(lid); }
    });
    sceneDirty = true;
  }

  // Ease every node toward its target; lines and hulls follow the eased
  // positions so nothing jumps. Returns true while anything moves.
  function step2d() {
    var c = cams.grouped.cur;
    attr($("v2d-cam"), "transform",
      "translate(" + (VIEW_W / 2 + c.panX).toFixed(1) + " " + (VIEW_H / 2 + c.panY).toFixed(1) + ") " +
      "scale(" + c.zoom.toFixed(4) + ") translate(" + (-VIEW_W / 2) + " " + (-VIEW_H / 2) + ")");
    var moving = false;
    var reduce = reducedMotion();
    drawn.forEach(function (d) {
      var dx = d.tx - d.x;
      var dy = d.ty - d.y;
      if (reduce || Math.abs(dx) + Math.abs(dy) < 0.3) {
        d.x = d.tx;
        d.y = d.ty;
      } else {
        d.x += dx * 0.14;
        d.y += dy * 0.14;
        moving = true;
      }
      attr(d.g, "transform", "translate(" + d.x.toFixed(1) + " " + d.y.toFixed(1) + ")");
    });
    links.forEach(function (line, wid) {
      var w = drawn.get(wid);
      var l = drawn.get(line.getAttribute("data-leader"));
      if (!w || !l) return;
      attr(line, "x1", w.x.toFixed(1));
      attr(line, "y1", w.y.toFixed(1));
      attr(line, "x2", l.x.toFixed(1));
      attr(line, "y2", l.y.toFixed(1));
    });
    hulls.forEach(function (circ, lid) {
      var l = drawn.get(lid);
      var members = groupsNow.get(lid) || [];
      if (!l) return;
      var pts = [l].concat(members.map(function (m) { return drawn.get(m.id); }).filter(Boolean));
      var cx = 0, cy = 0;
      pts.forEach(function (p) { cx += p.x; cy += p.y; });
      cx /= pts.length;
      cy /= pts.length;
      var r = 0;
      pts.forEach(function (p) { r = Math.max(r, Math.hypot(p.x - cx, p.y - cy)); });
      attr(circ, "cx", cx.toFixed(1));
      attr(circ, "cy", cy.toFixed(1));
      attr(circ, "r", (r + 34).toFixed(1));
    });
    return moving;
  }

  function flashNode(id) {
    var d = drawn.get(id);
    if (d) flash(d.g);
    var d3 = drones3.get(id);
    if (d3) flash(d3.g);
    var row = nodeRows.get(id);
    if (row) flash(row.tr);
  }

  // A new leader wears a pulsing ring and a "NEW LEADER" tag for a while.
  var promoted_ = new Map(); // id -> timer
  function promote(id) {
    clearTimeout(promoted_.get(id));
    promoted_.set(id, setTimeout(function () {
      promoted_.delete(id);
      restyle(id);
    }, 4000));
    restyle(id);
  }
  function restyle(id) {
    var n = model.byId.get(id);
    if (!n) return;
    var d = drawn.get(id);
    if (d) styleGlyph(d, n);
    var d3 = drones3.get(id);
    if (d3) styleGlyph(d3, n);
    sceneDirty = true;
    kick();
  }

  // ---------------------------------------------------------------- 3D view

  var drones3 = new Map(); // id -> glyph parts + drop line + positions
  var links3 = new Map();  // worker id -> {line, hit, label, t1, t2, leader}
  var peers3 = new Map();  // "a|b" -> {line, hit, label, t1, a, b}
  var hulls3 = new Map();  // leader id -> polygon
  var grid = null;
  var lastCamSig = "";
  var lastOrder = "";
  var showPeers = false;
  var showLabels = true;

  function buildGrid() {
    var root = $("g3-grid");
    grid = { ground: [], edges: [], axes: [] };
    for (var i = 0; i <= 10; i++) {
      var a = s("line", { "class": i % 5 === 0 ? "grid major" : "grid" });
      var b = s("line", { "class": i % 5 === 0 ? "grid major" : "grid" });
      root.appendChild(a);
      root.appendChild(b);
      grid.ground.push([a, [i / 10, 0, 0], [i / 10, 1, 0]]);
      grid.ground.push([b, [0, i / 10, 0], [1, i / 10, 0]]);
    }
    var E = [
      [[0, 0, 1], [1, 0, 1]], [[1, 0, 1], [1, 1, 1]], [[1, 1, 1], [0, 1, 1]], [[0, 1, 1], [0, 0, 1]],
      [[0, 0, 0], [0, 0, 1]], [[1, 0, 0], [1, 0, 1]], [[1, 1, 0], [1, 1, 1]], [[0, 1, 0], [0, 1, 1]]
    ];
    E.forEach(function (e) {
      var l = s("line", { "class": "cube" });
      root.appendChild(l);
      grid.edges.push([l, e[0], e[1]]);
    });
    // Axis names: [text, 3D anchor, map anchor or null (hidden on the map)].
    // On the map, each name sits beside the middle of its tick row.
    [["x", [1.07, 0, 0], [0.5, -0.075, 0]], ["y", [0, 1.07, 0], [-0.085, 0.5, 0]],
      ["z", [0, 0, 1.07], null], ["0", [-0.03, -0.03, 0], null]].forEach(function (t) {
      var tx = s("text", { "class": "axis-label" });
      tx.textContent = t[0];
      root.appendChild(tx);
      grid.axes.push([tx, t[1], t[2]]);
    });
    // Map only: the airspace boundary and 0..size ticks along x and y.
    grid.bound = s("polygon", { "class": "map-bound" });
    root.appendChild(grid.bound);
    grid.ticks = [];
    for (var j = 0; j <= 10; j++) {
      var tk = s("text", { "class": "tick-label" });
      root.appendChild(tk);
      grid.ticks.push([tk, [j / 10, -0.035, 0], j / 10]);
      if (j === 0) continue; // one "0" at the corner is enough
      var ty = s("text", { "class": "tick-label tick-y" });
      root.appendChild(ty);
      grid.ticks.push([ty, [-0.018, j / 10, 0], j / 10]);
    }
  }

  function fmtTick(v) { return String(Math.round(v * 10) / 10); }

  function drawGrid(B) {
    if (!grid) buildGrid();
    var flat = !!B.ortho;
    // Seen from straight above, the cube's vertical edges collapse to points
    // and its top face sits on the boundary: the map draws neither.
    grid.ground.concat(flat ? [] : grid.edges).forEach(function (g) {
      var p = project(B, g[1][0], g[1][1], g[1][2]);
      var q = project(B, g[2][0], g[2][1], g[2][2]);
      if (!p || !q) { show(g[0], false); return; }
      show(g[0], true);
      attr(g[0], "x1", p.x.toFixed(1)); attr(g[0], "y1", p.y.toFixed(1));
      attr(g[0], "x2", q.x.toFixed(1)); attr(g[0], "y2", q.y.toFixed(1));
    });
    if (flat) grid.edges.forEach(function (g) { show(g[0], false); });
    grid.axes.forEach(function (a) {
      var at = flat ? a[2] : a[1];
      var p = at && project(B, at[0], at[1], at[2]);
      show(a[0], !!p);
      if (p) { attr(a[0], "x", p.x.toFixed(1)); attr(a[0], "y", p.y.toFixed(1)); }
    });
    show(grid.bound, flat);
    if (flat) {
      attr(grid.bound, "points", [[0, 0], [1, 0], [1, 1], [0, 1]].map(function (c) {
        var p = project(B, c[0], c[1], 0);
        return p.x.toFixed(1) + "," + p.y.toFixed(1);
      }).join(" "));
    }
    var S = worldSize();
    grid.ticks.forEach(function (t) {
      show(t[0], flat);
      if (!flat) return;
      var p = project(B, t[1][0], t[1][1], 0);
      attr(t[0], "x", p.x.toFixed(1));
      attr(t[0], "y", p.y.toFixed(1));
      setText(t[0], fmtTick(S * t[2]));
    });
  }

  function makeLabel(parent) {
    var label = s("text", { "class": "link-label" });
    var t1 = s("tspan", { dy: "0" });
    var t2 = s("tspan", { dy: "1.15em" });
    label.appendChild(t1);
    label.appendChild(t2);
    parent.appendChild(label);
    return { label: label, t1: t1, t2: t2 };
  }

  // Structural update, once per snapshot: create and remove elements, set
  // classes and label text. Geometry is set per frame in render3d.
  function sync3d() {
    var S = worldSize();
    var reduce = reducedMotion();
    var seen = new Set();
    model.nodes.forEach(function (n) {
      seen.add(n.id);
      var d = drones3.get(n.id);
      var p = posOf(n);
      if (!d) {
        d = makeGlyph(n.id, "drone");
        d.drop = s("line", { "class": "drop" });
        d.shadow = s("circle", { "class": "shadow", r: 2.5 });
        $("g3-drops").appendChild(d.drop);
        $("g3-drops").appendChild(d.shadow);
        $("g3-nodes").appendChild(d.g);
        d.wx = p.x / S; d.wy = p.y / S; d.wz = p.z / S;
        drones3.set(n.id, d);
        lastOrder = "";
      }
      d.tx = p.x / S; d.ty = p.y / S; d.tz = p.z / S;
      if (reduce) { d.wx = d.tx; d.wy = d.ty; d.wz = d.tz; }
      d.guess = p.guess;
      d.node = n;
      styleGlyph(d, n);
      // The label slot and "stacked" classes are added per frame (layoutLabels).
      d.baseCls = "drone" + (n.id === selectedId ? " selected" : "") + (n.isLeader ? " is-leader" : "");
      attr(d.g, "class", d.baseCls + (d.slot ? " lbl-" + d.slot : "") + (d.stackedNow ? " stacked" : ""));
      show(d.sel, n.id === selectedId);
      attr(d.drop, "class", "drop" + (n.eff === "alive" ? "" : " faint"));
    });
    drones3.forEach(function (d, id) {
      if (seen.has(id)) return;
      d.g.remove(); d.drop.remove(); d.shadow.remove();
      drones3.delete(id);
      releaseDotsFor(id);
      if (selectedId === id) select(null, false);
    });

    // Worker -> leader links, in the leader's colour.
    var want = new Set();
    model.nodes.forEach(function (n) {
      if (n.isLeader || !n.leader || n.leader === n.id || !drones3.has(n.leader)) return;
      if (n.eff === "killed") return;
      want.add(n.id);
      var L = links3.get(n.id);
      if (!L) {
        L = makeLabel($("g3-labels"));
        L.line = s("line", { "class": "link3" });
        L.hit = s("line", { "class": "hit", "data-link": n.id });
        $("g3-links").appendChild(L.line);
        $("g3-links").appendChild(L.hit);
        links3.set(n.id, L);
      }
      L.leader = n.leader;
      var lead = model.byId.get(n.leader);
      var bad = n.eff !== "alive" || !lead || lead.eff !== "alive";
      var hl = selectedId === n.id || selectedId === n.leader;
      attr(L.line, "class", "link3" + (bad ? " bad" : "") + (hl ? " hl" : ""));
      attr(L.line, "stroke", leaderColour.get(n.leader) || NEUTRAL);
      // Always the true 3D distance: that is what drives the latency model,
      // even when the map only shows the ground distance.
      var dd = dist(n, lead);
      setText(L.t1, dd.toFixed(1) + " u" + (mode === "map" ? " (3D)" : ""));
      var pr = predicted(dd);
      setText(L.t2, "rtt " + fmtMs(rtt(n, n.leader)) + (pr === null ? "" : " | model " + fmtMs(pr)));
    });
    links3.forEach(function (L, id) {
      if (want.has(id)) return;
      L.line.remove(); L.hit.remove(); L.label.remove();
      links3.delete(id);
    });

    // Peer links: every pair some node lists as a peer. Built only when they
    // can be seen: the toggle is on, or a drone is selected.
    var wantP = new Set();
    if (showPeers || selectedId) {
      model.nodes.forEach(function (n) {
        if (n.eff === "killed") return;
        n.peerList.forEach(function (p) {
          if (!drones3.has(p.id) || p.id === n.id) return;
          if (!showPeers && n.id !== selectedId && p.id !== selectedId) return;
          var a = n.id < p.id ? n.id : p.id, b = n.id < p.id ? p.id : n.id;
          var key = a + "|" + b;
          if (wantP.has(key)) return;
          wantP.add(key);
          var P = peers3.get(key);
          if (!P) {
            P = makeLabel($("g3-labels"));
            P.line = s("line", { "class": "peerlink" });
            P.hit = s("line", { "class": "hit", "data-peer": key });
            $("g3-peers").appendChild(P.line);
            $("g3-peers").appendChild(P.hit);
            P.a = a; P.b = b;
            peers3.set(key, P);
          }
          var sel = selectedId === a || selectedId === b;
          attr(P.line, "class", "peerlink" + (sel ? " hl" : ""));
          // The worker-leader link already carries a label for this pair.
          var na = model.byId.get(a), nb = model.byId.get(b);
          var leaderPair = (na && na.leader === b && links3.has(a)) || (nb && nb.leader === a && links3.has(b));
          P.showLabel = sel && !leaderPair;
          if (sel) {
            var me = model.byId.get(selectedId);
            var other = model.byId.get(selectedId === a ? b : a);
            var d2 = other ? dist(me, other) : 0;
            setText(P.t1, d2.toFixed(1) + " u" + (mode === "map" ? " (3D)" : ""));
            setText(P.t2, "rtt " + fmtMs(rtt(me, selectedId === a ? b : a)));
          }
        });
      });
    }
    peers3.forEach(function (P, key) {
      if (wantP.has(key)) return;
      P.line.remove(); P.hit.remove(); P.label.remove();
      peers3.delete(key);
    });

    // Cluster boundaries: the convex hull of each cluster's ground shadows.
    groupsNow.forEach(function (members, lid) {
      if (!hulls3.has(lid)) {
        var poly = s("polygon", { "class": "hull3" });
        $("g3-hulls").appendChild(poly);
        hulls3.set(lid, poly);
      }
      var col = leaderColour.get(lid) || NEUTRAL;
      attr(hulls3.get(lid), "stroke", col);
      attr(hulls3.get(lid), "fill", col);
    });
    hulls3.forEach(function (poly, lid) {
      if (!groupsNow.has(lid)) { poly.remove(); hulls3.delete(lid); }
    });

    sceneDirty = true;
  }

  function convexHull(pts) {
    if (pts.length < 3) return pts.slice();
    pts = pts.slice().sort(function (a, b) { return a.x - b.x || a.y - b.y; });
    var cross = function (o, a, b) { return (a.x - o.x) * (b.y - o.y) - (a.y - o.y) * (b.x - o.x); };
    var lower = [], upper = [], i;
    for (i = 0; i < pts.length; i++) {
      while (lower.length >= 2 && cross(lower[lower.length - 2], lower[lower.length - 1], pts[i]) <= 0) lower.pop();
      lower.push(pts[i]);
    }
    for (i = pts.length - 1; i >= 0; i--) {
      while (upper.length >= 2 && cross(upper[upper.length - 2], upper[upper.length - 1], pts[i]) <= 0) upper.pop();
      upper.push(pts[i]);
    }
    upper.pop();
    lower.pop();
    return lower.concat(upper);
  }

  function setLine(el, p, q) {
    if (!p || !q) { show(el, false); return false; }
    show(el, true);
    attr(el, "x1", p.x.toFixed(1)); attr(el, "y1", p.y.toFixed(1));
    attr(el, "x2", q.x.toFixed(1)); attr(el, "y2", q.y.toFixed(1));
    return true;
  }

  // ---------------------------------------------------------------- label layout
  //
  // Greedy, per frame, in view-box pixels. Drone labels go first, then link
  // labels, each into the first free slot:
  //   drones: the previous frame's slot if still free, else right, left,
  //           above, below; if nothing is free, the slot overlapping least
  //           (a drone is never left unnamed)
  //   links:  the previous position if still free, else points along the
  //           link, midpoint first; if nothing is free the label is hidden
  //           (the link's hover tooltip still has it)
  // "Free" = no overlap with a label placed earlier or another drone's disc.
  // The selected and hovered items are placed first, so they win conflicts,
  // and the selected drone's links are always labelled. Everything else is
  // placed in id order, so the result is deterministic and does not jitter.
  // Text boxes are estimated from character counts (the fonts are
  // monospace), which costs nothing and needs no layout pass.

  var LABEL_MIN_PX = 70;       // shorter links on screen get no label
  var CH_NAME = 7.3, CH_ALT = 6.7, CH_SUB = 6.1, CH_LINK = 6.1; // px per char (mono 12/11/10/10)
  var DRONE_SLOTS = ["r", "l", "a", "b"];
  var LINK_TS = [0.5, 0.36, 0.64, 0.24, 0.76];
  var hoverLink = null;        // "w:<worker id>" or "p:<a|b>" while a link is hovered
  var labelBoxes = [];         // last layout, for the test hook

  function boxOverlap(a, b) {
    var w = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x);
    var hh = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
    return w > 0 && hh > 0 ? w * hh : 0;
  }

  // Overlap with everything placed so far, plus any part outside the view.
  function boxCost(box, taken, self) {
    var c = 0;
    for (var i = 0; i < taken.length; i++) if (taken[i].owner !== self) c += boxOverlap(box, taken[i]);
    var inside = boxOverlap(box, { x: 0, y: 0, w: VIEW_W, h: VIEW_H });
    return c + (box.w * box.h - inside);
  }

  function droneLabelSize(d) {
    var name = d.name.textContent.length, alt = d.alt.textContent.length, sub = d.sub.textContent.length;
    var w = name * CH_NAME + (alt ? 5 + alt * CH_ALT : 0);
    return { w: Math.max(w, sub * CH_SUB) + 6, h: sub ? 28 : 16 };
  }

  function droneSlotBox(d, slot, sz) {
    var R = d.clear + 5, x = d.p.x, y = d.p.y;
    if (slot === "r") return { x: x + R, y: y - sz.h / 2, w: sz.w, h: sz.h };
    if (slot === "l") return { x: x - R - sz.w, y: y - sz.h / 2, w: sz.w, h: sz.h };
    if (slot === "a") return { x: x - sz.w / 2, y: y - R - sz.h, w: sz.w, h: sz.h };
    return { x: x - sz.w / 2, y: y + R - 3, w: sz.w, h: sz.h };
  }

  // Move the two text lines into the chosen box (coordinates are relative
  // to the drone; the anchor class on the drone's <g> sets text-anchor).
  function applyDroneSlot(d, slot, box) {
    var lx = slot === "r" ? box.x + 3 : slot === "l" ? box.x + box.w - 3 : box.x + box.w / 2;
    lx -= d.p.x;
    var top = box.y - d.p.y;
    attr(d.label, "x", lx.toFixed(1)); attr(d.label, "y", (top + 12).toFixed(1));
    attr(d.sub, "x", lx.toFixed(1)); attr(d.sub, "y", (top + 24).toFixed(1));
    d.slot = slot;
  }

  function droneRank(d) {
    var id = d.node ? d.node.id : "";
    if (id === selectedId) return 0;
    if (id === hoverId) return 1;
    return d.node && d.node.isLeader ? 2 : 3;
  }

  function byRankThenKey(a, b) { return a.rank - b.rank || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0); }

  // list: the drones on screen, already projected and sized.
  // links: [{L, p, q, visible, force, key}]
  function layoutLabels(list, links) {
    var taken = [];
    // Every disc is an obstacle. Discs that overlap on screen are "stacked":
    // they get a see-through fill and an outline, so both stay visible.
    list.forEach(function (d) {
      d.stackedNow = false;
      d.clear = d.r; // radius the label must clear: its own disc, or the whole stack
      var pad = d.r + 3;
      taken.push({ x: d.p.x - pad, y: d.p.y - pad, w: 2 * pad, h: 2 * pad, owner: d, kind: "disc" });
    });
    for (var i = 0; i < list.length; i++) {
      for (var j = i + 1; j < list.length; j++) {
        var a = list[i], b = list[j];
        var dd = Math.hypot(a.p.x - b.p.x, a.p.y - b.p.y);
        if (dd < a.r + b.r) {
          a.stackedNow = true; b.stackedNow = true;
          a.clear = Math.max(a.clear, dd + b.r + 3);
          b.clear = Math.max(b.clear, dd + a.r + 3);
        }
      }
    }

    var order = list.map(function (d) { return { d: d, rank: droneRank(d), key: d.node ? d.node.id : "" }; });
    order.sort(byRankThenKey);
    order.forEach(function (o) {
      var d = o.d;
      var sz = droneLabelSize(d);
      var tries = d.slot ? [d.slot].concat(DRONE_SLOTS.filter(function (s2) { return s2 !== d.slot; })) : DRONE_SLOTS;
      var best = null, bestCost = Infinity, bestSlot = null;
      for (var k = 0; k < tries.length; k++) {
        var box = droneSlotBox(d, tries[k], sz);
        var cost = boxCost(box, taken, d);
        if (cost < bestCost) { best = box; bestCost = cost; bestSlot = tries[k]; }
        if (cost === 0) break;
      }
      applyDroneSlot(d, bestSlot, best);
      best.owner = d; best.kind = "drone"; best.id = o.key;
      taken.push(best);
      attr(d.g, "class", d.baseCls + " lbl-" + bestSlot + (d.stackedNow ? " stacked" : ""));
    });

    links.forEach(function (it) { it.rank = it.force ? 0 : it.first ? 1 : 2; });
    links.slice().sort(byRankThenKey).forEach(function (it) {
      var L = it.L, p = it.p, q = it.q;
      var len = p && q ? Math.hypot(q.x - p.x, q.y - p.y) : 0;
      // A forced label (the selected drone's link, a hovered link) shows even
      // on a short link; it may then sit beside the link instead of on it.
      if (!it.visible || !p || !q || (len < LABEL_MIN_PX && !it.force)) { show(L.label, false); return; }
      var w = Math.max(L.t1.textContent.length, L.t2.textContent.length) * CH_LINK + 6;
      var spots = LINK_TS.map(function (t) { return [t, 0]; });
      if (it.force && len > 0) {
        // Beside the midpoint, on either side, clear of the endpoint discs.
        [30, -30, 48, -48].forEach(function (o) { spots.push([0.5, o]); });
      }
      // The previous frame's spot first (a stable sort keeps the rest in order).
      var prev = L.t !== undefined ? L.t + "," + L.off : "";
      spots.sort(function (a, b) { return (b[0] + "," + b[1] === prev) - (a[0] + "," + a[1] === prev); });
      var nx = len ? -(q.y - p.y) / len : 0, ny = len ? (q.x - p.x) / len : 0;
      var best = null, bestCost = Infinity, bestT = null, bestOff = 0;
      for (var k = 0; k < spots.length; k++) {
        var mx = p.x + (q.x - p.x) * spots[k][0] + nx * spots[k][1];
        var my = p.y + (q.y - p.y) * spots[k][0] + ny * spots[k][1];
        var box = { x: mx - w / 2, y: my - 16, w: w, h: 26 };
        var cost = boxCost(box, taken, null);
        if (cost < bestCost) { best = box; bestCost = cost; bestT = spots[k][0]; bestOff = spots[k][1]; }
        if (cost === 0) break;
      }
      if (bestCost > 0 && !it.force) { show(L.label, false); return; }
      show(L.label, true);
      var lx = (best.x + w / 2).toFixed(1), ly = (best.y + 10).toFixed(1);
      attr(L.label, "x", lx); attr(L.label, "y", ly);
      attr(L.t1, "x", lx); attr(L.t2, "x", lx);
      L.t = bestT; L.off = bestOff;
      best.kind = "link"; best.id = it.key;
      taken.push(best);
    });
    labelBoxes = taken;
  }

  // Per-frame geometry. Returns true while drones are still easing.
  function render3d() {
    var moving = false;
    var reduce = reducedMotion();
    drones3.forEach(function (d) {
      var dx = d.tx - d.wx, dy = d.ty - d.wy, dz = d.tz - d.wz;
      if (reduce || Math.abs(dx) + Math.abs(dy) + Math.abs(dz) < 0.0008) {
        if (dx || dy || dz) sceneDirty = true;
        d.wx = d.tx; d.wy = d.ty; d.wz = d.tz;
      } else {
        d.wx += dx * 0.12; d.wy += dy * 0.12; d.wz += dz * 0.12;
        moving = true;
        sceneDirty = true;
      }
    });
    // The map and 3D share this renderer; only the camera basis differs.
    var c = cams[mode].cur;
    var sig = mode + ":" + worldSize() + ":" + CAM_KEYS[mode].map(function (k) { return c[k].toFixed(4); }).join(",");
    if (sig !== lastCamSig) { sceneDirty = true; }
    if (!sceneDirty) return moving;
    sceneDirty = false;
    var B = basisFor(mode, c);
    if (sig !== lastCamSig) { drawGrid(B); lastCamSig = sig; }
    currentBasis = B;

    var total = drones3.size;
    var list = [];
    drones3.forEach(function (d) {
      d.p = project(B, d.wx, d.wy, d.wz);
      d.gp = project(B, d.wx, d.wy, 0);
      if (!d.p) {
        show(d.g, false); show(d.drop, false); show(d.shadow, false);
        return;
      }
      show(d.g, true);
      list.push(d);
      var n = d.node;
      var base = n && n.isLeader ? (total > 30 ? 11 : 14) : (total > 30 ? 6 : 8.5);
      var r = base * clamp(d.p.k / (FOCAL / CAM_DIST), 0.45, 2.6);
      sizeGlyph(d, r);
      attr(d.g, "transform", "translate(" + d.p.x.toFixed(1) + " " + d.p.y.toFixed(1) + ")");
      // Drop lines show altitude in 3D; from straight above they have no length.
      if (!B.ortho && setLine(d.drop, d.p, d.gp)) {
        show(d.shadow, true);
        attr(d.shadow, "cx", d.gp.x.toFixed(1));
        attr(d.shadow, "cy", d.gp.y.toFixed(1));
      } else {
        show(d.drop, false);
        show(d.shadow, false);
      }
    });

    // Painter's algorithm: far drones first.
    list.sort(function (a, b) { return b.p.depth - a.p.depth; });
    var order = list.map(function (d) { return d.g.getAttribute("data-id"); }).join("\n");
    if (order !== lastOrder) {
      lastOrder = order;
      var root = $("g3-nodes");
      list.forEach(function (d) { root.appendChild(d.g); });
    }

    var labelled = [];
    links3.forEach(function (L, wid) {
      var w = drones3.get(wid), l = drones3.get(L.leader);
      var p = w && w.p, q = l && l.p;
      setLine(L.line, p, q);
      setLine(L.hit, p, q);
      // With a selection, only the selected drone's links keep their labels,
      // and those always show.
      var sel = !!selectedId && (selectedId === wid || selectedId === L.leader);
      labelled.push({ L: L, p: p, q: q, key: "w:" + wid,
        visible: showLabels && (!selectedId || sel), force: sel || hoverLink === "w:" + wid });
    });
    peers3.forEach(function (P, key) {
      var a = drones3.get(P.a), b = drones3.get(P.b);
      var p = a && a.p, q = b && b.p;
      setLine(P.line, p, q);
      setLine(P.hit, p, q);
      // Peer labels exist only for the selected drone's links: placed early,
      // but hidden when there is no room (there can be dozens).
      labelled.push({ L: P, p: p, q: q, key: "p:" + key, visible: showLabels && P.showLabel,
        first: true, force: hoverLink === "p:" + key });
    });
    layoutLabels(list, labelled);
    hulls3.forEach(function (poly, lid) {
      var members = [lid].concat((groupsNow.get(lid) || []).map(function (m) { return m.id; }));
      var pts = [];
      members.forEach(function (id) {
        var d = drones3.get(id);
        if (d && d.gp && d.node && d.node.eff !== "killed") pts.push(d.gp);
      });
      if (pts.length < 2) { show(poly, false); return; }
      show(poly, true);
      attr(poly, "points", convexHull(pts).map(function (p) { return p.x.toFixed(1) + "," + p.y.toFixed(1); }).join(" "));
    });
    return moving;
  }
  var currentBasis = null;

  // ---------------------------------------------------------------- message animation

  // Off by default: a full mesh of N drones probes N*(N-1) links a second, so
  // at 10 drones about 86% of all dots are PING/PONG and they bury the leader
  // and worker structure. The user opts in, and the choice is remembered.
  var flowsOn = false;
  var dots = [];     // active: {el, from, to, start, dur, group}
  var dotPool = [];  // idle path elements
  var lastSample = new Map(); // node id -> {key, sig}

  function dotLayer() { return $(mode === "grouped" ? "topo-dots" : "g3-dots"); }

  function acquireDot() {
    var el = dotPool.pop() || s("path");
    dotLayer().appendChild(el);
    return el;
  }

  function releaseDot(d) {
    d.el.remove();
    dotPool.push(d.el);
  }

  function releaseAllDots() {
    dots.forEach(releaseDot);
    dots = [];
  }

  function releaseDotsFor(id) {
    dots = dots.filter(function (d) {
      if (d.from !== id && d.to !== id) return true;
      releaseDot(d);
      return false;
    });
  }

  // The CC resends a node's latest flows sample in every snapshot until a
  // newer one arrives, so a repeat must not animate twice. A sample is
  // identified by when the CC last heard from the node (snapshot time minus
  // last_seen_ms). A CC keepalive PONG also moves that time, so an identical
  // sample under 900ms later is treated as the same one.
  function spawnFlows(nodes) {
    if (!flowsOn || document.hidden) return;
    var t0 = now();
    var reduce = reducedMotion();
    var positions = mode === "grouped" ? drawn : drones3;
    nodes.forEach(function (n) {
      if (!n.flows.length || n.eff === "killed") return;
      var key = model.atMs !== null && n.lastSeen !== null ? model.atMs - n.lastSeen : null;
      var sig = n.flows.map(function (f) { return f.to + ":" + f.type + ":" + f.count; }).join(",");
      var prev = lastSample.get(n.id);
      lastSample.set(n.id, { key: key, sig: sig });
      if (prev && key !== null && prev.key !== null) {
        if (Math.abs(key - prev.key) < 50) return;
        if (prev.sig === sig && Math.abs(key - prev.key) < 900) return;
      }
      if (!positions.has(n.id)) return;
      n.flows.forEach(function (f) {
        var g = msgGroup(f.type);
        if (!g.on || !positions.has(f.to) || f.to === n.id) return;
        var k = Math.min(f.count, reduce ? 1 : FLOW_PER_LINK_CAP);
        for (var i = 0; i < k; i++) {
          if (dots.length >= FLOW_MAX_DOTS) return;
          var el = acquireDot();
          el.setAttribute("class", "dot mt-" + g.key);
          el.setAttribute("d", SHAPES[g.shape]);
          el.setAttribute("display", "none");
          dots.push({
            el: el, from: n.id, to: f.to, group: g,
            start: t0 + (i + Math.random() * 0.5) * (FLOW_WINDOW_MS / k),
            dur: FLOW_DOT_MS * (0.85 + Math.random() * 0.3)
          });
        }
      });
    });
    kick();
  }

  function stepDots(t) {
    if (!dots.length) return false;
    var B = mode === "grouped" ? null : (currentBasis || basisFor(mode, cams[mode].cur));
    var keep = [];
    for (var i = 0; i < dots.length; i++) {
      var d = dots[i];
      var u = (t - d.start) / d.dur;
      if (u > 1 || !d.group.on || !flowsOn) { releaseDot(d); continue; }
      keep.push(d);
      if (u < 0) { attr(d.el, "display", "none"); continue; }
      var x, y;
      if (B) {
        var a = drones3.get(d.from), b = drones3.get(d.to);
        if (!a || !b) { attr(d.el, "display", "none"); continue; }
        var p = project(B, a.wx + (b.wx - a.wx) * u, a.wy + (b.wy - a.wy) * u, a.wz + (b.wz - a.wz) * u);
        if (!p) { attr(d.el, "display", "none"); continue; }
        x = p.x; y = p.y;
      } else {
        var a2 = drawn.get(d.from), b2 = drawn.get(d.to);
        if (!a2 || !b2) { attr(d.el, "display", "none"); continue; }
        x = a2.x + (b2.x - a2.x) * u; y = a2.y + (b2.y - a2.y) * u;
      }
      attr(d.el, "display", "inline");
      d.el.setAttribute("transform", "translate(" + x.toFixed(1) + " " + y.toFixed(1) + ")");
    }
    dots = keep;
    return dots.length > 0;
  }

  function initFlowControls() {
    var ul = $("flow-legend");
    MSG_GROUPS.forEach(function (g) {
      var li = h("li");
      var lab = h("label", "chk");
      var cb = h("input");
      cb.type = "checkbox";
      cb.checked = true;
      cb.setAttribute("data-group", g.key);
      cb.addEventListener("change", function () { g.on = cb.checked; kick(); });
      var sw = s("svg", { viewBox: "-6 -6 12 12", "aria-hidden": "true", "class": "swatch" });
      sw.appendChild(s("path", { d: SHAPES[g.shape], "class": "dot mt-" + g.key }));
      lab.appendChild(cb);
      lab.appendChild(sw);
      lab.appendChild(document.createTextNode(g.label));
      li.appendChild(lab);
      ul.appendChild(li);
    });
    setFlows(loadPref("swarm-net.flows", ["on", "off"], "off") === "on", false);
    $("flows-on").addEventListener("click", function () { setFlows(!flowsOn, true); });
    // "m" toggles messages from anywhere on the page, except while typing.
    document.addEventListener("keydown", function (e) {
      if (e.altKey || e.ctrlKey || e.metaKey || (e.key !== "m" && e.key !== "M")) return;
      var t = e.target;
      var tag = t && t.tagName ? t.tagName.toLowerCase() : "";
      if (tag === "input" || tag === "textarea" || tag === "select" || (t && t.isContentEditable)) return;
      setFlows(!flowsOn, true);
      e.preventDefault();
    });
  }

  function setFlows(on, persist) {
    flowsOn = !!on;
    var btn = $("flows-on");
    btn.setAttribute("aria-pressed", String(flowsOn));
    // The label says what a click will do, so the state is never colour alone.
    btn.textContent = flowsOn ? "Hide messages" : "Show messages";
    $("flow-bar").hidden = !flowsOn;
    if (!flowsOn) releaseAllDots();
    if (persist) savePref("swarm-net.flows", flowsOn ? "on" : "off");
    kick();
  }

  // ---------------------------------------------------------------- tooltip, selection, inspector

  var selectedId = null;
  var hoverId = null;

  function select(id, focus) {
    selectedId = id && model.byId.has(id) ? id : null;
    if (selectedId && focus) focusOn(selectedId);
    var sel = $("drone-sel");
    if (selectedId && sel.value !== selectedId) {
      sel.value = selectedId;
      renderPosSliders(true);
    }
    renderTopology();
    sync3d();
    renderInspector();
    kick();
  }

  function droneLines(n) {
    var p = posOf(n);
    var lines = [
      n.id + "  (" + n.role + ", " + n.eff + ")",
      "leader: " + (n.isLeader ? "(self)" : dash(n.leader)),
      "pos: x " + p.x.toFixed(1) + "  y " + p.y.toFixed(1) + "  z " + p.z.toFixed(1) + (p.guess ? "  (not reported, hash placement)" : ""),
      "threshold " + fmtNum(n.threshold, 2) + "  hysteresis " + fmtNum(n.hysteresis, 2) +
        "  sim_version " + dash(n.simVersion) + (model.sim && model.sim.version !== null ? "/" + model.sim.version : "")
    ];
    return lines;
  }

  function peerRows(n) {
    return n.peerList.map(function (p) {
      var other = model.byId.get(p.id);
      var d = other ? dist(n, other) : null;
      return { id: p.id, role: p.role, d: d, rtt: rtt(n, p.id), model: d === null ? null : predicted(d) };
    }).sort(function (a, b) { return (a.rtt === null) - (b.rtt === null) || (a.rtt || 0) - (b.rtt || 0); });
  }

  function tipForDrone(id) {
    var n = model.byId.get(id);
    if (!n) return null;
    var lines = droneLines(n);
    if (mode === "map") {
      lines.splice(3, 0, "altitude: z " + posOf(n).z.toFixed(1) + " (the map shows ground position)");
    }
    var rows = peerRows(n);
    if (rows.length) lines.push("RTT to peers (measured / 3D distance):");
    rows.slice(0, 10).forEach(function (r) {
      lines.push("  " + shortId(r.id) + (r.role === "leader" ? " (L)" : "") + "  " + fmtMs(r.rtt) +
        "  d " + (r.d === null ? "-" : r.d.toFixed(1)));
    });
    if (rows.length > 10) lines.push("  ... " + (rows.length - 10) + " more (click to inspect)");
    return lines;
  }

  function tipForLink(wid) {
    var w = model.byId.get(wid);
    var l = w && model.byId.get(w.leader);
    if (!w || !l) return null;
    var d = dist(w, l);
    return [
      shortId(w.id) + " -> " + shortId(l.id) + " (leader)"
    ].concat(distLines(d), [
      "measured RTT: " + fmtMs(rtt(w, l.id)) + " (worker's EWMA)"
    ], modelLines(d));
  }

  // Link distances are always 3D: that is what the latency model uses.
  function distLines(d) {
    var out = ["distance: " + d.toFixed(1) + " units"];
    if (mode === "map") out.push("3D distance; the map shows ground position");
    return out;
  }

  function tipForPeer(key) {
    var ids = key.split("|");
    var a = model.byId.get(ids[0]), b = model.byId.get(ids[1]);
    if (!a || !b) return null;
    var d = dist(a, b);
    return [
      shortId(a.id) + " <-> " + shortId(b.id)
    ].concat(distLines(d), [
      "RTT " + shortId(a.id) + " -> " + shortId(b.id) + ": " + fmtMs(rtt(a, b.id)),
      "RTT " + shortId(b.id) + " -> " + shortId(a.id) + ": " + fmtMs(rtt(b, a.id))
    ], modelLines(d));
  }

  function showTip(lines, vx, vy) {
    var tip = $("tip");
    if (!lines) { hideTip(); return; }
    var text = $("tip-text");
    var sig = lines.join("\n");
    if (text.getAttribute("data-sig") !== sig) {
      text.setAttribute("data-sig", sig);
      text.textContent = "";
      lines.forEach(function (l, i) {
        var t = s("tspan", { x: 8, dy: i === 0 ? "1.2em" : "1.25em" });
        if (i === 0) t.setAttribute("class", "tip-head");
        t.textContent = l;
        text.appendChild(t);
      });
    }
    var w = 0;
    lines.forEach(function (l) { w = Math.max(w, l.length); });
    var tw = w * 6.6 + 16;
    var th = lines.length * 15 + 10;
    try {
      var bb = text.getBBox();
      if (bb && bb.width) { tw = bb.width + 16; th = bb.height + 10; }
    } catch (e) { /* not rendered yet */ }
    attr($("tip-bg"), "width", tw.toFixed(0));
    attr($("tip-bg"), "height", th.toFixed(0));
    var x = vx + 14, y = vy + 14;
    if (x + tw > VIEW_W - 4) x = Math.max(4, vx - tw - 14);
    if (y + th > VIEW_H - 4) y = Math.max(4, vy - th - 14);
    attr(tip, "transform", "translate(" + x.toFixed(0) + " " + y.toFixed(0) + ")");
    show(tip, true);
    // Keep the tooltip above everything drawn later.
    if (tip.parentNode.lastChild !== tip) tip.parentNode.appendChild(tip);
  }

  function hideTip() {
    show($("tip"), false);
    setHoverLink(null);
    if (hoverId !== null) {
      hoverId = null;
      if (!selectedId) renderInspector();
      relabel();
    }
  }

  // Hovered items win label conflicts, so a hover change re-runs the layout.
  function relabel() { sceneDirty = true; kick(); }
  function setHoverLink(key) {
    if (hoverLink === key) return;
    hoverLink = key;
    relabel();
  }

  function hoverTarget(el) {
    while (el && el !== topo) {
      if (el.getAttribute) {
        var id = el.getAttribute("data-id");
        if (id) return { kind: "drone", id: id };
        var lk = el.getAttribute("data-link");
        if (lk) return { kind: "link", id: lk };
        var pk = el.getAttribute("data-peer");
        if (pk) return { kind: "peer", id: pk };
      }
      el = el.parentNode;
    }
    return null;
  }

  function hover(target, vx, vy) {
    if (!target) { hideTip(); return; }
    var lines = target.kind === "drone" ? tipForDrone(target.id)
      : target.kind === "link" ? tipForLink(target.id) : tipForPeer(target.id);
    showTip(lines, vx, vy);
    setHoverLink(target.kind === "link" ? "w:" + target.id : target.kind === "peer" ? "p:" + target.id : null);
    if (target.kind === "drone" && hoverId !== target.id) {
      hoverId = target.id;
      if (!selectedId) renderInspector();
      relabel();
    }
  }

  var inspectorSig = "";
  function renderInspector() {
    var box = $("inspector");
    var id = selectedId || hoverId;
    var n = id ? model.byId.get(id) : null;
    if (!n) {
      if (inspectorSig === "") return;
      inspectorSig = "";
      box.textContent = "";
      box.appendChild(h("p", "empty", "Hover or click a drone to inspect it."));
      return;
    }
    var rows = peerRows(n);
    var head = droneLines(n);
    var sig = JSON.stringify([!!selectedId, head, rows]);
    if (sig === inspectorSig) return;
    inspectorSig = sig;
    box.textContent = "";
    var top = h("div", "insp-head");
    top.appendChild(h("strong", "mono", n.id));
    top.appendChild(badge(n.role, n.isLeader ? "b-leader" : "b-worker"));
    top.appendChild(badge(n.eff, "b-" + n.eff));
    if (selectedId) {
      var clear = h("button", "btn", "Clear selection");
      clear.type = "button";
      clear.addEventListener("click", function () { select(null, false); });
      top.appendChild(clear);
    } else {
      top.appendChild(h("span", "muted", "(hover; click to pin)"));
    }
    box.appendChild(top);
    var dl = h("ul", "insp-facts");
    head.slice(1).forEach(function (l) { dl.appendChild(h("li", "mono", l)); });
    box.appendChild(dl);
    if (!rows.length) {
      box.appendChild(h("p", "empty", "No peers reported."));
      return;
    }
    var wrap = h("div", "table-wrap scroll short");
    var t = h("table", "insp-table");
    var thead = h("thead");
    var hr = h("tr");
    ["peer", "role", "distance", "measured RTT", "model"].forEach(function (c) { hr.appendChild(h("th", "", c)); });
    thead.appendChild(hr);
    t.appendChild(thead);
    var tb = h("tbody");
    rows.forEach(function (r) {
      var tr = h("tr");
      tr.appendChild(h("td", "mono", r.id));
      tr.appendChild(h("td", "", r.role || "-"));
      tr.appendChild(h("td", "num", r.d === null ? "-" : r.d.toFixed(1) + " u"));
      tr.appendChild(h("td", "num", fmtMs(r.rtt)));
      tr.appendChild(h("td", "num", r.model === null ? "-" : "~" + fmtMs(r.model)));
      tb.appendChild(tr);
    });
    t.appendChild(tb);
    wrap.appendChild(t);
    box.appendChild(wrap);
  }

  // ---------------------------------------------------------------- pointer, wheel, keys

  var topo = null;
  var pointers = new Map();
  var gesture = null;

  // Client coordinates to view box coordinates (preserveAspectRatio meet).
  function toView(e) {
    var r = topo.getBoundingClientRect();
    if (!r || !r.width || !r.height) return { x: VIEW_W / 2, y: VIEW_H / 2 };
    var k = Math.min(r.width / VIEW_W, r.height / VIEW_H);
    var ox = r.left + (r.width - VIEW_W * k) / 2;
    var oy = r.top + (r.height - VIEW_H * k) / 2;
    return { x: (e.clientX - ox) / k, y: (e.clientY - oy) / k };
  }

  function pinchState() {
    var ps = Array.from(pointers.values());
    return {
      dist: Math.hypot(ps[0].x - ps[1].x, ps[0].y - ps[1].y) || 1,
      mx: (ps[0].x + ps[1].x) / 2,
      my: (ps[0].y + ps[1].y) / 2
    };
  }

  function initPointer() {
    topo = $("topo");
    topo.addEventListener("contextmenu", function (e) { e.preventDefault(); });

    topo.addEventListener("pointerdown", function (e) {
      if (e.pointerType === "mouse" && e.button > 2) return;
      try { topo.setPointerCapture(e.pointerId); } catch (err) { /* synthetic event */ }
      try { topo.focus({ preventScroll: true }); } catch (err) { /* old browser */ }
      var p = toView(e);
      pointers.set(e.pointerId, p);
      hideTip();
      if (pointers.size === 1) {
        var pan = e.button === 2 || e.button === 1 || e.shiftKey || mode !== "3d";
        gesture = { mode: pan ? "pan" : "orbit", last: p, start: p, moved: false, target: hoverTarget(e.target) };
      } else if (pointers.size === 2) {
        var ps = pinchState();
        gesture = { mode: "pinch", dist: ps.dist, mx: ps.mx, my: ps.my, moved: true };
      }
      e.preventDefault();
    });

    topo.addEventListener("pointermove", function (e) {
      var p = toView(e);
      if (!gesture || !pointers.has(e.pointerId)) {
        if (e.pointerType !== "touch") hover(hoverTarget(e.target), p.x, p.y);
        return;
      }
      pointers.set(e.pointerId, p);
      if (gesture.mode === "pinch") {
        if (pointers.size < 2) return;
        var ps = pinchState();
        zoomAt(ps.mx, ps.my, ps.dist / gesture.dist, false);
        panBy(ps.mx - gesture.mx, ps.my - gesture.my, false);
        gesture.dist = ps.dist; gesture.mx = ps.mx; gesture.my = ps.my;
        return;
      }
      var dx = p.x - gesture.last.x, dy = p.y - gesture.last.y;
      gesture.last = p;
      if (!gesture.moved && Math.hypot(p.x - gesture.start.x, p.y - gesture.start.y) < 4) return;
      gesture.moved = true;
      if (gesture.mode === "orbit") orbitBy(-dx * 0.01, dy * 0.008, false);
      else panBy(dx, dy, false);
    });

    function end(e) {
      if (!pointers.has(e.pointerId)) return;
      pointers.delete(e.pointerId);
      try { topo.releasePointerCapture(e.pointerId); } catch (err) { /* already released */ }
      if (!gesture) return;
      if (gesture.mode === "pinch") {
        // One finger left: carry on panning with it.
        if (pointers.size === 1) {
          var rest = pointers.values().next().value;
          gesture = { mode: "pan", last: rest, start: rest, moved: true, target: null };
        } else if (!pointers.size) {
          gesture = null;
        }
        return;
      }
      if (e.type === "pointerup" && !gesture.moved) {
        var t = gesture.target;
        if (t && t.kind === "drone") select(t.id, true);
        else if (t && t.kind === "link") select(t.id, false);
        else select(null, false);
      }
      if (!pointers.size) gesture = null;
    }
    topo.addEventListener("pointerup", end);
    topo.addEventListener("pointercancel", end);
    topo.addEventListener("pointerleave", function (e) {
      if (!pointers.has(e.pointerId)) hideTip();
    });

    topo.addEventListener("wheel", function (e) {
      e.preventDefault();
      var p = toView(e);
      var unit = e.deltaMode === 1 ? 0.05 : e.deltaMode === 2 ? 0.5 : 0.0015;
      zoomAt(p.x, p.y, Math.exp(-e.deltaY * unit), false);
    }, { passive: false });

    topo.addEventListener("keydown", function (e) {
      if (e.altKey || e.ctrlKey || e.metaKey) return;
      var handled = true;
      switch (e.key) {
        // 3D: arrows orbit. 2D: arrows pan (move the view that way).
        case "ArrowLeft": if (mode === "3d") orbitBy(0.15, 0, true); else panBy(40, 0, true); break;
        case "ArrowRight": if (mode === "3d") orbitBy(-0.15, 0, true); else panBy(-40, 0, true); break;
        case "ArrowUp": if (mode === "3d") orbitBy(0, 0.1, true); else panBy(0, 40, true); break;
        case "ArrowDown": if (mode === "3d") orbitBy(0, -0.1, true); else panBy(0, -40, true); break;
        case "+": case "=": zoomAt(VIEW_W / 2, VIEW_H / 2, 1.25, true); break;
        case "-": case "_": zoomAt(VIEW_W / 2, VIEW_H / 2, 1 / 1.25, true); break;
        case "0": resetView(); break;
        case "f": case "F": fitView(true); break;
        case "Escape": select(null, false); break;
        default: handled = false;
      }
      if (handled) e.preventDefault();
    });
  }

  function isFullscreen() {
    return document.fullscreenElement === $("topo-panel") || $("topo-panel").classList.contains("maximized");
  }

  function syncFullButton() {
    var on = isFullscreen();
    $("view-full").setAttribute("aria-pressed", String(on));
    setText($("view-full"), on ? "Exit fullscreen" : "Fullscreen");
  }

  function toggleFullscreen() {
    var panel = $("topo-panel");
    if (document.fullscreenElement) {
      document.exitFullscreen();
      return;
    }
    if (panel.classList.contains("maximized")) {
      panel.classList.remove("maximized");
      syncFullButton();
      return;
    }
    // Fall back to a CSS "maximized" panel where the Fullscreen API is
    // missing, refused (iOS Safari, iframes), or never answers (some
    // headless and embedded browsers leave the promise pending).
    var settled = false;
    var fallback = function () {
      if (settled) return;
      settled = true;
      panel.classList.add("maximized");
      syncFullButton();
    };
    if (panel.requestFullscreen) {
      var pr = null;
      try { pr = panel.requestFullscreen(); } catch (e) { fallback(); return; }
      if (pr && pr.then) pr.then(function () { settled = true; }, fallback);
      setTimeout(function () {
        if (!document.fullscreenElement) fallback();
      }, 700);
    } else {
      fallback();
    }
  }

  function initToolbar() {
    $("view-2d").addEventListener("click", function () { setView("2d"); });
    $("view-3d").addEventListener("click", function () { setView("3d"); });
    $("layout-map").addEventListener("click", function () { setLayout("map"); });
    $("layout-grouped").addEventListener("click", function () { setLayout("grouped"); });
    // "v" flips 2D / 3D from anywhere on the page, except while typing.
    document.addEventListener("keydown", function (e) {
      if (e.altKey || e.ctrlKey || e.metaKey || (e.key !== "v" && e.key !== "V")) return;
      var t = e.target;
      var tag = t && t.tagName ? t.tagName.toLowerCase() : "";
      if (tag === "input" || tag === "textarea" || tag === "select" || (t && t.isContentEditable)) return;
      setView(primary === "3d" ? "2d" : "3d");
      e.preventDefault();
    });
    $("zoom-in").addEventListener("click", function () { zoomAt(VIEW_W / 2, VIEW_H / 2, 1.3, true); });
    $("zoom-out").addEventListener("click", function () { zoomAt(VIEW_W / 2, VIEW_H / 2, 1 / 1.3, true); });
    $("view-fit").addEventListener("click", function () { fitView(true); });
    $("view-reset").addEventListener("click", resetView);
    $("view-full").addEventListener("click", toggleFullscreen);
    document.addEventListener("fullscreenchange", syncFullButton);
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && $("topo-panel").classList.contains("maximized")) {
        $("topo-panel").classList.remove("maximized");
        syncFullButton();
      }
    });
    $("show-peers").addEventListener("change", function () {
      showPeers = $("show-peers").checked;
      sync3d();
      kick();
    });
    $("show-labels").addEventListener("change", function () {
      showLabels = $("show-labels").checked;
      sceneDirty = true;
      kick();
    });
  }

  // ---------------------------------------------------------------- simulation panel

  var sliders = {};       // key -> {input, out, dragging, touched}
  var simPending = null;  // merged patch waiting for the throttle
  var simTimer = null;
  var simLastSent = 0;

  function makeSlider(parent, key, label, min, max, step, onInput) {
    var row = h("label", "slider");
    var name = h("span", "sl-name", label);
    var out = h("output", "sl-val mono", "-");
    var input = h("input");
    input.type = "range";
    input.min = String(min);
    input.max = String(max);
    input.step = String(step);
    input.id = "sl-" + key;
    out.htmlFor = input.id;
    row.appendChild(name);
    row.appendChild(out);
    row.appendChild(input);
    parent.appendChild(row);
    var sl = { input: input, out: out, dragging: false, touched: 0, step: step };
    var release = function () { if (sl.dragging) { sl.dragging = false; sl.touched = Date.now(); } };
    input.addEventListener("pointerdown", function () { sl.dragging = true; sl.touched = Date.now(); });
    input.addEventListener("pointerup", release);
    input.addEventListener("pointercancel", release);
    input.addEventListener("blur", release);
    input.addEventListener("input", function () {
      sl.touched = Date.now();
      onInput(roundStep(Number(input.value), step));
    });
    input.addEventListener("change", release);
    sliders[key] = sl;
    return sl;
  }

  function owned(sl) { return sl.dragging || Date.now() - sl.touched < SLIDER_HOLD_MS; }

  function setSlider(sl, value, text) {
    if (!owned(sl) && value !== null) {
      var v = String(value);
      if (sl.input.value !== v) sl.input.value = v;
    }
    setText(sl.out, text);
  }

  function simMsg(text, cls) {
    var el = $("sim-msg");
    el.textContent = text;
    el.className = "form-msg" + (cls ? " " + cls : "");
  }

  // Merge a change into the pending patch and send at most every ~150ms,
  // always sending the last value (leading and trailing edge).
  function queueSim(patch) {
    simPending = simPending || {};
    for (var k in patch) {
      if (k === "positions") {
        simPending.positions = simPending.positions || {};
        for (var id in patch.positions) simPending.positions[id] = patch.positions[id];
      } else {
        simPending[k] = patch[k];
      }
    }
    if (simTimer) return;
    var wait = SIM_THROTTLE_MS - (Date.now() - simLastSent);
    if (wait <= 0) flushSim();
    else simTimer = setTimeout(flushSim, wait);
  }

  function flushSim() {
    clearTimeout(simTimer);
    simTimer = null;
    if (!simPending) return Promise.resolve();
    var msg = { type: "sim" };
    for (var k in simPending) msg[k] = simPending[k];
    simPending = null;
    return sendSim(msg);
  }

  function sendSim(msg) {
    simLastSent = Date.now();
    var what = Object.keys(msg).filter(function (k) { return k !== "type"; }).join(", ");
    return send(msg).then(function (via) {
      simMsg("sent " + what + " (" + via + ")", "ok");
    }).catch(function (err) {
      simMsg("sim update failed: " + err.message, "err");
    });
  }

  // One-shot actions flush pending slider changes first, so ordering holds.
  function simAction(patch, label) {
    flushSim().then(function () {
      var msg = { type: "sim" };
      for (var k in patch) msg[k] = patch[k];
      sendSim(msg);
      addEvent("sim", "", "requested " + label, null, true);
    });
  }

  // "0.3 on 6/6" or "0.3 x4, 0.5 x2 of 6".
  function summarize(values, total, digits) {
    var counts = new Map();
    values.forEach(function (v) {
      if (v === null) return;
      var k = v.toFixed(digits);
      counts.set(k, (counts.get(k) || 0) + 1);
    });
    if (!counts.size) return { text: "-", mode: null };
    var list = Array.from(counts.entries()).sort(function (a, b) { return b[1] - a[1]; });
    var text = list.length === 1
      ? list[0][0] + " on " + list[0][1] + "/" + total
      : list.map(function (e) { return e[0] + " x" + e[1]; }).join(", ") + " of " + total;
    return { text: text, mode: Number(list[0][0]) };
  }

  function renderSimPanel() {
    var sm = model.sim;
    var supported = !!sm;
    $("sim-unsupported").hidden = supported;
    var inputs = document.querySelectorAll("#adv input, #adv button, #adv select");
    for (var i = 0; i < inputs.length; i++) {
      if (inputs[i].disabled === supported) inputs[i].disabled = !supported;
    }

    var live = model.nodes.filter(function (n) { return n.connected && n.eff !== "killed"; });
    var eff = {
      threshold: summarize(live.map(function (n) { return n.threshold; }), live.length, 2),
      hysteresis: summarize(live.map(function (n) { return n.hysteresis; }), live.length, 2)
    };
    setText($("eff-threshold"), eff.threshold.text);
    setText($("eff-hysteresis"), eff.hysteresis.text);

    var ap = $("sim-applied");
    if (sm && sm.version !== null) {
      var done = live.filter(function (n) { return n.simVersion !== null && n.simVersion >= sm.version; }).length;
      setText(ap, "applied on " + done + "/" + live.length + " drones");
      ap.className = "badge " + (done === live.length ? "b-alive" : "b-pending");
      setText($("sim-version"), "config version " + sm.version);
    } else {
      setText(ap, "applied on -");
      ap.className = "badge b-unknown";
      setText($("sim-version"), "");
    }
    if (!sm) { renderPosSliders(false); return; }

    var en = $("sim-enabled");
    if (sm.enabled !== null && Date.now() - enTouched > SLIDER_HOLD_MS && en.checked !== sm.enabled) en.checked = sm.enabled;

    SIM_FIELDS.forEach(function (f) {
      var sl = sliders[f.key];
      var v = sm[f.key];
      var note = "";
      // threshold 0 / hysteresis < 0: no operator override; show the nodes' own.
      if (f.key === "threshold" && (v === null || v <= 0)) { v = eff.threshold.mode; note = " (nodes' own)"; }
      if (f.key === "hysteresis" && (v === null || v < 0)) { v = eff.hysteresis.mode; note = " (nodes' own)"; }
      var shown = owned(sl) ? roundStep(Number(sl.input.value), f.step) : v;
      setSlider(sl, v, shown === null ? "-" : shown + (f.unit ? " " + f.unit : "") + (owned(sl) ? "" : note));
    });
    renderPosSliders(false);
  }
  var enTouched = 0;

  function renderDroneOptions() {
    var sel = $("drone-sel");
    var want = [""].concat(model.nodes.map(function (n) { return n.id; }));
    var have = Array.prototype.map.call(sel.options, function (o) { return o.value; });
    if (want.join("\n") === have.join("\n")) return;
    var cur = sel.value;
    while (sel.options.length > 1) sel.remove(1);
    model.nodes.forEach(function (n) {
      var o = h("option", "", n.id);
      o.value = n.id;
      sel.appendChild(o);
    });
    sel.value = want.indexOf(cur) >= 0 ? cur : "";
  }

  function renderPosSliders(force) {
    renderDroneOptions();
    var id = $("drone-sel").value;
    var n = id ? model.byId.get(id) : null;
    var size = worldSize();
    ["x", "y", "z"].forEach(function (ax) {
      var sl = sliders["pos-" + ax];
      if (sl.input.max !== String(size)) sl.input.max = String(size);
      sl.input.disabled = !n || !model.sim;
      if (!n) { setText(sl.out, "-"); return; }
      var p = posOf(n);
      if (force) { sl.touched = 0; sl.dragging = false; }
      var v = Math.round(p[ax] * 10) / 10;
      setSlider(sl, v, (owned(sl) ? Number(sl.input.value) : v).toFixed(1) + (p.guess ? " (guess)" : ""));
    });
  }

  function initSimPanel() {
    var box = $("sim-sliders");
    SIM_FIELDS.forEach(function (f) {
      makeSlider(box, f.key, f.label === f.key ? f.key : f.label + " (" + f.key + ")", f.min, f.max, f.step, function (v) {
        setText(sliders[f.key].out, v + (f.unit ? " " + f.unit : ""));
        var patch = {};
        patch[f.key] = v;
        queueSim(patch);
      });
    });
    var pbox = $("pos-sliders");
    ["x", "y", "z"].forEach(function (ax) {
      makeSlider(pbox, "pos-" + ax, ax + (ax === "z" ? " (altitude)" : ""), 0, DEFAULT_SIZE, 0.5, function (v) {
        var id = $("drone-sel").value;
        if (!id) return;
        setText(sliders["pos-" + ax].out, v.toFixed(1));
        var pos = {
          x: roundStep(Number(sliders["pos-x"].input.value), 0.5),
          y: roundStep(Number(sliders["pos-y"].input.value), 0.5),
          z: roundStep(Number(sliders["pos-z"].input.value), 0.5)
        };
        // Move the drone at once; the snapshot confirms it shortly.
        localPos.set(id, { x: pos.x, y: pos.y, z: pos.z, at: Date.now() });
        sync3d();
        kick();
        var positions = {};
        positions[id] = pos;
        queueSim({ positions: positions });
      });
    });
    $("sim-enabled").addEventListener("change", function () {
      enTouched = Date.now();
      queueSim({ enabled: $("sim-enabled").checked });
    });
    $("sim-randomize").addEventListener("click", function () {
      localPos.clear();
      simAction({ randomize: true }, "randomize positions");
    });
    $("sim-reset-pos").addEventListener("click", function () {
      localPos.clear();
      simAction({ reset_positions: true }, "reset positions");
    });
    // threshold 0 and a negative hysteresis clear the operator override; each
    // drone then returns to its own configured values.
    $("sim-clear-election").addEventListener("click", function () {
      simAction({ threshold: 0, hysteresis: -1 }, "drones' own threshold/hysteresis");
    });
    $("drone-sel").addEventListener("change", function () {
      var id = $("drone-sel").value;
      renderPosSliders(true);
      if (id) select(id, true);
    });
    renderSimPanel();
  }

  // ---------------------------------------------------------------- node table

  // Rows are keyed by node id and updated in place, so the delay input keeps
  // its value and focus across the 1 Hz refresh.
  var nodeRows = new Map();
  var NODE_COLS = ["id", "role", "state", "term", "leader", "x", "y", "z", "peers", "ledger", "dropped", "seen", "chaos"];
  var NUM_COLS = { term: 1, x: 1, y: 1, z: 1, peers: 1, ledger: 1, dropped: 1, seen: 1 };

  function makeNodeRow(id) {
    var tr = h("tr");
    var c = {};
    NODE_COLS.forEach(function (k) {
      var td = h("td");
      if (k === "id" || k === "leader") td.className = "mono";
      if (NUM_COLS[k]) td.className = "num";
      c[k] = td;
      tr.appendChild(td);
    });
    c.id.classList.add("pick");
    c.id.title = "select in the airspace view";
    c.id.addEventListener("click", function () { select(id, true); });

    var box = h("div", "chaos");
    var input = h("input");
    input.type = "number";
    input.min = "0";
    input.max = String(MAX_DELAY_MS);
    input.step = "50";
    input.value = String(DEFAULT_DELAY_MS);
    input.title = "delay in ms (0-" + MAX_DELAY_MS + ")";
    input.setAttribute("aria-label", "delay ms for " + id);

    var bDelay = h("button", "btn", "Delay");
    bDelay.type = "button";
    bDelay.addEventListener("click", function () {
      var v = Math.round(Number(input.value));
      if (!isFinite(v) || v < 0 || v > MAX_DELAY_MS) {
        input.setCustomValidity("0-" + MAX_DELAY_MS);
        input.reportValidity();
        return;
      }
      input.setCustomValidity("");
      chaos(id, "delay", v);
    });

    var bClear = h("button", "btn", "Clear");
    bClear.type = "button";
    bClear.addEventListener("click", function () { chaos(id, "clear", 0); });

    // Kill needs two clicks within 3s: cheap protection against a stray click
    // without a modal dialog interrupting a demo.
    var bKill = h("button", "btn danger", "Kill");
    bKill.type = "button";
    var armTimer = null;
    bKill.addEventListener("click", function () {
      if (!armTimer) {
        bKill.textContent = "Confirm?";
        armTimer = setTimeout(function () { armTimer = null; bKill.textContent = "Kill"; }, 3000);
        return;
      }
      clearTimeout(armTimer);
      armTimer = null;
      bKill.textContent = "Kill";
      chaos(id, "kill", 0);
    });

    box.appendChild(input);
    box.appendChild(bDelay);
    box.appendChild(bClear);
    box.appendChild(bKill);
    c.chaos.appendChild(box);
    setText(c.id, id);
    return { tr: tr, c: c, buttons: [input, bDelay, bClear, bKill] };
  }

  function renderNodeTable() {
    var tbody = $("node-table").tBodies[0];
    var seen = new Set();
    model.nodes.forEach(function (n) {
      seen.add(n.id);
      var row = nodeRows.get(n.id);
      if (!row) {
        row = makeNodeRow(n.id);
        nodeRows.set(n.id, row);
      }
      var c = row.c;
      var roleCls = n.isLeader ? "b-leader" : (n.role === "worker" ? "b-worker" : "b-unknown");
      var roles = [[n.role, roleCls]];
      if (!n.isLeader && !n.leader) roles.push(["detached", "b-detached"]);
      setBadges(c.role, roles);
      var states = [[n.eff, "b-" + n.eff]];
      if (n.degraded) states.push(["delay", "b-degraded"]);
      setBadges(c.state, states);
      setText(c.term, dash(n.term));
      setText(c.leader, n.isLeader ? "(self)" : dash(n.leader));
      setText(c.x, n.pos ? n.pos.x.toFixed(1) : "-");
      setText(c.y, n.pos ? n.pos.y.toFixed(1) : "-");
      setText(c.z, n.pos ? n.pos.z.toFixed(1) : "-");
      setText(c.peers, dash(n.peers));
      setText(c.ledger, dash(n.ledger));
      setText(c.dropped, dash(n.dropped));
      setText(c.seen, n.lastSeen === null ? "-" : fmtAgo(n.lastSeen) + " ago");
      row.tr.classList.toggle("gone", !n.connected);
      row.tr.classList.toggle("killed", n.eff === "killed");
      row.tr.classList.toggle("selected", n.id === selectedId);
      var off = n.eff === "killed";
      row.buttons.forEach(function (b) { if (b.disabled !== off) b.disabled = off; });
    });
    nodeRows.forEach(function (row, id) {
      if (!seen.has(id)) { row.tr.remove(); nodeRows.delete(id); }
    });
    reorder(tbody, model.nodes.map(function (n) { return nodeRows.get(n.id).tr; }));
  }

  // Put rows in the wanted order, touching the DOM only where the order
  // actually differs (moving a row blurs a focused input inside it).
  function reorder(tbody, rows) {
    for (var i = 0; i < rows.length; i++) {
      if (tbody.children[i] !== rows[i]) tbody.insertBefore(rows[i], tbody.children[i] || null);
    }
  }

  function chaos(node, action, delayMs) {
    var msg = { type: "chaos", node: node, action: action };
    if (action === "delay") msg.delay_ms = delayMs;
    return send(msg).then(function () {
      addEvent("chaos", node, "requested " + action + (action === "delay" ? " " + delayMs + "ms" : ""), null, true);
    }).catch(function (err) {
      addEvent("error", node, "chaos " + action + " failed: " + err.message, null, true);
    });
  }

  // ---------------------------------------------------------------- task table

  var taskRows = new Map();

  function makeTaskRow(id) {
    var tr = h("tr");
    var c = {};
    ["id", "kind", "state", "route", "dur", "out"].forEach(function (k) {
      var td = h("td");
      c[k] = td;
      tr.appendChild(td);
    });
    c.id.className = "mono";
    c.route.className = "mono";
    c.dur.className = "num";
    c.out.className = "out";
    setText(c.id, id);
    return { tr: tr, c: c, state: null };
  }

  function renderTaskTable() {
    var tbody = $("task-table").tBodies[0];
    var seen = new Set();
    var ordered = [];
    model.tasks.forEach(function (t) {
      if (seen.has(t.id)) return; // at-least-once: the server keeps the first result
      seen.add(t.id);
      var row = taskRows.get(t.id);
      if (!row) {
        row = makeTaskRow(t.id);
        taskRows.set(t.id, row);
      }
      var c = row.c;
      setText(c.kind, dash(t.kind));
      var st = [[t.state, "b-" + (t.state === "done" || t.state === "pending" || t.state === "failed" ? t.state : "unknown")]];
      if (t.state === "done" && t.ok === false) st = [["not ok", "b-failed"]];
      setBadges(c.state, st);
      setText(c.route, dash(t.leader) + " -> " + dash(t.worker));
      setText(c.dur, fmtDuration(t.duration));
      setText(c.out, dash(t.output));
      if (c.out.title !== t.output) c.out.title = t.output;
      if (row.state !== null && row.state !== t.state) flash(row.tr);
      row.state = t.state;
      ordered.push(row.tr);
    });
    taskRows.forEach(function (row, id) {
      if (!seen.has(id)) { row.tr.remove(); taskRows.delete(id); }
    });
    reorder(tbody, ordered);
    setText($("task-count-label"), ordered.length ? "(" + ordered.length + ")" : "");
  }

  // ---------------------------------------------------------------- events

  function addEvent(kind, node, detail, atMs, local) {
    var log = $("event-log");
    var li = h("li", "ev-" + kind.replace(/[^a-z_]/gi, "") + (local ? " ev-local" : ""));
    li.appendChild(h("span", "t", fmtClock(atMs)));
    li.appendChild(h("span", "k", kind.replace(/_/g, " ")));
    li.appendChild(h("span", "d", (node ? node + (detail ? ": " : "") : "") + detail));
    log.insertBefore(li, log.firstChild);
    while (log.children.length > EVENT_CAP) log.removeChild(log.lastChild);
    if (!local) {
      setText($("sum-last-event"), fmtClock(atMs) + "  " + kind.replace(/_/g, " ") + (node ? "  " + node : "") + (detail ? "  (" + detail + ")" : ""));
    }
  }

  function logLocal(text) {
    addEvent("dashboard", "", text, null, true);
  }

  // ---------------------------------------------------------------- task form

  function initTaskForm() {
    var kindSel = $("task-kind");
    var body = $("task-body");
    var count = $("task-count");
    var msgEl = $("task-msg");
    var lastDefault = TASK_DEFAULTS[kindSel.value];
    body.value = lastDefault;

    kindSel.addEventListener("change", function () {
      var def = TASK_DEFAULTS[kindSel.value] || "{}";
      // Only overwrite the body if the user has not edited it.
      if (body.value.trim() === "" || body.value === lastDefault) body.value = def;
      lastDefault = def;
      body.classList.remove("invalid");
    });

    body.addEventListener("input", function () { body.classList.remove("invalid"); });

    function say(text, cls) {
      msgEl.textContent = text;
      msgEl.className = "form-msg" + (cls ? " " + cls : "");
    }

    $("task-form").addEventListener("submit", function (ev) {
      ev.preventDefault();
      var parsed;
      try {
        parsed = body.value.trim() === "" ? {} : JSON.parse(body.value);
      } catch (e) {
        body.classList.add("invalid");
        say("body is not valid JSON: " + e.message, "err");
        return;
      }
      var n = Math.round(Number(count.value));
      if (!isFinite(n) || n < 1) n = 1;
      if (n > 100) n = 100;
      count.value = String(n);
      var msg = { type: "task", kind: kindSel.value, body: parsed, count: n };
      say("sending...", "");
      send(msg).then(function (via) {
        say("sent " + n + " " + kindSel.value + " task" + (n === 1 ? "" : "s") + " (" + via + ")", "ok");
      }).catch(function (err) {
        say("failed: " + err.message, "err");
      });
    });

    $("chaos-clear-all").addEventListener("click", function () {
      var targets = model.nodes.filter(function (n) { return n.connected; });
      if (!targets.length) return;
      targets.forEach(function (n) { chaos(n.id, "clear", 0); });
    });
  }

  // ---------------------------------------------------------------- boot

  function boot() {
    initTaskForm();
    initPointer();
    initToolbar();
    initFlowControls();
    initSimPanel();
    primary = loadPref(VIEW_KEY, ["2d", "3d"], "2d");
    layout2d = loadPref(LAYOUT_KEY, ["map", "grouped"], "map");
    applyMode();
    sceneDirty = true;
    kick();
    setInterval(tick, 1000);
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") {
        // Coming back to the tab: skip the remaining backoff and redraw.
        reconnectNow();
        sceneDirty = true;
        kick();
      } else {
        // Hidden: stop animating. Queued dots would all fire at once later.
        releaseAllDots();
        if (rafId && typeof cancelAnimationFrame === "function") cancelAnimationFrame(rafId);
        rafId = 0;
      }
    });
    window.addEventListener("online", reconnectNow);
    connect();
  }

  // Test hook: jsdom-based checks drive the page through these. Not used by
  // the page itself.
  window.__swarmDashboard = {
    handle: handle,
    model: model,
    cams: cams,
    project: function (x, y, z) { return project(camBasis(cams["3d"].cur), x, y, z); },
    projectIn: function (m, x, y, z) { return project(basisFor(m, cams[m].cur), x, y, z); },
    mode: function () { return mode; },
    setLayout: setLayout,
    frame: function (t) { frame(t); },
    select: select,
    setView: setView,
    defaultPos: defaultPos,
    predicted: predicted,
    rtt: rtt,
    queueSim: queueSim,
    flushSim: flushSim,
    dotCount: function () { return dots.length; },
    labelBoxes: function () { return labelBoxes.map(function (b) { return { x: b.x, y: b.y, w: b.w, h: b.h, kind: b.kind, id: b.id || (b.owner && b.owner.node ? b.owner.node.id : "") }; }); }
  };

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();
