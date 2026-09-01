// tailtab popup. Everything shown here is derived from the last status event
// the background script received: the popup keeps no idea of its own about
// whether the node is up.

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

port.onMessage.addListener(render);
port.postMessage({ cmd: "status" });

function render(msg) {
  latest = msg;
  const st = msg.status || {};
  const state = st.state || "Unknown";
  const running = state === "Running";

  el("state").textContent = running ? "Connected" : state;
  el("hint").textContent = st.error || HINTS[state] || "";

  el("details").hidden = !running;
  if (running) {
    el("tailnet").textContent = st.tailnet || "unknown";
    el("hostname").textContent = st.hostname || "unknown";
    el("selfip").textContent = st.selfIP || "unknown";
    el("port").textContent = st.proxyPort ? "127.0.0.1:" + st.proxyPort : "not listening";
  }

  el("login").hidden = !st.authURL;
  el("connect").hidden = running || !!st.authURL || !msg.connected;
  el("disconnect").hidden = !running;
  el("logout").hidden = !msg.connected || (!running && state !== "Stopped");

  const warning = el("warning");
  if (msg.proxyProblem) {
    warning.hidden = false;
    warning.textContent = msg.proxyProblem;
  } else if (!msg.connected) {
    warning.hidden = false;
    warning.textContent = "Not connected to the tailtab host. Reconnecting…";
  } else {
    warning.hidden = true;
    warning.textContent = "";
  }

  if (!logoutArmed) el("logout").textContent = "Log out";
}

el("login").addEventListener("click", () => {
  const url = latest && latest.status && latest.status.authURL;
  if (!url) return;
  api.tabs.create({ url: url });
  window.close();
});

el("connect").addEventListener("click", () => port.postMessage({ cmd: "up" }));
el("disconnect").addEventListener("click", () => port.postMessage({ cmd: "down" }));

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
