// Tests for extension/background.js, run under node with a stubbed Chromium
// MV3 API and fake timers:
//
//   node scripts/test-extension.js
//
// The background script has no browser to run in here, so the harness supplies
// chrome.runtime, chrome.proxy.settings and chrome.storage, then drives real
// status events through it and inspects what it did to the proxy settings.

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const root = path.resolve(__dirname, "..");
const rulesSrc = fs.readFileSync(path.join(root, "extension", "rules.js"), "utf8");
const backgroundSrc = fs.readFileSync(path.join(root, "extension", "background.js"), "utf8");

// pacTarget pulls the proxy the PAC script would return, so a test can assert
// on "SOCKS5 127.0.0.1:64378" rather than on a blob of generated JavaScript.
function pacTarget(value) {
  if (!value || !value.pacScript || typeof value.pacScript.data !== "string") {
    return JSON.stringify(value);
  }
  const m = value.pacScript.data.match(/SOCKS5 [\d.]+:\d+/);
  return m ? m[0] : "pac without a SOCKS5 target";
}

// newEnv builds a stubbed Chromium and loads the background script into it.
function newEnv(options) {
  const opts = options || {};
  const calls = { set: [], clear: [], sent: [], connectNative: 0, timers: [] };
  const ports = [];
  const local = {};
  const session = {};

  function newPort() {
    const port = {
      onMessage: { fns: [], addListener(f) { this.fns.push(f); } },
      onDisconnect: { fns: [], addListener(f) { this.fns.push(f); } },
      postMessage(msg) { calls.sent.push(msg.cmd); },
      disconnect() {},
    };
    ports.push(port);
    return port;
  }

  const chrome = {
    runtime: {
      lastError: opts.lastError,
      connectNative() {
        calls.connectNative++;
        return newPort();
      },
      onStartup: { addListener() {} },
      onInstalled: { addListener() {} },
      onConnect: { addListener() {} },
    },
    proxy: {
      settings: {
        get(_details, cb) { cb({ levelOfControl: opts.levelOfControl || "controllable_by_this_extension" }); },
        set(details, cb) { calls.set.push(pacTarget(details.value)); if (cb) cb(); },
        clear(details, cb) { calls.clear.push(details.scope); if (cb) cb(); },
      },
    },
    storage: {
      local: {
        get(key) { return Promise.resolve(key in local ? { [key]: local[key] } : {}); },
        set(obj) { Object.assign(local, obj); return Promise.resolve(); },
      },
      session: {
        get(key) { return Promise.resolve(key in session ? { [key]: session[key] } : {}); },
        set(obj) { Object.assign(session, obj); return Promise.resolve(); },
      },
    },
  };

  const sandbox = {
    chrome: chrome,
    console: { log() {}, warn() {}, error() {} },
    crypto: { randomUUID: () => "0f8fad5b-d9cb-469f-a165-70867728950e" },
    URL: URL,
    Promise: Promise,
    Object: Object,
    Math: Math,
    // Fake timers: scheduleReconnect's delays are the thing under test, so they
    // are recorded rather than waited on.
    setTimeout(fn, ms) {
      calls.timers.push({ fn: fn, ms: ms });
      return calls.timers.length;
    },
    clearTimeout() {},
  };
  const ctx = vm.createContext(sandbox);
  vm.runInContext(rulesSrc, ctx, { filename: "rules.js" });
  vm.runInContext(backgroundSrc, ctx, { filename: "background.js" });

  return {
    calls: calls,
    ports: ports,
    // status pushes one status event from the host.
    status(fields) {
      const port = ports[ports.length - 1];
      const msg = Object.assign({ event: "status", state: "", proxyPort: 0, tailnet: "" }, fields);
      for (const fn of port.onMessage.fns) fn(msg);
    },
    // disconnect drops the current native port, as a dead host would.
    disconnect() {
      const port = ports[ports.length - 1];
      for (const fn of port.onDisconnect.fns) fn();
    },
    // runNextTimer fires the pending reconnect timer.
    runNextTimer() {
      const t = calls.timers.shift();
      if (!t) throw new Error("no timer was scheduled");
      t.fn();
      return t.ms;
    },
  };
}

// settle lets the background script's promise chains run to completion.
async function settle() {
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));
}

