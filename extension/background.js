// tailtab background script. One file for both browsers: the proxy layer is
// chosen by feature detection, everything else is shared.
//
// Chromium loads this as an MV3 service worker; Firefox and Zen load it as an
// MV3 event page, with rules.js listed first in background.scripts. Every
// listener here is registered at the top level, synchronously: an event page
// does not wake for a listener registered inside a callback.

"use strict";

// rules.js supplies tailtabIsTailnetHost and tailtabBuildPac. A service worker
// has to pull it in; an event page has already loaded it.
if (typeof importScripts === "function" && typeof tailtabIsTailnetHost === "undefined") {
  importScripts("rules.js");
}

const api = typeof browser !== "undefined" ? browser : chrome;

// Firefox and Zen filter per request; Chromium installs a PAC script.
const USE_ON_REQUEST = typeof api.proxy !== "undefined" && typeof api.proxy.onRequest !== "undefined";
const BROWSER = USE_ON_REQUEST ? "zen" : "edge";
const HOST_NAME = "com.stocist.tailtab";
// Stamped by scripts/build.sh. The popup compares it with its own copy: a
// mismatch means Chromium is running a background worker from an older build
// than the popup files, which a browser restart does not fix but an extension
// reload does.
const BUILD = "__TAILTAB_BUILD__";
// The username half of the proxy credential. The password is the per-process
// token the host sends with every status event.
const PROXY_USER = "tailtab";
// The realm the host puts in its 407 challenge, which is how a challenge from
// a tailtab host that is not ours (an earlier life's) is told apart from
// another local proxy.
const PROXY_REALM = "tailtab";
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 30000;

// The last status event from the host, and the only thing the UI renders.
let status = { state: "Disconnected", error: "", proxyPort: 0, tailnet: "", warnings: [], exitNode: "", exitNodes: [], exitNodeActive: false, accounts: [], peers: [], subnetRoutes: [] };

// exitMode reports whether the browser should send everything through the
// proxy. It keys off the exit node being SELECTED, not on it being active: if
// the node goes offline, the host refuses public destinations and browsing
// stops, which is the honest failure. Falling back to the split tunnel would
// quietly put this profile's traffic back on the local network instead, which
// is exactly what choosing an exit node was meant to prevent (G15).
function exitMode() {
  return !!status.exitNode;
}
// Why the proxy could not be configured, if it could not be.
let proxyProblem = "";
// The proxy credential from the host. This is a secret, and it is deliberately
// kept nowhere but this variable: not storage.local, not storage.session, not
// the popup, not a log line.
//
// There is nothing to persist. The host is a native-messaging child of this
// worker, so it dies when the worker does; a restarted worker always faces a
// new host process with a new port and a new token, which makes any saved pair
// stale by construction.
let proxyToken = "";

let profileID = null;
let nativePort = null;
let initSent = false;
let reconnectDelay = RECONNECT_MIN_MS;
let reconnectTimer = null;
const popups = new Set();
// Resolves once the proxy settings left behind by an earlier life of this
// worker have been dealt with (see dropStaleProxy). applyProxy waits on it so a
// fast host cannot have its fresh PAC wiped by the startup sweep.
let proxySettled = Promise.resolve();

// The heartbeat alarm. Chromium may put the service worker to sleep with the
// host dead and a reconnect timer pending — timers do not survive that, alarms
// do. One minute is the MV3 floor.
const HEARTBEAT_ALARM = "tailtab-heartbeat";
const HEARTBEAT_MINUTES = 1;

// ---------------------------------------------------------------- proxy layer

// Firefox: the listener must exist from the moment the event page loads, so it
// is registered unconditionally and reads the current status when it fires.
if (USE_ON_REQUEST) {
  api.proxy.onRequest.addListener(
    (info) => {
      const port = status.proxyPort;
      if (!port) return { type: "direct" };
      let host = "";
      try {
        host = new URL(info.url).hostname;
      } catch (e) {
        return { type: "direct" };
      }
      const proxied = exitMode()
        ? tailtabExitModeProxies(host, status.subnetRoutes)
        : tailtabIsTailnetHost(host, status.tailnet, status.subnetRoutes);
      if (!proxied) return { type: "direct" };
      // proxyDNS keeps MagicDNS names unresolved until they reach the node.
      // Firefox can authenticate SOCKS5 in-protocol, so the credential rides
      // along here. With no token yet the request still goes to the proxy and
      // fails there, rather than leaking a tailnet name to the public DNS.
      const via = { type: "socks", host: "127.0.0.1", port: port, proxyDNS: true };
      if (proxyToken) {
        via.username = PROXY_USER;
        via.password = proxyToken;
      }
      return via;
    },
    { urls: ["<all_urls>"] }
  );
}

