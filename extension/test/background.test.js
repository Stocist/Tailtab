// Tests for the tailtab extension, run under node:
//
//   node extension/test/background.test.js     (or ./scripts/test.sh)
//
// background.js is loaded unmodified into a stubbed Chromium MV3 environment —
// chrome.runtime, chrome.proxy.settings, chrome.storage, and an importScripts
// that resolves against the real extension directory, so the service-worker
// path that pulls in rules.js is the one under test. Timers are fake: the
// reconnect delays are a thing to assert on, not to wait for.
//
// This harness began as the verifier's reproduction of B1 and F1 (REVIEW.md);
// the two throwaway scripts were merged here and turned into assertions so the
// two defects stay fixed.

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const SRC = path.resolve(__dirname, "..");
const read = (f) => fs.readFileSync(path.join(SRC, f), "utf8");

// flush lets the background script's promise chains run to completion.
const flush = () => new Promise((r) => setImmediate(() => setImmediate(() => setImmediate(r))));

// pacTarget pulls the proxy a PAC script would return, so tests can assert on
// "PROXY 127.0.0.1:64378" rather than on generated JavaScript.
function pacTarget(value) {
  if (!value || !value.pacScript || typeof value.pacScript.data !== "string") {
    return JSON.stringify(value);
  }
  const m = value.pacScript.data.match(/(?:PROXY|SOCKS5) [\d.]+:\d+/);
  return m ? m[0] : "pac without a proxy target";
}

// makeEnv builds a stubbed Chromium and loads background.js into it.
function makeEnv(options) {
  const opts = options || {};
  const log = { set: [], clear: [], sent: [], timers: [], connects: 0, localSet: [], sessionSet: [] };
  const mkEvent = () => {
    const fns = [];
    return { addListener: (f) => fns.push(f), _fire: (...a) => fns.forEach((f) => f(...a)) };
  };

  let nativePort = null;
  const popupMessages = [];
  const chrome = {
    runtime: {
      lastError: null,
      connectNative() {
        log.connects++;
        nativePort = {
          onMessage: mkEvent(),
          onDisconnect: mkEvent(),
          postMessage: (m) => log.sent.push(m.cmd),
          disconnect() {},
        };
        return nativePort;
      },
      connect: () => ({ onMessage: mkEvent(), onDisconnect: mkEvent(), postMessage() {} }),
      onConnect: mkEvent(),
      onStartup: mkEvent(),
      onInstalled: mkEvent(),
    },
    proxy: {
      settings: {
        get: (_details, cb) => cb({ levelOfControl: opts.levelOfControl || "controllable_by_this_extension" }),
        set: (details, cb) => { log.set.push(pacTarget(details.value)); if (cb) cb(); },
        clear: (details, cb) => { log.clear.push(details.scope); if (cb) cb(); },
      },
    },
    storage: {
      local: {
        get: async () => ({ profileID: "0f8fad5b-d9cb-469f-a165-70867728950e" }),
        set: async (v) => { log.localSet.push(v); },
      },
      session: {
        get: async () => (opts.session || {}),
        set: async (v) => { log.sessionSet.push(v); },
      },
    },
    tabs: { create() {} },
    webRequest: {
      onAuthRequired: {
        addListener: (fn, filter, extra) => {
          log.authListener = { fn: fn, filter: filter, extra: extra };
        },
      },
    },
  };

  let ctx;
  const sandbox = {
    chrome: chrome,
    console: { log() {}, warn() {}, error() {} },
    crypto: crypto,
    URL: URL,
    // Recording timers: the delay is the assertion, and the callback is fired
    // by hand so a reconnect happens exactly when the test says so.
    setTimeout: (fn, ms) => { log.timers.push({ fn: fn, ms: ms }); return log.timers.length; },
    clearTimeout: () => {},
    importScripts: (f) => vm.runInContext(read(f), ctx, { filename: f }),
  };
  sandbox.self = sandbox;
  sandbox.globalThis = sandbox;
  ctx = vm.createContext(sandbox);
  vm.runInContext(read("background.js"), ctx, { filename: "background.js" });

  return {
    ctx: ctx,
    log: log,
    // popupMessages holds everything the background pushed to an open popup.
    popupMessages: popupMessages,
    // openPopup connects a popup port, the way clicking the toolbar icon does.
    openPopup() {
      const port = {
        name: "popup",
        onMessage: mkEvent(),
        onDisconnect: mkEvent(),
        postMessage: (m) => popupMessages.push(m),
      };
      chrome.runtime.onConnect._fire(port);
      return port;
    },
    // status pushes one status event from the host.
    status(fields) {
      nativePort.onMessage._fire(Object.assign(
        { event: "status", state: "", proxyPort: 0, tailnet: "" },
        fields
      ));
    },
    // disconnect drops the native port, as a dead host would.
    disconnect() { nativePort.onDisconnect._fire(); },
    // popup sends a command the way the popup's port does.
    popup(cmd) { vm.runInContext("send(" + JSON.stringify(cmd) + ");", ctx); },
    // authRequired fires an authentication challenge at the registered
    // onAuthRequired listener and returns what it answered.
    authRequired(details) {
      if (!log.authListener) throw new Error("no onAuthRequired listener was registered");
      let answer;
      log.authListener.fn(details, (a) => { answer = a; });
      if (answer === undefined) throw new Error("the listener never answered the challenge");
      return answer;
    },
    authListener: () => log.authListener,
    // runNextTimer fires the pending reconnect timer and returns its delay.
    runNextTimer() {
      const t = log.timers.shift();
      if (!t) throw new Error("no timer was scheduled");
      t.fn();
      return t.ms;
    },
  };
}

