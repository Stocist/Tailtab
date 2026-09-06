"use strict";

const api = typeof browser !== "undefined" ? browser : chrome;
// Gecko alone exposes proxy.onRequest and gates it on private-window access.
const IS_GECKO =
  typeof browser !== "undefined" &&
  typeof browser.proxy !== "undefined" &&
  typeof browser.proxy.onRequest !== "undefined";
const port = api.runtime.connect({ name: "popup" });

const el = (id) => document.getElementById(id);
let latest = null;
let logoutArmed = false;
// Keep login feedback visible while the control request is in flight.
let awaitingLogin = false;
let switchingTo = "";
let menuOpen = false;
const BUILD = "__TAILTAB_BUILD__";
const SWITCH_TIMEOUT_MS = 20000;
let switchTimer = null;
let loginTimer = null;
// Status re-renders must not open the same login URL twice.
let openedLogin = "";
const MAX_MACHINES = 8;
const PREVIEW_MACHINES = 3;
let showAllMachines = false;

const HINTS = {
  NeedsLogin: "Log in to connect this browser profile to a tailnet.",
  NeedsMachineAuth: "Approve this device in the tailnet admin console.",
  Starting: "Connecting to the tailnet…",
  Stopped: "Disconnected. The node is logged in but not running.",
  NoState: "The node has not started yet.",
  InUseOtherUser: "Another user is signed in to this node.",
  Disconnected: "The tailtab host is not running.",
};

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

function runningStateLine(msg, st) {
  if (msg.proxyProblem) return "Connected, not routing";
  if (st.exitNode) {
    if (!st.exitNodeActive) return "Connected, exit node offline — browsing blocked";
    const chosen = (st.exitNodes || []).find((n) => n.id === st.exitNode);
    return "Connected via " + ((chosen && chosen.name) || "exit node");
  }
  return "Connected";
}

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

