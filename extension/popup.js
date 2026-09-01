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
// Set when Connect is pressed and cleared once the host answers with a login
// URL or an error. Asking control for a URL can take a while, and can fail
// silently when the network is blocked, which made the button look dead.
let awaitingLogin = false;

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

  const warnings = Array.isArray(st.warnings) ? st.warnings : [];
  if (st.authURL || st.error || running || state === "Starting") {
    awaitingLogin = false;
  }

  // A login URL supersedes the last login error: the error explains a failure
  // the URL has already moved past, and showing it beside a working Log in
  // button reads as though the button will not work. It stays in the warnings
  // list below, so a node that genuinely cannot reach control still says so.
  const errorLine = st.authURL ? "" : st.error;

  el("state").textContent = running ? "Connected" : state;
  el("hint").textContent = awaitingLogin
    ? "Requesting login link…"
    : errorLine || HINTS[state] || "";

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

  el("details").hidden = !running;
  if (running) {
    el("tailnet").textContent = st.tailnet || "unknown";
    el("hostname").textContent = st.hostname || "unknown";
    el("selfip").textContent = st.selfIP || "unknown";
    el("port").textContent = st.proxyPort ? "127.0.0.1:" + st.proxyPort : "not listening";
  }

  // A login URL is always offered when there is one, warnings or not.
  el("login").hidden = !st.authURL;
  el("connect").hidden = running || !!st.authURL || !msg.connected;
  el("connect").disabled = awaitingLogin;
  el("connect").textContent = awaitingLogin ? "Requesting…" : "Connect";
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

el("connect").addEventListener("click", () => {
  const st = (latest && latest.status) || {};
  // Only NeedsLogin waits on control for a URL; from Stopped, up is immediate.
  awaitingLogin = st.state === "NeedsLogin" && !st.authURL;
  if (awaitingLogin) {
    el("hint").textContent = "Requesting login link…";
    el("connect").disabled = true;
    el("connect").textContent = "Requesting…";
  }
  port.postMessage({ cmd: "up" });
});
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