// makeFirefoxEnv builds a stubbed Zen: an MV3 event page with
// browser.proxy.onRequest, rules.js already loaded, and no importScripts. It is
// the other half of the proxy layer, and the only place the SOCKS credential
// is used.
function makeFirefoxEnv() {
  const log = { onRequest: null, sent: [], timers: [], sessionSet: [], localSet: [] };
  const mkEvent = () => {
    const fns = [];
    return { addListener: (f) => fns.push(f), _fire: (...a) => fns.forEach((f) => f(...a)) };
  };
  let nativePort = null;
  const browserApi = {
    runtime: {
      lastError: null,
      connectNative() {
        nativePort = {
          onMessage: mkEvent(),
          onDisconnect: mkEvent(),
          postMessage: (m) => log.sent.push(m.cmd),
          disconnect() {},
        };
        return nativePort;
      },
      onConnect: mkEvent(),
      onStartup: mkEvent(),
      onInstalled: mkEvent(),
    },
    proxy: { onRequest: { addListener: (fn) => { log.onRequest = fn; } } },
    storage: {
      local: {
        get: async () => ({ profileID: "0f8fad5b-d9cb-469f-a165-70867728950e" }),
        set: async (v) => { log.localSet.push(v); },
      },
      session: { get: async () => ({}), set: async (v) => { log.sessionSet.push(v); } },
    },
    tabs: { create() {} },
  };
  const sandbox = {
    browser: browserApi,
    console: { log() {}, warn() {}, error() {} },
    crypto: crypto,
    URL: URL,
    setTimeout: (fn, ms) => { log.timers.push({ fn: fn, ms: ms }); return log.timers.length; },
    clearTimeout: () => {},
  };
  sandbox.self = sandbox;
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  // The event page lists rules.js first in background.scripts.
  vm.runInContext(read("rules.js"), ctx, { filename: "rules.js" });
  vm.runInContext(read("background.js"), ctx, { filename: "background.js" });

  return {
    ctx: ctx,
    log: log,
    status(fields) {
      nativePort.onMessage._fire(Object.assign(
        { event: "status", state: "", proxyPort: 0, tailnet: "" },
        fields
      ));
    },
    // resolve asks the split-tunnel listener what to do with a URL.
    resolve(url) {
      if (!log.onRequest) throw new Error("no proxy.onRequest listener was registered");
      return log.onRequest({ url: url });
    },
  };
}

