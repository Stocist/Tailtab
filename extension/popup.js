// tailtab popup. Everything shown here is derived from the last status event
// the background script received: the popup keeps no idea of its own about
// whether the node is up, which account is active, or where traffic goes.

"use strict";

const api = typeof browser !== "undefined" ? browser : chrome;
// Only Firefox and Zen have proxy.onRequest, and only they gate proxying on
// private-browsing access.
const IS_GECKO =
  typeof browser !== "undefined" &&
  typeof browser.proxy !== "undefined" &&
  typeof browser.proxy.onRequest !== "undefined";
const port = api.runtime.connect({ name: "popup" });

const el = (id) => document.getElementById(id);
let latest = null;
let logoutArmed = false;
// Set when Connect is pressed and cleared once the host answers with a login
// URL or an error. Asking control for a URL can take a while, and can fail
// silently when the network is blocked, which made the button look dead.
let awaitingLogin = false;
// Set when an account switch or "Add account" was asked for, cleared when the
// host reports a state that shows it happened.
let switchingTo = "";
let menuOpen = false;
const MAX_MACHINES = 8;
// How many machines to show before anything is typed.
const PREVIEW_MACHINES = 3;

// A state the node reports that is worth explaining.
const HINTS = {
  NeedsLogin: "Log in to connect this browser profile to a tailnet.",
  NeedsMachineAuth: "Approve this device in the tailnet admin console.",
  Starting: "Connecting to the tailnet…",
  Stopped: "Disconnected. The node is logged in but not running.",
  NoState: "The node has not started yet.",
  InUseOtherUser: "Another user is signed in to this node.",
  Disconnected: "The tailtab host is not running.",
};

// What the state pill says for a state other than Running.
const LABELS = {
  NeedsLogin: "Not logged in",
  NeedsMachineAuth: "Needs approval",
  Starting: "Connecting…",
  Stopped: "Disconnected",
  NoState: "Starting…",
  Disconnected: "Host not running",
};

port.onMessage.addListener(render);
port.postMessage({ cmd: "status" });

// runningStateLine says what "Running" actually means right now.
function runningStateLine(msg, st) {
  if (msg.proxyProblem) return "Connected, not routing";
  if (st.exitNode) {
    if (!st.exitNodeActive) return "Connected, exit node offline — browsing blocked";
    const chosen = (st.exitNodes || []).find((n) => n.id === st.exitNode);
    return "Connected via " + ((chosen && chosen.name) || "exit node");
  }
  return "Connected";
}

// pillKind picks the colour: ok when routing works, bad when browsing is
// blocked, warn for anything half-way, off when nothing is running.
function pillKind(msg, st, running) {
  if (switchingTo) return "warn";
  if (!running) return st.state === "Starting" ? "warn" : "off";
  if (msg.proxyProblem) return "warn";
  if (st.exitNode && !st.exitNodeActive) return "bad";
  return "ok";
}

function setText(id, text) {
  el(id).textContent = text;
}

