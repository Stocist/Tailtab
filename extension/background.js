// Chromium runs this as an MV3 service worker; Gecko uses an event page.
// Register listeners synchronously at top level so event-page wakeups reach them.

"use strict";

// Service workers import rules.js; Gecko loads it first via background.scripts.
if (typeof importScripts === "function" && typeof tailtabIsTailnetHost === "undefined") {
  importScripts("rules.js");
}

const api = typeof browser !== "undefined" ? browser : chrome;

// Gecko filters each request; Chromium installs a PAC script.
const USE_ON_REQUEST = typeof api.proxy !== "undefined" && typeof api.proxy.onRequest !== "undefined";

// Gecko retains the historical "zen" hostname suffix; Chromium uses UA brands.
function detectBrowser() {
  if (USE_ON_REQUEST) return "zen";
  try {
    const brands = (navigator.userAgentData && navigator.userAgentData.brands) || [];
    const names = brands.map((b) => String(b.brand || "").toLowerCase());
    if (names.some((n) => n.indexOf("edge") !== -1)) return "edge";
    if (names.some((n) => n.indexOf("brave") !== -1)) return "brave";
    if (names.some((n) => n.indexOf("google chrome") !== -1)) return "chrome";
    if (names.some((n) => n.indexOf("chromium") !== -1)) return "chromium";
  } catch (e) {
  }
  return "edge";
}
const BROWSER = detectBrowser();
const HOST_NAME = "com.stocist.tailtab";
// A mismatch with the popup identifies Chromium's stale background worker.
const BUILD = "__TAILTAB_BUILD__";
// Chromium authenticates with this username and the host's per-process token.
const PROXY_USER = "tailtab";
// The realm distinguishes a stale tailtab host from another loopback proxy.
const PROXY_REALM = "tailtab";
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 30000;

let status = { state: "Disconnected", error: "", proxyPort: 0, tailnet: "", warnings: [], exitNode: "", exitNodes: [], exitNodeActive: false, accounts: [], peers: [], subnetRoutes: [] };

// Keep exit routing selected when its node is offline. The host then blocks
// public traffic instead of leaking it through the local connection.
function exitMode() {
  return !!status.exitNode;
}
let proxyProblem = "";
// Keep the secret token only in memory: never storage, popup messages, or logs.
// It expires with the native host, so a persisted port-token pair is always stale.
let proxyToken = "";

let profileID = null;
let nativePort = null;
// Only an explicit stop or logout may clear routing instead of parking it.
let userStopped = false;
let initSent = false;
let reconnectDelay = RECONNECT_MIN_MS;
let reconnectTimer = null;
const popups = new Set();
// Prevent the startup stale-PAC sweep from erasing a fast host's fresh PAC.
let proxySettled = Promise.resolve();

// MV3 sleep drops timers but alarms survive; one minute is Chromium's floor.
const HEARTBEAT_ALARM = "tailtab-heartbeat";
const HEARTBEAT_MINUTES = 1;

// Register immediately so Gecko can wake the event page for a request.
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
      // proxyDNS resolves MagicDNS inside the node. Without a token, fail at the
      // proxy rather than leaking the tailnet name to public DNS.
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

// asyncBlocking allows a newly awakened worker to wait without showing a
// Chromium proxy-password dialog.
const AUTH_WAIT_MS = 8000;

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

// onAuthRequired sees every browser 401 and 407, so scope the secret to this
// loopback proxy and its current port. A challenge can wake an empty worker;
// wait for the host's init response rather than triggering Chromium's dialog.
async function proxyAuthAnswer(details) {
  if (!details || details.isProxy !== true) return {};
  const challenger = details.challenger || {};
  if (String(challenger.host) !== "127.0.0.1") return {};

  if (!proxyToken || !status.proxyPort) {
    connect();
    await whenHostAnswers(AUTH_WAIT_MS);
  }
  if (!proxyToken || !status.proxyPort) return {};

  if (Number(challenger.port) !== Number(status.proxyPort)) {
    // A stale PAC can challenge from an old port. Reinstall current settings and
    // trust neither process; cancel stale tailtab realms to suppress Chromium's
    // dialog, but decline other loopback proxies so their login still works.
    console.warn("tailtab: a proxy challenge came from port " + challenger.port + ", but our proxy is on " + status.proxyPort + "; reinstalling the proxy settings");
    applyProxy();
    return details.realm === PROXY_REALM ? { cancel: true } : {};
  }
  return { authCredentials: { username: PROXY_USER, password: proxyToken } };
}