const RUNNING = {
  state: "Running",
  proxyPort: 64378,
  tailnet: "tail4d5e6f.ts.net",
  hostname: "mac-tailtab-edge",
  selfIP: "100.64.0.9",
};

const tests = [];
const test = (name, fn) => tests.push({ name: name, fn: fn });

function eq(got, want, what) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(what + ": got " + g + ", want " + w);
}

// B1 (REVIEW.md). Disconnect and Connect against one host process leave the
// port and the tailnet unchanged, so a guard that watches those hands the PAC
// back on Disconnect and never takes it again — a popup reading Connected over
// a browser with no PAC at all.
test("the PAC is reinstalled after a Disconnect and Connect on one host", async () => {
  const env = makeEnv();
  await flush();

  env.status(RUNNING);
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:64378"], "PAC installed on the first Running");

  env.popup("down");
  env.status(Object.assign({}, RUNNING, { state: "Stopped" }));
  await flush();
  eq(env.log.clear, ["regular"], "PAC handed back on Disconnect");

  env.popup("up");
  env.status(Object.assign({}, RUNNING, { state: "Starting" }));
  await flush();
  env.status(RUNNING); // same process: same port, same tailnet
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:64378", "PROXY 127.0.0.1:64378"], "PAC reinstalled on Connect");
  eq(env.log.sent, ["init", "down", "up"], "commands reaching the host");
});

test("a host restart on a new port reinstalls the PAC", async () => {
  const env = makeEnv();
  await flush();

  env.status({ state: "Running", proxyPort: 1111, tailnet: "tail4d5e6f.ts.net" });
  await flush();
  env.disconnect();
  await flush();
  eq(env.log.clear, ["regular"], "PAC handed back when the host dies");

  env.runNextTimer(); // the reconnect
  env.status({ state: "Running", proxyPort: 2222, tailnet: "tail4d5e6f.ts.net" });
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:1111", "PROXY 127.0.0.1:2222"], "PAC follows the new port");
});

test("a tailnet rename while Running rewrites the PAC", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  env.status(Object.assign({}, RUNNING, { tailnet: "tail1a2b3c.ts.net" }));
  await flush();
  eq(env.log.set.length, 2, "PAC rewritten for the new tailnet suffix");
});

test("proxy settings owned by policy are left alone", async () => {
  const env = makeEnv({ levelOfControl: "controlled_by_policy" });
  await flush();
  env.status(RUNNING);
  await flush();
  eq(env.log.set, [], "no attempt to take a setting owned by policy");
  const problem = vm.runInContext("proxyProblem", env.ctx);
  if (!problem) throw new Error("the popup was told nothing about the policy");
});

// F1 (REVIEW.md). The reset used to run in connect(), which always beat the
// doubling: connectNative does not throw for a missing host, it reports the
// failure later on onDisconnect.
test("the reconnect backoff doubles to the 30s cap", async () => {
  const env = makeEnv();
  await flush();

  const delays = [];
  for (let i = 0; i < 6; i++) {
    env.disconnect();
    await flush();
    delays.push(env.runNextTimer());
  }
  eq(delays, [1000, 2000, 4000, 8000, 16000, 30000], "backoff delays");
});

test("the backoff resets once the host answers", async () => {
  const env = makeEnv();
  await flush();

  env.disconnect();
  await flush();
  eq(env.runNextTimer(), 1000, "first delay");
  env.disconnect();
  await flush();
  eq(env.runNextTimer(), 2000, "second delay");

  env.status(RUNNING);
  await flush();
  env.disconnect();
  await flush();
  eq(env.runNextTimer(), 1000, "delay after a successful exchange");
});

