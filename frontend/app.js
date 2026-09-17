// swarm-net dashboard.
//
// Plain browser JavaScript: no framework, no bundler, no CDN. The file is
// served as-is by the frontend's nginx, which proxies api/ and ws on the same
// origin to the control center, so every URL here is relative.
//
// Data flow (contract: docs/architecture/control-plane.md):
//   server -> browser  {"type":"snapshot", nodes:[...], tasks:[...]}  every 1s
//   server -> browser  {"type":"event", kind, node, detail}           as they happen
//   browser -> server  {"type":"task", kind, body, count}
//   browser -> server  {"type":"chaos", node, action, delay_ms}
//
// Every field read from the server goes through a defensive accessor: a
// missing or null field must degrade to "-", never throw and freeze the page.
// All server text is inserted with textContent, never innerHTML.

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

  var TASK_DEFAULTS = {
    echo: '{\n  "msg": "hello swarm"\n}',
    sleep: '{\n  "ms": 200\n}',
    hash: '{\n  "data": "abc"\n}'
  };

  var KNOWN_STATES = { alive: 1, suspect: 1, dead: 1 };
  var FLASH_EVENTS = { leader_change: 1, node_down: 1, node_up: 1, state_change: 1, chaos: 1 };

  // ---------------------------------------------------------------- helpers

  function $(id) { return document.getElementById(id); }

  function num(v) { return typeof v === "number" && isFinite(v) ? v : null; }
  function str(v) {
    if (v === null || v === undefined) return "";
    return typeof v === "string" ? v : String(v);
  }
  function arr(v) { return Array.isArray(v) ? v : []; }

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

  // ---------------------------------------------------------------- state

  var model = {
    nodes: [],          // normalised, sorted by id
    byId: new Map(),
    tasks: [],          // normalised, newest first
    recvAt: 0,          // local time the last snapshot arrived
    prevRole: new Map() // id -> role, to flash role changes between snapshots
  };

  function normNode(n) {
    if (!n || typeof n !== "object") return null;
    var id = str(n.id);
    if (!id) return null;
    // A missing "connected" is treated as connected: older servers may omit it,
    // and hiding every node would be worse than showing one stale one.
    var connected = n.connected !== false;
    var st = str(n.state).toLowerCase() || "unknown";
    var eff = !connected ? "disconnected" : (KNOWN_STATES[st] ? st : "unknown");
    var role = str(n.role).toLowerCase() || "unknown";
    var leader = str(n.leader);
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
      peers: Array.isArray(n.peers) ? n.peers.length : null
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

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(msg));
      return Promise.resolve("ws");
    }
    // Fall back to the HTTP API so controls still work while the socket is
    // reconnecting.
    var path = msg.type === "task" ? "api/tasks" : "api/chaos";
    return fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(msg)
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
    model.tasks = tasks;
    model.recvAt = Date.now();

    renderSummary();
    renderTopology();
    renderNodeTable();
    renderTaskTable();
    changed.forEach(flashNode);
  }

  function onEvent(msg) {
    var kind = str(msg.kind) || "event";
    var node = str(msg.node);
    var detail = str(msg.detail);
    addEvent(kind, node, detail, num(msg.at_unix_ms), false);
    if (node && FLASH_EVENTS[kind]) flashNode(node);
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

  // ---------------------------------------------------------------- topology

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
  var animating = false;

  function nodeRadius(n, total) {
    if (n.isLeader) return total > 30 ? 16 : 22;
    return total > 30 ? 8 : total > 15 ? 11 : 13;
  }

  function makeNode(n) {
    var g = s("g", { "class": "topo-node" });
    var title = s("title");
    var flashRing = s("circle", { "class": "flash-ring", r: 30 });
    var halo = s("circle", { "class": "halo", r: 30 });
    var shape = s("circle", { "class": "n-shape", r: 12 });
    var glyph = s("text", { "class": "n-glyph" });
    var cross = s("g", { "class": "n-cross-g" });
    var c1 = s("line", { "class": "n-cross" });
    var c2 = s("line", { "class": "n-cross" });
    cross.appendChild(c1);
    cross.appendChild(c2);
    var label = s("text", { "class": "n-label" });
    var sub = s("text", { "class": "n-label n-sublabel" });
    [title, flashRing, halo, shape, glyph, cross, label, sub].forEach(function (e) { g.appendChild(e); });
    $("topo-nodes").appendChild(g);
    return {
      g: g, title: title, flashRing: flashRing, halo: halo, shape: shape, glyph: glyph,
      cross: cross, c1: c1, c2: c2, label: label, sub: sub,
      x: null, y: null, tx: 0, ty: 0, sig: ""
    };
  }

  function styleNode(d, n, total) {
    var r = nodeRadius(n, total);
    var sig = [n.eff, n.isLeader, n.degraded, n.term, n.role, r, n.leader].join("|");
    if (sig === d.sig) return;
    d.sig = sig;

    d.shape.setAttribute("r", r);
    d.shape.setAttribute("class", "n-shape st-" + n.eff + (n.isLeader ? " leader-shape" : ""));
    d.flashRing.setAttribute("r", r + 6);
    d.halo.setAttribute("r", r + 6);
    d.halo.style.display = n.degraded ? "" : "none";

    // Leaders carry an "L" glyph; colour is never the only cue.
    setText(d.glyph, n.isLeader && n.eff !== "disconnected" ? "L" : "");
    d.glyph.style.fontSize = Math.round(r * 0.9) + "px";

    var dead = n.eff === "dead";
    d.cross.style.display = dead ? "" : "none";
    var k = r * 0.6;
    d.c1.setAttribute("x1", -k); d.c1.setAttribute("y1", -k);
    d.c1.setAttribute("x2", k);  d.c1.setAttribute("y2", k);
    d.c2.setAttribute("x1", -k); d.c2.setAttribute("y1", k);
    d.c2.setAttribute("x2", k);  d.c2.setAttribute("y2", -k);

    d.label.setAttribute("y", r + 15);
    setText(d.label, shortId(n.id));
    d.sub.setAttribute("y", r + 28);
    var parts = [];
    if (n.eff !== "alive") parts.push(n.eff.toUpperCase());
    if (n.degraded) parts.push("DELAY");
    if (n.isLeader && n.term !== null) parts.push("t" + n.term);
    setText(d.sub, parts.join(" "));
    d.sub.setAttribute("class", "n-label n-sublabel" + (n.degraded ? " deg" : ""));

    setText(d.title, n.id + "\nrole: " + n.role + "\nstate: " + n.eff +
      (n.degraded ? " (delay injected)" : "") +
      "\nterm: " + dash(n.term) + "\nleader: " + dash(n.leader));
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
        d = makeNode(n);
        drawn.set(n.id, d);
        d.x = p.x;
        d.y = p.y;
      }
      d.tx = p.x;
      d.ty = p.y;
      styleNode(d, n, total);
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
      line.setAttribute("class", "link" + (bad ? " bad" : ""));
      line.setAttribute("data-leader", n.leader);
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
    });
    hulls.forEach(function (c, lid) {
      if (!groupsNow.has(lid)) { c.remove(); hulls.delete(lid); }
    });

    startAnimation();
  }

  function startAnimation() {
    if (animating) return;
    animating = true;
    requestAnimationFrame(step);
  }

  // Ease every node toward its target; lines and hulls follow the eased
  // positions so nothing jumps.
  function step() {
    var moving = false;
    var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
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
      d.g.setAttribute("transform", "translate(" + d.x.toFixed(1) + " " + d.y.toFixed(1) + ")");
    });
    links.forEach(function (line, wid) {
      var w = drawn.get(wid);
      var l = drawn.get(line.getAttribute("data-leader"));
      if (!w || !l) return;
      line.setAttribute("x1", w.x.toFixed(1));
      line.setAttribute("y1", w.y.toFixed(1));
      line.setAttribute("x2", l.x.toFixed(1));
      line.setAttribute("y2", l.y.toFixed(1));
    });
    hulls.forEach(function (c, lid) {
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
      c.setAttribute("cx", cx.toFixed(1));
      c.setAttribute("cy", cy.toFixed(1));
      c.setAttribute("r", (r + 34).toFixed(1));
    });
    if (moving) requestAnimationFrame(step);
    else animating = false;
  }

  function flashNode(id) {
    var d = drawn.get(id);
    if (d) flash(d.g);
    var row = nodeRows.get(id);
    if (row) flash(row.tr);
  }

  // ---------------------------------------------------------------- node table

  // Rows are keyed by node id and updated in place, so the delay input keeps
  // its value and focus across the 1 Hz refresh.
  var nodeRows = new Map();

  function makeNodeRow(id) {
    var tr = h("tr");
    var c = {};
    ["id", "role", "state", "term", "leader", "peers", "ledger", "dropped", "seen", "chaos"].forEach(function (k) {
      var td = h("td");
      if (k === "id" || k === "leader") td.className = "mono";
      if (k === "term" || k === "peers" || k === "ledger" || k === "dropped" || k === "seen") td.className = "num";
      c[k] = td;
      tr.appendChild(td);
    });

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
    return { tr: tr, c: c };
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
      setText(c.peers, dash(n.peers));
      setText(c.ledger, dash(n.ledger));
      setText(c.dropped, dash(n.dropped));
      setText(c.seen, n.lastSeen === null ? "-" : fmtAgo(n.lastSeen) + " ago");
      row.tr.classList.toggle("gone", !n.connected);
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
    setInterval(tick, 1000);
    // Coming back to the tab or the network: skip the remaining backoff.
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") reconnectNow();
    });
    window.addEventListener("online", reconnectNow);
    connect();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();