// renderAccount fills the header. The name is the active login profile's; a
// node that has never completed a login has no profiles yet, and says so.
function renderAccount(msg, st, running) {
  const accounts = Array.isArray(st.accounts) ? st.accounts : [];
  const active = accounts.find((a) => a.active);
  let name = "tailtab";
  let tailnet = "";
  if (switchingTo) {
    name = "Switching…";
    tailnet = switchingTo === "new" ? "adding an account" : "";
  } else if (active) {
    name = active.name || "Signed in";
    tailnet = active.tailnet || st.tailnet || "";
  } else if (running || st.tailnet) {
    name = "Signed in";
    tailnet = st.tailnet || "";
  } else if (st.state === "NeedsLogin") {
    name = "Not logged in";
  }
  setText("accountname", name);
  setText("accounttailnet", tailnet);
  // The avatar letter is the account's, and only the account's: a placeholder
  // like "Not logged in" gets the neutral mark.
  setText("avatar", active && active.name && /^[a-z0-9]/i.test(active.name) ? active.name[0].toUpperCase() : "t");
  el("account").disabled = !msg.connected;

  // The menu: every held account, then "Add account…".
  const menu = el("accountmenu");
  menu.textContent = "";
  for (const account of accounts) {
    const item = document.createElement("button");
    item.className = "item" + (account.active ? " on" : "");
    item.setAttribute && item.setAttribute("role", "menuitem");
    const who = document.createElement("span");
    const b = document.createElement("b");
    b.textContent = account.name || "(unnamed)";
    const t = document.createElement("span");
    t.className = "tn";
    t.textContent = account.tailnet || "";
    who.appendChild(b);
    who.appendChild(t);
    item.appendChild(who);
    item.dataset && (item.dataset.id = account.id);
    item.accountID = account.id;
    item.addEventListener("click", () => switchAccount(account));
    menu.appendChild(item);
  }
  if (accounts.length) {
    const div = document.createElement("div");
    div.className = "div";
    menu.appendChild(div);
  }
  const add = document.createElement("button");
  add.className = "add";
  add.textContent = "+ Add account…";
  add.addEventListener("click", addAccount);
  menu.appendChild(add);
  menu.hidden = !menuOpen;
}

function switchAccount(account) {
  closeMenu();
  if (account.active) return;
  switchingTo = account.id;
  port.postMessage({ cmd: "switch", id: account.id });
  if (latest) render(latest);
}

function addAccount() {
  closeMenu();
  switchingTo = "new";
  port.postMessage({ cmd: "addaccount" });
  if (latest) render(latest);
}

function closeMenu() {
  menuOpen = false;
  el("accountmenu").hidden = true;
}

// renderExitNodes fills the picker from the status and nothing else: the
// selection shown is always the one the host reported, never what was clicked,
// so a refused or slow change cannot leave the popup claiming something untrue.
function renderExitNodes(msg, st, running) {
  const row = el("exitrow");
  const select = el("exitnode");
  const nodes = Array.isArray(st.exitNodes) ? st.exitNodes : [];
  // Most tailnets have no exit node at all, and an empty picker is just noise.
  row.hidden = !running || nodes.length === 0;
  if (row.hidden) return;

  select.textContent = "";
  const none = document.createElement("option");
  none.value = "";
  none.textContent = "None";
  select.appendChild(none);
  for (const node of nodes) {
    const option = document.createElement("option");
    option.value = node.id;
    option.textContent = node.online ? node.name : node.name + " (offline)";
    // An offline node stays listed, because it is a real choice the user made
    // or may want back, but it cannot be newly picked while it cannot carry
    // traffic.
    option.disabled = !node.online && node.id !== st.exitNode;
    select.appendChild(option);
  }
  select.value = st.exitNode || "";
  select.disabled = !msg.connected;
}

// renderMachines lists the tailnet's machines. With nothing typed it shows the
// first few, online ones first, and says how many more there are; typing
// filters by name, DNS name or address.
function renderMachines(st, running) {
  const sec = el("machinesec");
  const peers = Array.isArray(st.peers) ? st.peers : [];
  sec.hidden = !running || peers.length === 0;
  const list = el("machines");
  list.textContent = "";
  if (sec.hidden) return;
  const q = String(el("search").value || "").trim().toLowerCase();
  let hits;
  let limit = MAX_MACHINES;
  if (q) {
    hits = peers.filter(
      (p) =>
        (p.name || "").toLowerCase().includes(q) ||
        (p.dnsName || "").toLowerCase().includes(q) ||
        (p.ip || "").includes(q)
    );
  } else {
    hits = peers.slice().sort((a, b) => (b.online ? 1 : 0) - (a.online ? 1 : 0));
    limit = PREVIEW_MACHINES;
  }
  for (const peer of hits.slice(0, limit)) {
    const li = document.createElement("li");
    const name = document.createElement("button");
    name.className = "name" + (peer.online ? "" : " off");
    name.textContent = peer.name || peer.dnsName || peer.ip;
    name.title = peer.online ? "Open http://" + (peer.dnsName || peer.name) + "/" : "Offline";
    name.addEventListener("click", () => openPeer(peer));
    const ip = document.createElement("button");
    ip.className = "v copy";
    ip.textContent = peer.ip || "";
    ip.title = "Copy";
    ip.addEventListener("click", () => copy(peer.ip));
    li.appendChild(name);
    li.appendChild(ip);
    list.appendChild(li);
  }
  if (hits.length > limit) {
    const more = document.createElement("li");
    more.className = "more";
    more.textContent = hits.length - limit + " more · type to filter";
    list.appendChild(more);
  }
  if (hits.length === 0) {
    const none = document.createElement("li");
    none.className = "more";
    none.textContent = "No machine matches";
    list.appendChild(none);
  }
}