// The split-tunnel rules, through the predicate and through the PAC generated
// from it. The two must agree on every host, since they are one source.
function rulesContext() {
  const ctx = vm.createContext({});
  vm.runInContext(read("rules.js"), ctx, { filename: "rules.js" });
  return ctx;
}

// Chromium's `<local>` bypass token means "hostnames without dots", which is
// exactly the set of MagicDNS short names that must be proxied. It is not used
// here and must not be reintroduced: the loopback exclusion is by name and
// address only. This test is the guard on that.
test("single-label MagicDNS names are proxied and loopback is not", () => {
  const rules = rulesContext();
  const isTailnet = (h) => rules.tailtabIsTailnetHost(h, "tail4d5e6f.ts.net");
  const findProxy = vm.runInContext(
    rules.tailtabBuildPac(51234, "tail4d5e6f.ts.net") + "\nFindProxyForURL",
    vm.createContext({})
  );

  for (const host of ["wiki", "server"]) {
    if (isTailnet(host) !== true) throw new Error(host + " goes DIRECT; a MagicDNS short name must be proxied");
    if (findProxy("http://" + host + "/", host) !== "PROXY 127.0.0.1:51234") {
      throw new Error("the PAC sends " + host + " DIRECT; a MagicDNS short name must be proxied");
    }
  }
  for (const host of ["localhost", "127.0.0.1", "127.5.5.5", "::1", "dev.localhost"]) {
    if (isTailnet(host) !== false) throw new Error(host + " is proxied; loopback must stay DIRECT");
    if (findProxy("http://" + host + "/", host) !== "DIRECT") {
      throw new Error("the PAC proxies " + host + "; loopback must stay DIRECT");
    }
  }
});

test("the split-tunnel rules and the generated PAC agree", () => {
  const rules = rulesContext();
  const isTailnet = rules.tailtabIsTailnetHost;
  const findProxy = vm.runInContext(
    rules.tailtabBuildPac(51234, "tail4d5e6f.ts.net") + "\nFindProxyForURL",
    vm.createContext({})
  );

  const proxied = [
    "wiki", "server", "WIKI", "wiki.tail4d5e6f.ts.net", "wiki.tail4d5e6f.ts.net.",
    "WIKI.TAIL4D5E6F.TS.NET", "host.tail1a2b3c.ts.net", "tail4d5e6f.ts.net",
    "100.64.0.1", "100.101.102.103", "100.127.255.255",
    "fd7a:115c:a1e0::1", "[fd7a:115c:a1e0:ab12::1]",
  ];
  const direct = [
    "", "github.com", "www.google.com", "8.8.8.8", "localhost", "dev.localhost",
    "127.0.0.1", "::1", "192.168.1.1", "100.63.255.255", "100.128.0.1", "fd00::1",
    "evil-ts.net", "notts.net", "ts.net.attacker.com",
    "wiki.tail4d5e6f.ts.net.attacker.com", "100.64.0.1.evil.com", "127.example.com",
    // Obfuscated forms of 127.0.0.1, which are not MagicDNS names.
    "2130706433", "0x7f000001",
  ];
  for (const host of proxied) {
    if (isTailnet(host, "tail4d5e6f.ts.net") !== true) throw new Error("predicate sends " + JSON.stringify(host) + " direct, want proxied");
    if (findProxy("http://" + host + "/", host) !== "PROXY 127.0.0.1:51234") throw new Error("PAC sends " + JSON.stringify(host) + " direct, want proxied");
  }
  for (const host of direct) {
    if (isTailnet(host, "tail4d5e6f.ts.net") !== false) throw new Error("predicate proxies " + JSON.stringify(host) + ", want direct");
    if (findProxy("http://" + host + "/", host) !== "DIRECT") throw new Error("PAC proxies " + JSON.stringify(host) + ", want direct");
  }
});