// How long a proxy challenge may wait for the host to report its port and
// token before we give up and decline. The whole point of asyncBlocking is that
// we are allowed this pause; declining instead is what puts Chromium's own
// password dialog in front of the user.
const AUTH_WAIT_MS = 8000;

// Whoever is waiting for the next status event, with the timer that gives up on
// it.
let statusWaiters = [];

function whenHostAnswers(timeoutMs) {
  if (status.proxyPort && proxyToken) return Promise.resolve();
  return new Promise((resolve) => {
    const waiter = {};
    waiter.done = () => {
      clearTimeout(waiter.timer);
      statusWaiters = statusWaiters.filter((w) => w !== waiter);
      resolve();
    };
    waiter.timer = setTimeout(waiter.done, timeoutMs);
    statusWaiters.push(waiter);
  });
}

function wakeStatusWaiters() {
  for (const waiter of statusWaiters.slice()) waiter.done();
}

// proxyAuthAnswer decides what to tell Chromium about one authentication
// challenge. It answers only our own listener: onAuthRequired fires for every
// 401 and 407 in the browser, so an unscoped handler would hand the token to
// any site or proxy that asked for one.
//
// It is async for one reason. Chromium can wake the service worker *with* this
// event, and a worker that has just started has no port and no token until the
// host answers its init, which is a round trip away. Answering "no credentials"
// in that window is what produces a proxy-password dialog, or the proxy's own
// 407 page, on a browser that is configured correctly and has simply not caught
// up yet.
async function proxyAuthAnswer(details) {
  if (!details || details.isProxy !== true) return {};
  const challenger = details.challenger || {};
  if (String(challenger.host) !== "127.0.0.1") return {};

  if (!proxyToken || !status.proxyPort) {
    // Make sure something is actually on its way, then wait for it.
    connect();
    await whenHostAnswers(AUTH_WAIT_MS);
  }
  if (!proxyToken || !status.proxyPort) return {};

  if (Number(challenger.port) !== Number(status.proxyPort)) {
    // The browser is still pointed at a proxy we no longer run — a PAC left
    // behind by an earlier host process, or by an earlier version of this
    // extension. Rewrite it so the next attempt reaches the live one, and
    // refuse this one: whatever is on that port now is not ours to trust.
    //
    // How to refuse depends on who is asking. A tailtab host announces itself
    // in the challenge (realm="tailtab"), so a stale one from an earlier life
    // is cancelled outright: nobody can type that password, and declining
    // would put Chromium's dialog up for it. Anything else on loopback is
    // somebody else's proxy, and declining leaves its own login alone.
    console.warn("tailtab: a proxy challenge came from port " + challenger.port + ", but our proxy is on " + status.proxyPort + "; reinstalling the proxy settings");
    applyProxy();
    return details.realm === PROXY_REALM ? { cancel: true } : {};
  }
  return { authCredentials: { username: PROXY_USER, password: proxyToken } };
}

// Chromium reaches the proxy over HTTP, because it cannot authenticate SOCKS5
// at all, and answers the listener's 407 here. Registered at the top level and
// unconditionally, like every other listener: a service worker woken for this
// event has no chance to register it later.
if (!USE_ON_REQUEST && api.webRequest && api.webRequest.onAuthRequired) {
  api.webRequest.onAuthRequired.addListener(
    (details, callback) => {
      // Always answer, and never cancel: a cancelled challenge is the dialog
      // again. The answer may take a moment, which asyncBlocking allows, but it
      // always comes.
      if (typeof callback !== "function") return;
      proxyAuthAnswer(details).then(callback, (e) => {
        console.warn("tailtab: answering a proxy challenge failed:", e);
        callback({});
      });
    },
    { urls: ["<all_urls>"] },
    ["asyncBlocking"]
  );
}