function openPeer(peer) {
  const host = peer.dnsName || peer.name;
  if (!host) return;
  api.tabs.create({ url: "http://" + host + "/" });
  window.close();
}

function copy(text) {
  if (!text) return;
  const done = () => {
    const toast = el("copied");
    if (!toast) return;
    toast.hidden = false;
    setTimeout(() => {
      toast.hidden = true;
    }, 1200);
  };
  if (typeof navigator !== "undefined" && navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, done);
  } else {
    done();
  }
}

function render(msg) {
  latest = msg;
  const st = msg.status || {};
  const state = st.state || "Unknown";
  const running = state === "Running";

  const warnings = Array.isArray(st.warnings) ? st.warnings : [];
  if (st.authURL || st.error || running || state === "Starting") {
    awaitingLogin = false;
  }
  // A switch is over once the host reports either the target account as
  // active, or a state the new profile would show (NeedsLogin for a fresh
  // one, Starting/Running for an existing one).
  if (switchingTo) {
    const accounts = Array.isArray(st.accounts) ? st.accounts : [];
    const active = accounts.find((a) => a.active);
    if (
      (switchingTo === "new" && (state === "NeedsLogin" || st.authURL)) ||
      (active && active.id === switchingTo) ||
      st.error
    ) {
      switchingTo = "";
    }
  }

  // A login URL supersedes the last login error: the error explains a failure
  // the URL has already moved past, and showing it beside a working Log in
  // button reads as though the button will not work. It stays in the warnings
  // list below, so a node that genuinely cannot reach control still says so.
  const errorLine = st.authURL ? "" : st.error;

  renderAccount(msg, st, running);

  // "Connected" means the node is up AND the browser is pointed at it. If the
  // proxy configuration did not take, saying Connected alone would be a lie:
  // tailnet names are going out over the public internet.
  const pill = el("state");
  pill.textContent = switchingTo
    ? "Switching account…"
    : running
      ? runningStateLine(msg, st)
      : LABELS[state] || state;
  pill.className = "pill " + pillKind(msg, st, running);
  setText(
    "hint",
    switchingTo
      ? "Keeping the proxy off until the other account is up."
      : awaitingLogin
        ? "Requesting login link…"
        : errorLine || HINTS[state] || ""
  );

  // The reason a login is failing arrives as a health warning, so it is shown
  // whether or not it also became the hint above.
  const list = el("warnings");
  list.textContent = "";
  const extra = warnings.filter((w) => w !== errorLine).slice(0, 4);
  for (const text of extra) {
    const li = document.createElement("li");
    li.textContent = text;
    list.appendChild(li);
  }
  list.hidden = extra.length === 0;

  const toggle = el("toggle");
  toggle.className = running ? "on" : "";
  toggle.setAttribute && toggle.setAttribute("aria-checked", running ? "true" : "false");
  toggle.disabled = !msg.connected || !!switchingTo || (!running && state !== "Stopped" && !st.authURL && state !== "NeedsLogin");

  el("details").hidden = !running;
  if (running) {
    setText("tailnet", st.tailnet || "unknown");
    setText("hostname", st.hostname || "unknown");
    setText("selfip", st.selfIP || "unknown");
  }
  setText("port", st.proxyPort ? "local proxy 127.0.0.1:" + st.proxyPort : "");

  // A login URL is always offered when there is one, warnings or not.
  el("login").hidden = !st.authURL;
  el("connect").hidden = running || !!st.authURL || !msg.connected;
  el("connect").disabled = awaitingLogin;
  el("connect").textContent = awaitingLogin ? "Requesting…" : "Connect";
  // The header toggle is the disconnect control; the button stays for
  // keyboard users but out of the way.
  el("disconnect").hidden = true;
  el("logout").hidden = !msg.connected || (!running && state !== "Stopped");

  renderExitNodes(msg, st, running);
  renderMachines(st, running);

  const warning = el("warning");
  if (msg.proxyProblem) {
    warning.hidden = false;
    warning.className = "";
    warning.textContent = msg.proxyProblem;
  } else if (!msg.connected) {
    warning.hidden = false;
    warning.className = "";
    warning.textContent = "Not connected to the tailtab host. Reconnecting…";
  } else {
    warning.hidden = true;
    warning.textContent = "";
  }

  if (!logoutArmed) el("logout").textContent = "Log out";
}