// The live A6 failure: the node was logged out because it could not reach the
// control plane, and the popup showed a bare NeedsLogin with a Connect button
// that looked dead. The reason has to reach the popup, and so does an auth URL
// that only arrives after the popup is already open.
test("a login URL that arrives after the popup is open reaches it", async () => {
  const env = makeEnv();
  await flush();
  const popup = env.openPopup();
  await flush();
  if (env.popupMessages.length === 0) throw new Error("the popup got nothing when it connected");

  env.status({ state: "NeedsLogin", proxyPort: 64378 });
  await flush();
  const before = env.popupMessages[env.popupMessages.length - 1];
  if (before.status.authURL) throw new Error("an auth URL appeared before the host sent one");

  // BrowseToURL lands on the bus seconds later and the host pushes it.
  env.status({ state: "NeedsLogin", proxyPort: 64378, authURL: "https://login.tailscale.com/a/deadbeef" });
  await flush();
  const after = env.popupMessages[env.popupMessages.length - 1];
  eq(after.status.authURL, "https://login.tailscale.com/a/deadbeef", "auth URL delivered to the open popup");
  if (popup.name !== "popup") throw new Error("the harness connected the wrong port");
});

test("health warnings reach an open popup", async () => {
  const env = makeEnv();
  await flush();
  env.openPopup();
  await flush();

  env.status({
    state: "NeedsLogin",
    proxyPort: 64378,
    error: "You are logged out. The last login error was: all connection attempts failed",
    warnings: [
      "You are logged out. The last login error was: all connection attempts failed",
      "Cannot reach the coordination server",
    ],
  });
  await flush();

  const last = env.popupMessages[env.popupMessages.length - 1];
  eq(last.status.warnings.length, 2, "warnings delivered to the popup");
  if (!last.status.error.includes("all connection attempts failed")) {
    throw new Error("the login failure did not reach the popup: " + JSON.stringify(last.status.error));
  }
});

test("a popup command reaches the host and the popup is answered", async () => {
  const env = makeEnv();
  await flush();
  const popup = env.openPopup();
  await flush();
  env.status({ state: "NeedsLogin", proxyPort: 64378 });
  await flush();

  const before = env.popupMessages.length;
  popup.onMessage._fire({ cmd: "up" });
  await flush();
  eq(env.log.sent[env.log.sent.length - 1], "up", "the up command reached the host");
  if (env.popupMessages.length <= before) throw new Error("the popup was not answered after its command");
});

// popup.js renders against a DOM, so it gets a small stub of one. The popup
// keeps no state of its own: everything below is a function of the last status
// payload the background pushed.
function openPopupUI() {
  const els = {};
  const makeEl = () => {
    const el = { hidden: false, disabled: false, children: [], listeners: {} };
    let text = "";
    Object.defineProperty(el, "textContent", {
      get: () => text,
      set: (v) => { text = v; el.children.length = 0; },
    });
    el.appendChild = (child) => el.children.push(child);
    el.addEventListener = (name, fn) => { el.listeners[name] = fn; };
    return el;
  };
  for (const id of ["state", "hint", "warnings", "details", "tailnet", "hostname", "selfip", "port", "login", "connect", "disconnect", "logout", "warning"]) {
    els[id] = makeEl();
  }

  let backgroundPort = null;
  const sandbox = {
    console: { log() {}, warn() {}, error() {} },
    setTimeout: () => 1,
    clearTimeout: () => {},
    window: { close() {} },
    document: {
      getElementById: (id) => els[id],
      createElement: () => makeEl(),
    },
    chrome: {
      runtime: {
        connect: () => {
          backgroundPort = {
            listeners: [],
            onMessage: { addListener: (f) => backgroundPort.listeners.push(f) },
            postMessage: () => {},
          };
          return backgroundPort;
        },
      },
      tabs: { create() {} },
    },
  };
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  vm.runInContext(read("popup.js"), ctx, { filename: "popup.js" });

  return {
    els: els,
    push(payload) {
      for (const fn of backgroundPort.listeners) fn(payload);
    },
    warningTexts: () => els.warnings.children.map((c) => c.textContent),
  };
}