// applyProxy points Chromium at the proxy. On Firefox the listener above
// already covers it and there is nothing to install.
async function applyProxy() {
  if (USE_ON_REQUEST) return;
  await proxySettled;
  const port = status.proxyPort;
  if (!port) return;

  const control = await new Promise((resolve) =>
    chrome.proxy.settings.get({}, (v) => resolve(v && v.levelOfControl))
  );
  // Something else owns the setting. Fighting it would only produce a loop of
  // failed writes, so say so in the popup instead.
  if (control === "controlled_by_policy" || control === "controlled_by_other_extensions") {
    proxyProblem =
      control === "controlled_by_policy"
        ? "Proxy settings are locked by policy, so tailtab cannot route tailnet traffic."
        : "Another extension controls the proxy settings, so tailtab cannot route tailnet traffic.";
    pushToPopups();
    return;
  }
  // tailtabBuildPac refuses to hand Chromium a script it would reject. Better
  // to say so in the popup than to install nothing and report Connected.
  let pac;
  try {
    pac = tailtabBuildPac(port, status.tailnet, exitMode(), status.subnetRoutes);
  } catch (e) {
    proxyProblem = "tailtab could not build a proxy script, so tailnet traffic is not routed: " + e.message;
    pushToPopups();
    return;
  }

  // chrome.proxy.settings.set reports a rejected value through
  // runtime.lastError in the callback, and nowhere else: unread, it is an
  // "Unchecked runtime.lastError" line in a console nobody has open, while the
  // browser carries on with no proxy configuration and the popup says
  // Connected. That is how the non-ASCII PAC went unnoticed.
  const failure = await new Promise((resolve) =>
    chrome.proxy.settings.set(
      {
        scope: "regular",
        value: {
          mode: "pac_script",
          pacScript: { data: pac, mandatory: false },
        },
      },
      () => {
        const err = chrome.runtime.lastError;
        resolve(err && err.message ? err.message : "");
      }
    )
  );
  if (failure) {
    proxyProblem = "The browser rejected tailtab's proxy configuration, so tailnet traffic is not routed: " + failure;
    pushToPopups();
    return;
  }
  proxyProblem = "";
  pushToPopups();
}

// clearProxy hands the setting back. It runs on any transition out of Running,
// which includes the host process dying: onDisconnect reports Disconnected,
// and that is a transition out of Running like any other.
//
// Clearing on a crash is deliberate, and is the reverse of the first cut.
// Ordinary browsing is safe either way — the PAC only ever routes tailnet
// hosts, so leaving it installed could not break github.com — but tailnet
// requests are not: a PAC still pointing at a dead port makes every one of them
// hang until Chromium gives up on the connection, while no PAC at all makes
// them fail immediately. Failing fast is the better answer, and the setting is
// reinstalled as soon as the host is back with its new port and token.
async function clearProxy() {
  if (USE_ON_REQUEST) return;
  await new Promise((resolve) => chrome.proxy.settings.clear({ scope: "regular" }, resolve));
}

// dropStaleProxy runs once, when the worker starts. Chromium keeps an
// extension's proxy setting across service-worker restarts and extension
// reloads, but the host process does not survive either, so a PAC found at
// startup always points at a port from an earlier life. Seen live in Edge: the
// old host still answering 407 on the old port, the new worker holding a token
// for the new one, and a proxy-password dialog for the user. Until the new
// host is Running there is nothing to route through, and a direct failure is
// the honest answer; applyProxy installs the real thing as soon as there is.
async function dropStaleProxy() {
  if (USE_ON_REQUEST) return;
  const control = await new Promise((resolve) =>
    chrome.proxy.settings.get({}, (v) => resolve(v && v.levelOfControl))
  );
  if (control !== "controlled_by_this_extension") return;
  console.warn("tailtab: dropping the proxy settings left by an earlier worker");
  await clearProxy();
}

// ------------------------------------------------------------- native host IO

function connect() {
  if (nativePort) return;
  try {
    nativePort = api.runtime.connectNative(HOST_NAME);
  } catch (e) {
    scheduleReconnect(String(e));
    return;
  }
  initSent = false;
  nativePort.onMessage.addListener(onHostMessage);
  nativePort.onDisconnect.addListener(() => {
    const err = api.runtime.lastError;
    nativePort = null;
    initSent = false;
    const why = err && err.message ? err.message : "The tailtab host stopped.";
    // The credential dies with the process that would have honoured it.
    // Holding on to it could only produce an answer to a challenge from
    // whatever takes the port next.
    proxyToken = "";
    setStatus({
      state: "Disconnected",
      error: why,
      proxyPort: 0,
      tailnet: status.tailnet,
      warnings: [],
      // The selection is kept so the popup can still say what it was, but with
      // no proxy port nothing is routed anywhere until the host is back.
      exitNodes: status.exitNodes,
      exitNode: status.exitNode,
      exitNodeActive: false,
    });
    scheduleReconnect(why);
  });
  sendInit();
}