el("account").addEventListener("click", (e) => {
  if (e && e.stopPropagation) e.stopPropagation();
  menuOpen = !menuOpen;
  el("accountmenu").hidden = !menuOpen;
});
if (document.addEventListener) {
  document.addEventListener("click", (e) => {
    if (!menuOpen) return;
    const menu = el("accountmenu");
    if (menu.contains && e && menu.contains(e.target)) return;
    closeMenu();
  });
}

el("toggle").addEventListener("click", () => {
  const st = (latest && latest.status) || {};
  if (st.state === "Running") {
    port.postMessage({ cmd: "down" });
    return;
  }
  if (st.authURL) {
    api.tabs.create({ url: st.authURL });
    window.close();
    return;
  }
  connect();
});

function connect() {
  const st = (latest && latest.status) || {};
  // Only NeedsLogin waits on control for a URL; from Stopped, up is immediate.
  awaitingLogin = st.state === "NeedsLogin" && !st.authURL;
  if (awaitingLogin) {
    setText("hint", "Requesting login link…");
    el("connect").disabled = true;
    el("connect").textContent = "Requesting…";
  }
  port.postMessage({ cmd: "up" });
}

el("login").addEventListener("click", () => {
  const url = latest && latest.status && latest.status.authURL;
  if (!url) return;
  api.tabs.create({ url: url });
  window.close();
});

el("connect").addEventListener("click", connect);
el("disconnect").addEventListener("click", () => port.postMessage({ cmd: "down" }));
el("selfip").addEventListener("click", () => copy(latest && latest.status && latest.status.selfIP));
el("search").addEventListener("input", () => {
  if (latest) renderMachines(latest.status || {}, (latest.status || {}).state === "Running");
});

// Choosing an exit node routes this whole browser profile through it. Nothing
// is rendered from this event: the picker moves only when the host reports the
// new selection back.
el("exitnode").addEventListener("change", (e) => {
  port.postMessage({ cmd: "exitnode", id: e.target.value || "" });
});

// Two-step confirmation: a dialog from a popup is unreliable across browsers,
// and logging out discards the node's credentials.
el("logout").addEventListener("click", () => {
  if (!logoutArmed) {
    logoutArmed = true;
    el("logout").textContent = "Confirm log out";
    setTimeout(() => {
      logoutArmed = false;
      el("logout").textContent = "Log out";
    }, 4000);
    return;
  }
  logoutArmed = false;
  port.postMessage({ cmd: "logout" });
});

// Firefox only routes private-window traffic through proxy.onRequest if the
// add-on is allowed there, and that is off by default for a temporary add-on.
if (IS_GECKO && api.extension && api.extension.isAllowedIncognitoAccess) {
  Promise.resolve(api.extension.isAllowedIncognitoAccess())
    .then((allowed) => {
      if (allowed) return;
      const warning = el("warning");
      if (warning.hidden) {
        warning.hidden = false;
        warning.textContent =
          "Private windows are not covered: turn on “Run in Private Windows” for tailtab.";
      }
    })
    .catch(() => {});
}