const LOGIN_ERROR = "You are logged out. The last login error was: all connection attempts failed";

// Architect ruling: an auth URL supersedes the last login error. The error must
// not sit on the hint line beside a working Log in button, but it must stay
// visible in the warnings list so a node that genuinely cannot reach control
// still explains itself.
test("a login URL supersedes the login error on the hint line", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: {
      state: "NeedsLogin",
      authURL: "https://login.tailscale.com/a/deadbeef",
      error: LOGIN_ERROR,
      warnings: [LOGIN_ERROR, "Cannot reach the coordination server"],
    },
  });

  if (ui.els.hint.textContent === LOGIN_ERROR) {
    throw new Error("the login error is still on the hint line beside the Log in button");
  }
  eq(ui.els.hint.textContent, "Log in to connect this browser profile to a tailnet.", "hint line");
  if (ui.els.login.hidden) throw new Error("the Log in button is hidden while an auth URL exists");
  // Still explained, one line down.
  eq(ui.warningTexts(), [LOGIN_ERROR, "Cannot reach the coordination server"], "warnings list");
  if (ui.els.warnings.hidden) throw new Error("the warnings list is hidden");
});

test("with no login URL the error is the hint and is not repeated below", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: {
      state: "NeedsLogin",
      authURL: "",
      error: LOGIN_ERROR,
      warnings: [LOGIN_ERROR, "Cannot reach the coordination server"],
    },
  });

  eq(ui.els.hint.textContent, LOGIN_ERROR, "hint line");
  eq(ui.warningTexts(), ["Cannot reach the coordination server"], "warnings list without the duplicate");
});

test("a running node shows its details and no warnings", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "tail4d5e6f.ts.net", hostname: "mac-tailtab-edge", selfIP: "100.64.0.9", proxyPort: 64378, warnings: [] },
  });
  eq(ui.els.state.textContent, "Connected", "state line");
  eq(ui.els.details.hidden, false, "details shown");
  eq(ui.els.tailnet.textContent, "tail4d5e6f.ts.net", "tailnet");
  eq(ui.els.warnings.hidden, true, "warnings hidden");
  eq(ui.els.login.hidden, true, "Log in hidden while running");
  eq(ui.els.disconnect.hidden, false, "Disconnect offered");
});

// The split-tunnel rule exists twice: here, and as allowTailnetHost in
// internal/proxy. testdata/tailnet-hosts.json is the table both are tested
// against, so the pair cannot drift into the failure R1 describes — the
// extension proxying a host the listener then answers 403 to.
test("rules.js matches the shared host table", () => {
  const table = JSON.parse(fs.readFileSync(path.resolve(SRC, "..", "testdata", "tailnet-hosts.json"), "utf8"));
  const rules = require(path.join(SRC, "rules.js"));
  if (!table.cases || table.cases.length < 55) {
    throw new Error("the shared host table has shrunk: " + (table.cases || []).length + " cases");
  }
  const wrong = [];
  for (const c of table.cases) {
    const got = rules.tailtabIsTailnetHost(c.host, c.suffix);
    if (got !== c.proxy) {
      wrong.push(JSON.stringify(c.host) + " with suffix " + JSON.stringify(c.suffix) +
        ": rules.js says " + got + ", the table says " + c.proxy + " (" + c.why + ")");
    }
  }
  if (wrong.length) throw new Error(wrong.length + " host(s) disagree:\n     " + wrong.join("\n     "));
});