function scheduleReconnect(why) {
  if (reconnectTimer) return;
  const delay = reconnectDelay;
  reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
  console.warn("tailtab: reconnecting in " + delay + "ms:", why);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, delay);
}

function sendInit() {
  if (!nativePort || initSent || !profileID) return;
  initSent = true;
  nativePort.postMessage({ cmd: "init", profileID: profileID, browser: BROWSER });
}

function send(cmd, extra) {
  if (!nativePort) {
    connect();
    return false;
  }
  try {
    nativePort.postMessage(Object.assign({ cmd: cmd }, extra || null));
    return true;
  } catch (e) {
    console.error("tailtab: sending " + cmd + " failed:", e);
    return false;
  }
}

function onHostMessage(msg) {
  if (!msg || typeof msg !== "object") return;
  // The host answered, so this attempt worked and the backoff starts over.
  // Resetting in connect() instead would never let the delay grow:
  // connectNative does not throw for a missing or crashing host, it reports
  // the failure later on onDisconnect, so the reset always won the race and
  // left a flat one-second retry loop.
  reconnectDelay = RECONNECT_MIN_MS;
  if (msg.event === "error") {
    status = Object.assign({}, status, { error: msg.error || "unknown error" });
    pushToPopups();
    return;
  }
  if (msg.event !== "status") return;

  // Kept out of status: the popup is sent status, and the token must never go
  // there. It arrives with every status event and rotates with the host.
  proxyToken = msg.proxyToken || "";
  // A challenge may be parked waiting for exactly this.
  if (proxyToken && msg.proxyPort) wakeStatusWaiters();

  setStatus({
    state: msg.state || "",
    authURL: msg.authURL || "",
    tailnet: msg.tailnet || "",
    hostname: msg.hostname || "",
    selfIP: msg.selfIP || "",
    proxyPort: msg.proxyPort || 0,
    error: msg.error || "",
    // The exit node this profile routes through, if any. exitNode is the
    // selection and exitNodeActive is whether it is really carrying traffic;
    // the pair is what the popup explains and what the routing rules use.
    exitNodes: Array.isArray(msg.exitNodes) ? msg.exitNodes : [],
    exitNode: msg.exitNode || "",
    exitNodeActive: !!msg.exitNodeActive,
    // Why the node is unhealthy — a blocked control plane, for instance.
    warnings: Array.isArray(msg.warnings) ? msg.warnings : [],
    // The login profiles this node holds, and the tailnet's machines, for
    // the popup's account switcher and machine search.
    accounts: Array.isArray(msg.accounts) ? msg.accounts : [],
    peers: Array.isArray(msg.peers) ? msg.peers : [],
    // Subnets peers route for the tailnet; part of the routing rules.
    subnetRoutes: Array.isArray(msg.subnetRoutes) ? msg.subnetRoutes : [],
  });
}

function sameList(a, b) {
  const x = Array.isArray(a) ? a : [];
  const y = Array.isArray(b) ? b : [];
  if (x.length !== y.length) return false;
  for (let i = 0; i < x.length; i++) if (x[i] !== y[i]) return false;
  return true;
}

// setStatus records a new status and keeps the browser's proxy configuration in
// step with it.
//
// The decision is keyed on the Running transition, not on the port or tailnet
// changing. Disconnect and Connect against one host process leave both of those
// identical, so a diff-based guard would hand the proxy setting back on
// Disconnect and never take it again, leaving a popup that says Connected over
// a browser with no PAC at all.
function setStatus(next) {
  const previous = status;
  status = next;
  saveStatus();

  const wasRunning = previous.state === "Running";
  const isRunning = next.state === "Running";
  if (isRunning) {
    // The PAC embeds the port and the tailnet suffix, so it is also rewritten
    // whenever either of those changes underneath a live connection.
    // The exit node is in this list because it decides which rule the PAC
    // carries: choosing or clearing one rewrites the whole script.
    if (
      !wasRunning ||
      next.proxyPort !== previous.proxyPort ||
      next.tailnet !== previous.tailnet ||
      next.exitNode !== previous.exitNode ||
      !sameList(next.subnetRoutes, previous.subnetRoutes)
    ) {
      applyProxy();
    }
  } else if (wasRunning) {
    clearProxy();
  }
  pushToPopups();
}

// ------------------------------------------------------------------ popup wire