// Chromium cannot authenticate SOCKS5, so answer the HTTP proxy's 407. Register
// at top level because a worker woken by this event cannot add the listener later.
if (!USE_ON_REQUEST && api.webRequest && api.webRequest.onAuthRequired) {
  api.webRequest.onAuthRequired.addListener(
    (details, callback) => {
      // Always invoke the callback; cancellation reopens Chromium's dialog.
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

async function applyProxy() {
  if (USE_ON_REQUEST) return;
  await proxySettled;
  const port = status.proxyPort;
  if (!port) return;

  const control = await new Promise((resolve) =>
    chrome.proxy.settings.get({}, (v) => resolve(v && v.levelOfControl))
  );
  // Do not fight policy or another extension for proxy ownership.
  if (control === "controlled_by_policy" || control === "controlled_by_other_extensions") {
    proxyProblem =
      control === "controlled_by_policy"
        ? "Proxy settings are locked by policy, so tailtab cannot route tailnet traffic."
        : "Another extension controls the proxy settings, so tailtab cannot route tailnet traffic.";
    pushToPopups();
    return;
  }
  // Report PAC build failures rather than claiming traffic is routed.
  let pac;
  try {
    pac = tailtabBuildPac(port, status.tailnet, exitMode(), status.subnetRoutes);
  } catch (e) {
    proxyProblem = "tailtab could not build a proxy script, so tailnet traffic is not routed: " + e.message;
    pushToPopups();
    return;
  }

  // proxy.settings.set reports rejection only through runtime.lastError during
  // its callback; missing it would falsely report a routed connection.
  const failure = await new Promise((resolve) =>
    chrome.proxy.settings.set(
      {
        scope: "regular",
        value: {
          mode: "pac_script",
          pacScript: { data: pac, mandatory: true }, // a failing PAC blocks, never falls back to DIRECT
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

async function clearProxy() {
  if (USE_ON_REQUEST) return;
  await new Promise((resolve) => chrome.proxy.settings.clear({ scope: "regular" }, resolve));
}

// When the host disappears unexpectedly, keep the rules but point them at an
// unreachable address. Tailnet names then cannot leak to public DNS, and exit
// mode remains a kill switch until the host returns.
// Use 0.0.0.1 because another process can bind a loopback port, especially on
// Windows, while this address fails immediately on every platform.
const PARKED_HOST = "0.0.0.1";
const PARKED_PORT = 1;
async function parkProxy() {
  if (USE_ON_REQUEST) return;
  const control = await new Promise((resolve) =>
    chrome.proxy.settings.get({}, (v) => resolve(v && v.levelOfControl))
  );
  if (control === "controlled_by_policy" || control === "controlled_by_other_extensions") return;
  let pac;
  try {
    pac = tailtabBuildPac(PARKED_PORT, status.tailnet, exitMode(), status.subnetRoutes, PARKED_HOST);
  } catch (e) {
    await clearProxy();
    return;
  }
  const failure = await new Promise((resolve) =>
    chrome.proxy.settings.set({ scope: "regular", value: { mode: "pac_script", pacScript: { data: pac, mandatory: true } } }, () => {
      const err = chrome.runtime.lastError;
      resolve(err && err.message ? err.message : "");
    })
  );
  if (failure) {
    proxyProblem = "The browser rejected tailtab's proxy configuration: " + failure;
    pushToPopups();
  }
}

// Chromium persists PAC settings across worker restarts, but the native host
// does not survive. Park any inherited PAC until the new host is Running.
async function dropStaleProxy() {
  if (USE_ON_REQUEST) return;
  const control = await new Promise((resolve) =>
    chrome.proxy.settings.get({}, (v) => resolve(v && v.levelOfControl))
  );
  if (control !== "controlled_by_this_extension") return;
  console.warn("tailtab: parking the proxy settings left by an earlier worker");
  // Preserve only routing facts so custom names stay out of public DNS and an
  // existing exit-mode kill switch remains active.
  try {
    let st = null;
    const saved = await api.storage.session.get("status");
    if (saved && saved.status && typeof saved.status === "object") st = saved.status;
    if (!st) {
      // Browser restarts erase session storage, so use the local routing snapshot.
      const local = await api.storage.local.get("rules");
      if (local && local.rules && typeof local.rules === "object") st = local.rules;
    }
    if (st && typeof st === "object") {
      status = Object.assign({}, status, {
        tailnet: typeof st.tailnet === "string" ? st.tailnet : "",
        subnetRoutes: Array.isArray(st.subnetRoutes) ? st.subnetRoutes : [],
        exitNode: typeof st.exitNode === "string" ? st.exitNode : "",
      });
    }
  } catch (e) {
  }
  await parkProxy();
}

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
    // Never offer a dead host's credential to whatever takes its port next.
    proxyToken = "";
    setStatus({
      state: "Disconnected",
      error: why,
      proxyPort: 0,
      tailnet: status.tailnet,
      warnings: [],
      // Retain the selection for fail-closed exit routing after reconnection.
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

let controlURL = "";

function sendInit() {
  if (!nativePort || initSent || !profileID) return;
  initSent = true;
  const msg = { cmd: "init", profileID: profileID, browser: BROWSER };
  if (controlURL) msg.controlURL = controlURL;
  nativePort.postMessage(msg);
}

function loadControlURL() {
  return api.storage.local
    .get("controlURL")
    .then((v) => {
      controlURL = (v && typeof v.controlURL === "string") ? v.controlURL : "";
      return controlURL;
    })
    .catch(() => controlURL);
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
  // Reset only after a host response. connectNative reports later through
  // onDisconnect, so resetting in connect() races and defeats backoff.
  reconnectDelay = RECONNECT_MIN_MS;
  if (msg.event === "error") {
    status = Object.assign({}, status, { error: msg.error || "unknown error" });
    pushToPopups();
    return;
  }
  if (msg.event !== "status") return;

  // Keep the rotating token outside status because status is sent to popups.
  proxyToken = msg.proxyToken || "";
  if (proxyToken && msg.proxyPort) wakeStatusWaiters();

  setStatus({
    state: msg.state || "",
    authURL: msg.authURL || "",
    tailnet: msg.tailnet || "",
    hostname: msg.hostname || "",
    selfIP: msg.selfIP || "",
    proxyPort: msg.proxyPort || 0,
    error: msg.error || "",
    exitNodes: Array.isArray(msg.exitNodes) ? msg.exitNodes : [],
    exitNode: msg.exitNode || "",
    exitNodeActive: !!msg.exitNodeActive,
    warnings: Array.isArray(msg.warnings) ? msg.warnings : [],
    accounts: Array.isArray(msg.accounts) ? msg.accounts : [],
    peers: Array.isArray(msg.peers) ? msg.peers : [],
    subnetRoutes: Array.isArray(msg.subnetRoutes) ? msg.subnetRoutes : [],
    controlURL: msg.controlURL || "",
  });
}

function sameList(a, b) {
  const x = Array.isArray(a) ? a : [];
  const y = Array.isArray(b) ? b : [];
  if (x.length !== y.length) return false;
  for (let i = 0; i < x.length; i++) if (x[i] !== y[i]) return false;
  return true;
}

// Key routing on Running transitions: disconnect and reconnect can reuse the
// same port and tailnet, so a value-only diff would fail to reinstall the PAC.
function setStatus(next) {
  const previous = status;
  status = next;
  saveStatus();

  const wasRunning = previous.state === "Running";
  const isRunning = next.state === "Running";
  if (isRunning) {
    // Rewrite whenever an embedded PAC input changes.
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
    // Explicit stops clear routing; failures park it to prevent DNS leaks.
    if (userStopped && (next.state === "Stopped" || next.state === "NeedsLogin")) clearProxy();
    else parkProxy();
  }
  if (next.state === "Running") userStopped = false;
  saveRulesSnapshot(next);
  pushToPopups();
}

// Only the popup page may drive the host; a content script added later must not.
function fromPopup(sender) {
  return !!sender && sender.id === api.runtime.id && typeof sender.url === "string" &&
    sender.url.split(/[?#]/)[0] === api.runtime.getURL("popup.html");
}

api.runtime.onConnect.addListener((port) => {
  if (port.name !== "popup" || !fromPopup(port.sender)) return;
  popups.add(port);
  port.onDisconnect.addListener(() => popups.delete(port));
  port.onMessage.addListener((msg) => {
    if (!msg || !msg.cmd) return;
    switch (msg.cmd) {
      case "up":
        send("up");
        break;
      case "down":
        // Wait for host confirmation so a failed command cannot clear routing.
        userStopped = true;
        send("down");
        break;
      case "logout":
        userStopped = true;
        send("logout");
        break;
      case "exitnode":
        send("exitnode", { id: typeof msg.id === "string" ? msg.id : "" });
        break;
      case "switch":
        send("switch", { id: typeof msg.id === "string" ? msg.id : "" });
        break;
      case "addaccount":
        loadControlURL().then((url) => {
          send("addaccount", url ? { controlURL: url } : null);
        });
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
    // Icon failures must not affect routing.
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

// Persist routing facts, never the token, so a post-restart stale PAC can be
// parked without leaking custom names or disabling exit mode.
function saveRulesSnapshot(st) {
  try {
    api.storage.local.set({ rules: { tailnet: st.tailnet || "", subnetRoutes: Array.isArray(st.subnetRoutes) ? st.subnetRoutes : [], exitNode: st.exitNode || "" } });
  } catch (e) {
  }
}

function saveStatus() {
  try {
    if (api.storage && api.storage.session) api.storage.session.set({ status: status });
  } catch (e) {
  }
}

// Regenerating the profile ID creates a new node and requires a fresh login.
async function loadProfileID() {
  const stored = await api.storage.local.get("profileID");
  if (stored && stored.profileID) return stored.profileID;
  const id = crypto.randomUUID();
  await api.storage.local.set({ profileID: id });
  return id;
}

// Settle inherited proxy state before accepting a fresh host PAC.
proxySettled = dropStaleProxy().catch(() => {});

// Chromium keeps the service worker alive while this native port is open.
connect();

// Register at top level so startup events can wake the worker and native host.
api.runtime.onStartup.addListener(connect);
api.runtime.onInstalled.addListener(connect);

// The top-level alarm listener replaces reconnect timers lost during MV3 sleep.
if (api.alarms) {
  api.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: HEARTBEAT_MINUTES });
  api.alarms.onAlarm.addListener((alarm) => {
    if (!alarm || alarm.name !== HEARTBEAT_ALARM) return;
    if (!nativePort) connect();
  });
}

Promise.all([loadProfileID(), loadControlURL()]).then(([id]) => {
  profileID = id;
  sendInit();
  // A restarted worker may still have session status from its previous life.
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