// R1 in the browser: a tailnet on a custom domain has to reach the PAC too,
// and only once the node has reported that suffix.
test("the PAC routes a custom MagicDNS domain once the suffix is known", () => {
  const rules = require(path.join(SRC, "rules.js"));
  const pac = rules.tailtabBuildPac(64378, "my-tailnet.example.com");
  const decide = new Function("url", "host", pac + "\nreturn FindProxyForURL(url, host);");

  const proxied = decide("http://host.my-tailnet.example.com/", "host.my-tailnet.example.com");
  if (proxied === "DIRECT") throw new Error("the custom domain went DIRECT");
  if (proxied.indexOf("127.0.0.1:64378") === -1) {
    throw new Error("the custom domain was routed to " + proxied);
  }
  // No fallback: a tailnet name must fail rather than leak onto the internet.
  if (proxied.indexOf("DIRECT") !== -1) throw new Error("the PAC offers a DIRECT fallback: " + proxied);

  eq(decide("https://github.com/", "github.com"), "DIRECT", "the public internet");
  // Unknown suffix, same host: DIRECT, because nothing says it is ours.
  const blind = rules.tailtabBuildPac(64378, "");
  const decideBlind = new Function("url", "host", blind + "\nreturn FindProxyForURL(url, host);");
  eq(decideBlind("http://host.my-tailnet.example.com/", "host.my-tailnet.example.com"), "DIRECT",
    "a custom domain before the suffix is known");
});

// ------------------------------------------------------------- H2, the token

const TOKEN = "tok-AAAA1111";
const RUNNING_AUTH = Object.assign({}, RUNNING, { proxyToken: TOKEN });

// Chromium reaches the proxy over HTTP because it cannot authenticate SOCKS5,
// so the PAC has to say PROXY. A SOCKS5 PAC would produce a proxy the browser
// can never authenticate to.
test("the PAC sends tailnet hosts to PROXY, not SOCKS5", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:64378"], "the PAC target");

  const rules = rulesContext();
  const pac = rules.tailtabBuildPac(64378, "tail4d5e6f.ts.net");
  if (pac.indexOf("SOCKS5") !== -1) throw new Error("the PAC still offers SOCKS5");
  if (pac.indexOf("PROXY 127.0.0.1:64378; DIRECT") !== -1) {
    throw new Error("the PAC has a DIRECT fallback; a tailnet request must fail, not go out direct");
  }
});

// G11. onAuthRequired fires for every 401 and 407 in the browser. An unscoped
// answer would hand the tailnet credential to any site or proxy that asked.
test("the proxy challenge is answered only for our own listener", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  const ours = { isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } };
  eq(env.authRequired(ours), { authCredentials: { username: "tailtab", password: TOKEN } },
    "our own challenger");

  const foreign = [
    ["a site's 401", { isProxy: false, challenger: { host: "127.0.0.1", port: 64378 } }],
    ["another local proxy", { isProxy: true, challenger: { host: "127.0.0.1", port: 8080 } }],
    ["a remote proxy", { isProxy: true, challenger: { host: "proxy.example.com", port: 64378 } }],
    ["a spoofed host name", { isProxy: true, challenger: { host: "127.0.0.1.evil.com", port: 64378 } }],
    ["no challenger at all", { isProxy: true }],
    ["nothing at all", null],
  ];
  for (const [what, details] of foreign) {
    eq(env.authRequired(details), {}, "answer to " + what);
  }

  // Registered at the top level, for every URL, and asynchronously — the three
  // things Chromium requires of a proxy-auth provider.
  const reg = env.authListener();
  eq(reg.extra, ["asyncBlocking"], "extraInfoSpec");
  eq(reg.filter, { urls: ["<all_urls>"] }, "url filter");
});

test("a challenge with no token yet is answered promptly with nothing", async () => {
  const env = makeEnv();
  await flush();
  // Running, but the host sent no token: answer at once rather than leaving
  // the request hanging on a dialog (G13).
  env.status(RUNNING);
  await flush();
  eq(env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } }), {},
    "answer with no token");
});