api.runtime.onConnect.addListener((port) => {
  if (port.name !== "popup") return;
  popups.add(port);
  port.onDisconnect.addListener(() => popups.delete(port));
  port.onMessage.addListener((msg) => {
    if (!msg || !msg.cmd) return;
    switch (msg.cmd) {
      case "up":
        send("up");
        break;
      case "down":
        // The proxy setting is handed back when the host reports that it has
        // left Running, not here: if the command fails, nothing should change.
        send("down");
        break;
      case "logout":
        send("logout");
        break;
      case "exitnode":
        // The host validates the id against the offers it knows; the popup
        // only ever passes on what the user picked.
        send("exitnode", { id: typeof msg.id === "string" ? msg.id : "" });
        break;
      case "switch":
        // Validated by the host against the profiles it holds.
        send("switch", { id: typeof msg.id === "string" ? msg.id : "" });
        break;
      case "addaccount":
        send("addaccount");
        break;
      case "status":
        send("status");
        break;
      case "reconnect":
        connect();
        break;
    }
    pushToPopups();
  });
  pushToPopups();
});

// iconState maps what the popup would say to the colour of the icon's tail
// dot: green when this profile is routing, red when browsing is blocked,
// orange when something needs the user, grey when nothing is running.
function iconState() {
  const st = status;
  if (!nativePort || st.state === "Disconnected") return "attention";
  if (st.state === "Running") {
    if (proxyProblem) return "blocked";
    if (st.exitNode && !st.exitNodeActive) return "blocked";
    return "connected";
  }
  if (st.state === "NeedsLogin" || st.state === "NeedsMachineAuth" || st.state === "Starting") return "attention";
  return "idle";
}

let shownIcon = "";
function updateIcon() {
  const next = iconState();
  if (next === shownIcon) return;
  shownIcon = next;
  const action = api.action || api.browserAction;
  if (!action || !action.setIcon) return;
  const path = {};
  for (const size of [16, 32, 48, 128]) path[size] = "icons/" + next + "/icon" + size + ".png";
  try {
    const r = action.setIcon({ path: path });
    if (r && r.catch) r.catch(() => {});
  } catch (e) {
    // An icon that fails to set is cosmetic; never let it break routing.
  }
}

function pushToPopups() {
  updateIcon();
  const payload = { status: status, proxyProblem: proxyProblem, browser: BROWSER, connected: !!nativePort, build: BUILD };
  for (const port of popups) {
    try {
      port.postMessage(payload);
    } catch (e) {
      popups.delete(port);
    }
  }
}

// --------------------------------------------------------------------- storage

function saveStatus() {
  try {
    if (api.storage && api.storage.session) api.storage.session.set({ status: status });
  } catch (e) {
    // storage.session is a convenience; losing it costs a status round-trip.
  }
}

// The profile ID names the node's state directory on disk. It is generated
// once and never regenerated: a new ID means a new node and a fresh login.
async function loadProfileID() {
  const stored = await api.storage.local.get("profileID");
  if (stored && stored.profileID) return stored.profileID;
  const id = crypto.randomUUID();
  await api.storage.local.set({ profileID: id });
  return id;
}

// ----------------------------------------------------------------- entry point

// Whatever proxy setting an earlier worker left behind goes first, before any
// host can report a port to route through.
proxySettled = dropStaleProxy().catch(() => {});

// Connect at the top level so the port is open as early as possible: on
// Chromium the open port is what keeps the service worker alive.
connect();

// Chromium wakes the worker for these; without them a browser restart leaves
// the host unstarted until the popup is opened.
api.runtime.onStartup.addListener(connect);
api.runtime.onInstalled.addListener(connect);

// The heartbeat: if the host is gone and the reconnect timer died with a
// sleeping worker, this is what brings it back without the user opening the
// popup. Registered at the top level, like every listener, so the alarm can
// wake the worker.
if (api.alarms) {
  api.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: HEARTBEAT_MINUTES });
  api.alarms.onAlarm.addListener((alarm) => {
    if (!alarm || alarm.name !== HEARTBEAT_ALARM) return;
    if (!nativePort) connect();
  });
}

loadProfileID().then((id) => {
  profileID = id;
  sendInit();
  // The service worker may have been restarted with a status already known.
  if (api.storage && api.storage.session) {
    api.storage.session.get("status").then((v) => {
      if (!v) return;
      if (v.status && !status.proxyPort) {
        status = Object.assign({}, status, { tailnet: v.status.tailnet || status.tailnet });
        pushToPopups();
      }
    }).catch(() => {});
  }
});