const RUNNING = { state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net", hostname: "mac-tailtab-edge", selfIP: "100.101.102.103" };

const tests = [];
function test(name, fn) { tests.push({ name: name, fn: fn }); }

function eq(got, want, what) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(what + ": got " + g + ", want " + w);
}

// B1. The regression the verifier found: Disconnect then Connect against one
// host process leaves the port and the tailnet unchanged, so a diff-based guard
// hands the PAC back and never reinstalls it.
test("the PAC is reinstalled after a Disconnect and Connect on one host", async (t) => {
  const env = newEnv();
  await settle();

  env.status(RUNNING);
  await settle();
  eq(env.calls.set, ["SOCKS5 127.0.0.1:64378"], "PAC installed on the first Running");

  env.status({ state: "Stopped", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net" });
  await settle();
  eq(env.calls.clear, ["regular"], "PAC handed back on Disconnect");

  // Same host process, so the same port and the same tailnet come back.
  env.status(RUNNING);
  await settle();
  eq(env.calls.set, ["SOCKS5 127.0.0.1:64378", "SOCKS5 127.0.0.1:64378"], "PAC reinstalled on Connect");
  eq(env.calls.sent[0], "init", "init was sent to the host");
});

test("a host restart on a new port reinstalls the PAC", async () => {
  const env = newEnv();
  await settle();

  env.status({ state: "Running", proxyPort: 1111, tailnet: "tail4d5e6f.ts.net" });
  await settle();
  env.disconnect();
  await settle();
  eq(env.calls.clear, ["regular"], "PAC handed back when the host dies");

  env.runNextTimer(); // reconnect
  env.status({ state: "Running", proxyPort: 2222, tailnet: "tail4d5e6f.ts.net" });
  await settle();
  eq(env.calls.set, ["SOCKS5 127.0.0.1:1111", "SOCKS5 127.0.0.1:2222"], "PAC follows the new port");
});

test("a tailnet rename while Running rewrites the PAC", async () => {
  const env = newEnv();
  await settle();
  env.status(RUNNING);
  await settle();
  env.status(Object.assign({}, RUNNING, { tailnet: "tail1a2b3c.ts.net" }));
  await settle();
  eq(env.calls.set.length, 2, "PAC rewritten for the new tailnet suffix");
});

test("proxy settings owned by policy are left alone", async () => {
  const env = newEnv({ levelOfControl: "controlled_by_policy" });
  await settle();
  env.status(RUNNING);
  await settle();
  eq(env.calls.set, [], "no attempt to take a setting owned by policy");
});

// F1. The reset used to run in connect(), which always beat the doubling in
// scheduleReconnect because connectNative fails asynchronously.
test("the reconnect backoff doubles to the 30s cap", async () => {
  const env = newEnv();
  await settle();

  const delays = [];
  for (let i = 0; i < 6; i++) {
    env.disconnect();
    await settle();
    delays.push(env.runNextTimer());
  }
  eq(delays, [1000, 2000, 4000, 8000, 16000, 30000], "backoff delays");
});

test("the backoff resets once the host answers", async () => {
  const env = newEnv();
  await settle();

  env.disconnect();
  await settle();
  eq(env.runNextTimer(), 1000, "first delay");
  env.disconnect();
  await settle();
  eq(env.runNextTimer(), 2000, "second delay");

  // A live host answers, so the next failure starts from the bottom again.
  env.status(RUNNING);
  await settle();
  env.disconnect();
  await settle();
  eq(env.runNextTimer(), 1000, "delay after a successful exchange");
});

// The split-tunnel rules, checked through the predicate and through the PAC
// script generated from it, which must agree on every host.
test("the split-tunnel rules and the generated PAC agree", () => {
  const ctx = vm.createContext({ console: console });
  vm.runInContext(rulesSrc, ctx, { filename: "rules.js" });
  const isTailnet = ctx.tailtabIsTailnetHost;
  const pac = ctx.tailtabBuildPac(51234, "tail4d5e6f.ts.net");
  const findProxy = vm.runInContext(pac + "\nFindProxyForURL", vm.createContext({}));

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

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn(t);
      console.log("ok   " + t.name);
    } catch (e) {
      failed++;
      console.log("FAIL " + t.name + "\n     " + e.message);
    }
  }
  console.log(failed === 0 ? "all " + tests.length + " extension tests passed" : failed + " of " + tests.length + " failed");
  process.exit(failed ? 1 : 0);
})();