test("a host restart rotates both the port and the token", async () => {
  const env = makeEnv();
  await flush();
  env.status({ state: "Running", proxyPort: 1111, tailnet: "tail4d5e6f.ts.net", proxyToken: "first-token" });
  await flush();
  eq(env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 1111 } }),
    { authCredentials: { username: "tailtab", password: "first-token" } }, "the first token");

  env.disconnect();
  await flush();
  env.runNextTimer(); // the reconnect
  env.status({ state: "Running", proxyPort: 2222, tailnet: "tail4d5e6f.ts.net", proxyToken: "second-token" });
  await flush();

  eq(env.log.set, ["PROXY 127.0.0.1:1111", "PROXY 127.0.0.1:2222"], "the PAC follows the new port");
  eq(env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 2222 } }),
    { authCredentials: { username: "tailtab", password: "second-token" } }, "the new token");
  // The old port is not ours any more.
  eq(env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 1111 } }), {},
    "a challenge from the dead port");
});

// FIX 1 (REVIEW.md). The token is written nowhere at all: a saved one is stale
// by construction, because the host dies with the worker that started it.
test("the token is never stored or pushed anywhere", async () => {
  const env = makeEnv();
  await flush();
  const port = env.openPopup();
  env.status(RUNNING_AUTH);
  await flush();

  const local = JSON.stringify(env.log.localSet);
  if (local.indexOf(TOKEN) !== -1) throw new Error("the token was written to storage.local: " + local);
  const session = JSON.stringify(env.log.sessionSet);
  if (session.indexOf(TOKEN) !== -1) throw new Error("the token was written to storage.session: " + session);
  const pushed = JSON.stringify(env.popupMessages);
  if (pushed.indexOf(TOKEN) !== -1) throw new Error("the token was pushed to the popup: " + pushed);
  if (port.name !== "popup") throw new Error("the popup port was not opened");
});

// A token left over in storage.session from an older build must not be picked
// up: the host that would honour it is long gone.
test("a token left in storage.session is not restored", async () => {
  const env = makeEnv({
    session: { proxyToken: "tok-STALE", status: { state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net" } },
  });
  await flush();
  eq(env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } }), {},
    "a challenge answered from storage");
});

// When the host dies the credential goes with it. Whatever takes the port next
// is not our proxy, and must not be handed the token.
test("the token is dropped when the host disconnects", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();
  const ours = { isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } };
  eq(env.authRequired(ours), { authCredentials: { username: "tailtab", password: TOKEN } },
    "while the host is up");

  env.disconnect();
  await flush();
  eq(env.authRequired(ours), {}, "a challenge from the old port after the host died");
  eq(vm.runInContext("proxyToken", env.ctx), "", "the token in memory");

  // And the replacement host's token is the one used from then on.
  env.runNextTimer(); // the reconnect
  env.status({ state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net", proxyToken: "tok-BBBB2222" });
  await flush();
  eq(env.authRequired(ours), { authCredentials: { username: "tailtab", password: "tok-BBBB2222" } },
    "after the reconnect");
});

// Zen authenticates SOCKS5 in-protocol, which Chromium cannot do at all.
test("Firefox sends the credential with the SOCKS proxy info", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  eq(env.resolve("http://wiki/"),
    { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true, username: "tailtab", password: TOKEN },
    "a tailnet host");
  eq(env.resolve("https://github.com/"), { type: "direct" }, "the public internet");

  // No PAC and no proxy settings are touched on this side.
  if (env.log.localSet.some((v) => JSON.stringify(v).indexOf(TOKEN) !== -1)) {
    throw new Error("the token was written to storage.local");
  }
});

test("Firefox still proxies a tailnet host before the token arrives", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status({ state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net" });
  await flush();
  // Without credentials the request fails at the proxy, which is the right
  // failure: sending it DIRECT would leak a tailnet name to the public DNS.
  eq(env.resolve("http://wiki/"),
    { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true }, "a tailnet host with no token");
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log("ok   " + t.name);
    } catch (e) {
      failed++;
      console.log("FAIL " + t.name + "\n     " + e.message);
    }
  }
  console.log(failed === 0 ? "all " + tests.length + " extension tests passed" : failed + " of " + tests.length + " failed");
  process.exit(failed ? 1 : 0);
})();
