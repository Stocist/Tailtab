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
// "SOCKS5 127.0.0.1:64378" rather than on generated JavaScript.
function pacTarget(value) {
  if (!value || !value.pacScript || typeof value.pacScript.data !== "string") {
    return JSON.stringify(value);
  }
  const m = value.pacScript.data.match(/SOCKS5 [\d.]+:\d+/);
  return m ? m[0] : "pac without a SOCKS5 target";
}

// makeEnv builds a stubbed Chromium and loads background.js into it.
function makeEnv(options) {
  const opts = options || {};
  const log = { set: [], clear: [], sent: [], timers: [], connects: 0 };
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
        set: async () => {},
      },
      session: { get: async () => ({}), set: async () => {} },
    },
    tabs: { create() {} },
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
    // runNextTimer fires the pending reconnect timer and returns its delay.
    runNextTimer() {
      const t = log.timers.shift();
      if (!t) throw new Error("no timer was scheduled");
      t.fn();
      return t.ms;
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
  eq(env.log.set, ["SOCKS5 127.0.0.1:64378"], "PAC installed on the first Running");

  env.popup("down");
  env.status(Object.assign({}, RUNNING, { state: "Stopped" }));
  await flush();
  eq(env.log.clear, ["regular"], "PAC handed back on Disconnect");

  env.popup("up");
  env.status(Object.assign({}, RUNNING, { state: "Starting" }));
  await flush();
  env.status(RUNNING); // same process: same port, same tailnet
  await flush();
  eq(env.log.set, ["SOCKS5 127.0.0.1:64378", "SOCKS5 127.0.0.1:64378"], "PAC reinstalled on Connect");
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
  eq(env.log.set, ["SOCKS5 127.0.0.1:1111", "SOCKS5 127.0.0.1:2222"], "PAC follows the new port");
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
    if (findProxy("http://" + host + "/", host) !== "SOCKS5 127.0.0.1:51234") {
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
    if (findProxy("http://" + host + "/", host) !== "SOCKS5 127.0.0.1:51234") throw new Error("PAC sends " + JSON.stringify(host) + " direct, want proxied");
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
