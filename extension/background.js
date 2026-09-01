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
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 30000;

// The last status event from the host, and the only thing the UI renders.
let status = { state: "Disconnected", error: "", proxyPort: 0, tailnet: "" };
// Why the proxy could not be configured, if it could not be.
let proxyProblem = "";

let profileID = null;
let nativePort = null;
let initSent = false;
let reconnectDelay = RECONNECT_MIN_MS;
let reconnectTimer = null;
const popups = new Set();

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
      if (!tailtabIsTailnetHost(host, status.tailnet)) return { type: "direct" };
      // proxyDNS keeps MagicDNS names unresolved until they reach the node.
      return { type: "socks", host: "127.0.0.1", port: port, proxyDNS: true };
    },
    { urls: ["<all_urls>"] }
  );
}

// applyProxy points Chromium at the proxy. On Firefox the listener above
// already covers it and there is nothing to install.
async function applyProxy() {
  if (USE_ON_REQUEST) return;
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
  proxyProblem = "";
  await new Promise((resolve) =>
    chrome.proxy.settings.set(
      {
        scope: "regular",
        value: {
          mode: "pac_script",
          pacScript: { data: tailtabBuildPac(port, status.tailnet), mandatory: false },
        },
      },
      resolve
    )
  );
}

// clearProxy hands the setting back. It runs on any transition out of Running.
// Ordinary browsing is unaffected either way: the PAC only ever routes tailnet
// hosts, so leaving it installed could not break anything.
async function clearProxy() {
  if (USE_ON_REQUEST) return;
  await new Promise((resolve) => chrome.proxy.settings.clear({ scope: "regular" }, resolve));
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
    setStatus({
      state: "Disconnected",
      error: why,
      proxyPort: 0,
      tailnet: status.tailnet,
    });
    scheduleReconnect(why);
  });
  reconnectDelay = RECONNECT_MIN_MS;
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

function send(cmd) {
  if (!nativePort) {
    connect();
    return false;
  }
  try {
    nativePort.postMessage({ cmd: cmd });
    return true;
  } catch (e) {
    console.error("tailtab: sending " + cmd + " failed:", e);
    return false;
  }
}

function onHostMessage(msg) {
  if (!msg || typeof msg !== "object") return;
  if (msg.event === "error") {
    status = Object.assign({}, status, { error: msg.error || "unknown error" });
    pushToPopups();
    return;
  }
  if (msg.event !== "status") return;

  setStatus({
    state: msg.state || "",
    authURL: msg.authURL || "",
    tailnet: msg.tailnet || "",
    hostname: msg.hostname || "",
    selfIP: msg.selfIP || "",
    proxyPort: msg.proxyPort || 0,
    error: msg.error || "",
  });
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
    if (!wasRunning || next.proxyPort !== previous.proxyPort || next.tailnet !== previous.tailnet) {
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

function pushToPopups() {
  const payload = { status: status, proxyProblem: proxyProblem, browser: BROWSER, connected: !!nativePort };
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

// Connect at the top level so the port is open as early as possible: on
// Chromium the open port is what keeps the service worker alive.
connect();

// Chromium wakes the worker for these; without them a browser restart leaves
// the host unstarted until the popup is opened.
api.runtime.onStartup.addListener(connect);
api.runtime.onInstalled.addListener(connect);

loadProfileID().then((id) => {
  profileID = id;
  sendInit();
  // The service worker may have been restarted with a status already known.
  if (api.storage && api.storage.session) {
    api.storage.session.get("status").then((v) => {
      if (v && v.status && !status.proxyPort) {
        status = Object.assign({}, status, { tailnet: v.status.tailnet || status.tailnet });
        pushToPopups();
      }
    }).catch(() => {});
  }
});