function renderAccount(msg, st, running) {
  const accounts = Array.isArray(st.accounts) ? st.accounts : [];
  const active = accounts.find((a) => a.active);
  let name = "tailtab";
  let tailnet = "";
  if (switchingTo) {
    name = "Switching…";
    tailnet = switchingTo === "new" ? "adding an account" : "";
  } else if (active) {
    name = accountLabel(active);
    tailnet = active.tailnet || st.tailnet || "";
  } else if (st.state === "NeedsLogin" || st.authURL) {
    name = "Not logged in";
  } else if (running || st.tailnet) {
    // A fresh or stale worker may lack account data; show only known status.
    name = st.hostname ? st.hostname : "This profile";
    tailnet = st.tailnet || "";
  }
  setText("accountname", name);
  setText("accounttailnet", tailnet);
  renderAvatar(active);
  el("account").disabled = !msg.connected;

  const menu = el("accountmenu");
  menu.textContent = "";
  for (const account of accounts) {
    const item = document.createElement("button");
    item.className = "item" + (account.active ? " on" : "");
    item.setAttribute && item.setAttribute("role", "menuitem");
    const who = document.createElement("span");
    const b = document.createElement("b");
    b.textContent = accountLabel(account);
    const t = document.createElement("span");
    t.className = "tn";
    t.textContent = account.tailnet || account.name || "";
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

function accountLabel(account) {
  return account.displayName || account.name || "Signed in";
}

// The picture URL comes from the identity provider: https, no credentials or port, somewhere public.
function avatarURL(picture) {
  let u;
  try { u = new URL(picture || ""); } catch (e) { return ""; }
  if (u.protocol !== "https:" || u.username || u.password || u.port) return "";
  return tailtabExitModeProxies(u.hostname, []) ? u.href : "";
}

function renderAvatar(active) {
  const avatar = el("avatar");
  const label = active ? accountLabel(active) : "";
  const letter = label && /^[a-z0-9]/i.test(label) ? label[0].toUpperCase() : "t";
  const picture = active ? avatarURL(active.picture) : "";
  if (picture) {
    avatar.textContent = "";
    const img = document.createElement("img");
    img.src = picture;
    img.alt = label;
    img.addEventListener("error", () => {
      avatar.textContent = letter;
    });
    avatar.appendChild(img);
  } else {
    avatar.textContent = letter;
  }
}

// The host reports NeedsLogin first and the login URL in a later status.
function awaitLogin() {
  awaitingLogin = true;
  if (loginTimer) clearTimeout(loginTimer);
  loginTimer = setTimeout(() => {
    loginTimer = null;
    if (!awaitingLogin) return;
    awaitingLogin = false;
    if (latest) {
      render(latest);
      setText("hint", "The login link did not arrive. Try again.");
    }
  }, SWITCH_TIMEOUT_MS);
}

function beginSwitch(target) {
  switchingTo = target;
  if (switchTimer) clearTimeout(switchTimer);
  // Abandon an unconfirmed switch rather than displaying speculative state.
  switchTimer = setTimeout(() => {
    switchTimer = null;
    if (!switchingTo) return;
    switchingTo = "";
    if (latest) {
      render(latest);
      setText("hint", "The tailtab host did not confirm the change. Reload the extension and try again.");
    }
  }, SWITCH_TIMEOUT_MS);
}

function switchAccount(account) {
  closeMenu();
  if (account.active) return;
  beginSwitch(account.id);
  port.postMessage({ cmd: "switch", id: account.id });
  if (latest) render(latest);
}

function addAccount() {
  closeMenu();
  beginSwitch("new");
  port.postMessage({ cmd: "addaccount" });
  if (latest) render(latest);
}

function closeMenu() {
  menuOpen = false;
  el("accountmenu").hidden = true;
}

// Render only host-confirmed selection so slow or rejected changes stay truthful.
function renderExitNodes(msg, st, running) {
  const row = el("exitrow");
  const select = el("exitnode");
  const nodes = Array.isArray(st.exitNodes) ? st.exitNodes : [];
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
    // Keep the selected offline node visible, but prevent selecting a new one.
    option.disabled = !node.online && node.id !== st.exitNode;
    select.appendChild(option);
  }
  select.value = st.exitNode || "";
  select.disabled = !msg.connected;
}

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
  if (showAllMachines) limit = Infinity;
  list.className = showAllMachines ? "all" : "";
  for (const peer of hits.slice(0, limit)) {
    const li = document.createElement("li");
    const name = document.createElement("button");
    name.className = "name" + (peer.online ? "" : " off");
    name.textContent = peer.name || peer.dnsName || peer.ip;
    const url = peer.online ? peerURL(peer, st) : "";
    name.title = peer.online ? (url ? "Open " + url : "") : "Offline";
    name.addEventListener("click", () => openPeer(url));
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
    const count = document.createElement("span");
    count.textContent = hits.length - limit + " more · type to filter";
    const all = document.createElement("button");
    all.textContent = "View all";
    all.addEventListener("click", () => {
      showAllMachines = true;
      renderMachines(st, running);
    });
    more.appendChild(count);
    more.appendChild(all);
    list.appendChild(more);
  } else if (showAllMachines && hits.length > PREVIEW_MACHINES && !q) {
    const less = document.createElement("li");
    less.className = "more";
    const count = document.createElement("span");
    count.textContent = hits.length + " machines";
    const fewer = document.createElement("button");
    fewer.textContent = "Show fewer";
    fewer.addEventListener("click", () => {
      showAllMachines = false;
      renderMachines(st, running);
    });
    less.appendChild(count);
    less.appendChild(fewer);
    list.appendChild(less);
  }
  if (hits.length === 0) {
    const none = document.createElement("li");
    none.className = "more";
    none.textContent = "No machine matches";
    list.appendChild(none);
  }
}

// The name comes from the control plane: open it only as a bare tailnet host.
function peerURL(peer, st) {
  const host = String(peer.dnsName || peer.name || peer.ip || "").toLowerCase();
  if (!host) return "";
  let u;
  try { u = new URL("http://" + host + "/"); } catch (e) { return ""; }
  if (u.hostname !== host || u.username || u.password || u.port || u.pathname !== "/" || u.search || u.hash) return "";
  return tailtabIsTailnetHost(host, st.tailnet, []) ? u.href : "";
}

function openPeer(url) {
  if (!url) return;
  api.tabs.create({ url: url });
  window.close();
}

// Control chooses the login URL; only an https page is opened.
function openLogin(url) {
  let u;
  try { u = new URL(url); } catch (e) { return; }
  if (u.protocol !== "https:") return;
  api.tabs.create({ url: u.href });
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

  let warnings = Array.isArray(st.warnings) ? st.warnings : [];
  // A login URL supersedes the prior attempt's logged-out warning.
  if (st.authURL) warnings = warnings.filter((w) => !/^You are logged out/.test(w));

  // Open a requested login URL when its asynchronous status arrives.
  if (st.authURL && (awaitingLogin || switchingTo === "new") && !openedLogin) {
    openedLogin = st.authURL;
    awaitingLogin = false;
    openLogin(st.authURL);
  }
  // The wait ends with the URL, the node moving on, or a real error. The stale
  // logged-out warning lands in between and is not one.
  if (st.authURL || state !== "NeedsLogin" || (st.error && !/^You are logged out/.test(st.error))) {
    awaitingLogin = false;
  }
  // End switching only on host-confirmed account, login state, or error.
  if (switchingTo) {
    const accounts = Array.isArray(st.accounts) ? st.accounts : [];
    const active = accounts.find((a) => a.active);
    if (
      (switchingTo === "new" && (state === "NeedsLogin" || st.authURL)) ||
      (active && active.id === switchingTo) ||
      st.error
    ) {
      if (switchingTo === "new" && state === "NeedsLogin" && !st.authURL) awaitLogin();
      switchingTo = "";
      if (switchTimer) {
        clearTimeout(switchTimer);
        switchTimer = null;
      }
    }
  }

  // Do not pair a working login URL with its superseded error.
  const errorLine = st.authURL ? "" : st.error;

  renderAccount(msg, st, running);

  // Never report routing when proxy setup failed and names could leak to DNS.
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

  // Hide stale account details while a switch is in flight.
  el("details").hidden = !running || !!switchingTo;
  if (running) {
    setText("tailnet", st.tailnet || "unknown");
    setText("hostname", st.hostname || "unknown");
    setText("selfip", st.selfIP || "unknown");
    const routes = Array.isArray(st.subnetRoutes) ? st.subnetRoutes : [];
    el("routesrow").hidden = routes.length === 0;
    setText("routes", routes.join(", "));
    el("routes").title = routes.join("\n");
    const control = String(st.controlURL || "").replace(/\/+$/, "");
    const custom = control && control !== "https://controlplane.tailscale.com";
    el("controlrow").hidden = !custom;
    setText("control", custom ? control.replace(/^https?:\/\//, "") : "");
  }
  setText("port", st.proxyPort ? "local proxy 127.0.0.1:" + st.proxyPort : "");

  el("login").hidden = !st.authURL;
  el("connect").hidden = running || !!st.authURL || !msg.connected;
  el("connect").disabled = awaitingLogin;
  el("connect").textContent = awaitingLogin ? "Requesting…" : "Connect";
  el("disconnect").hidden = true;
  el("logout").hidden = !msg.connected || (!running && state !== "Stopped");

  renderExitNodes(msg, st, running);
  renderMachines(st, running);

  const warning = el("warning");
  if (msg.build !== BUILD) {
    // Chromium can retain an older worker across browser restarts.
    warning.hidden = false;
    warning.className = "bad";
    warning.textContent = "tailtab was updated. Reload the extension (edge://extensions or about:debugging) to finish.";
  } else if (msg.proxyProblem) {
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
    openLogin(st.authURL);
    return;
  }
  connect();
});

function connect() {
  const st = (latest && latest.status) || {};
  // Only NeedsLogin waits asynchronously for a control-server URL.
  if (st.state === "NeedsLogin" && !st.authURL) {
    awaitLogin();
    setText("hint", "Requesting login link…");
    el("connect").disabled = true;
    el("connect").textContent = "Requesting…";
  }
  port.postMessage({ cmd: "up" });
}

el("login").addEventListener("click", () => {
  const url = latest && latest.status && latest.status.authURL;
  if (url) openLogin(url);
});

el("connect").addEventListener("click", connect);
el("settings").addEventListener("click", () => {
  if (api.runtime.openOptionsPage) api.runtime.openOptionsPage();
});
el("disconnect").addEventListener("click", () => port.postMessage({ cmd: "down" }));
el("selfip").addEventListener("click", () => copy(latest && latest.status && latest.status.selfIP));
el("search").addEventListener("input", () => {
  if (latest) renderMachines(latest.status || {}, (latest.status || {}).state === "Running");
});

// Wait for host status before rendering a new exit-node selection.
el("exitnode").addEventListener("change", (e) => {
  port.postMessage({ cmd: "exitnode", id: e.target.value || "" });
});

// Popup dialogs are unreliable across browsers; use inline confirmation before
// discarding node credentials.
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

// Gecko omits private-window proxy.onRequest events unless access is enabled.
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
