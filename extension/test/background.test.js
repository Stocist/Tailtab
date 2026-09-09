"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const SRC = path.resolve(__dirname, "..");
const read = (f) => fs.readFileSync(path.join(SRC, f), "utf8");

const flush = () => new Promise((r) => setImmediate(() => setImmediate(() => setImmediate(r))));

function pacTarget(value) {
  if (!value || !value.pacScript || typeof value.pacScript.data !== "string") {
    return JSON.stringify(value);
  }
  const m = value.pacScript.data.match(/(?:PROXY|SOCKS5) [\d.]+:\d+/);
  return m ? m[0] : "pac without a proxy target";
}

function makeEnv(options) {
  const opts = options || {};
  const log = { set: [], clear: [], sent: [], sentFull: [], timers: [], connects: 0, localSet: [], sessionSet: [], lastPac: "", alarms: [], icons: [] };
  const mkEvent = () => {
    const fns = [];
    return { addListener: (f) => fns.push(f), _fire: (...a) => fns.forEach((f) => f(...a)) };
  };

  let nativePort = null;
  const popupMessages = [];
  const chrome = {
    runtime: {
      id: "tailtab-test",
      getURL: (p) => "chrome-extension://tailtab-test/" + p,
      lastError: null,
      connectNative() {
        log.connects++;
        nativePort = {
          onMessage: mkEvent(),
          onDisconnect: mkEvent(),
          postMessage: (m) => { log.sent.push(m.cmd); log.sentFull.push(m); },
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
        get: (_details, cb) => cb({ levelOfControl: log.levelOfControl || opts.levelOfControl || "controllable_by_this_extension" }),
        set: (details, cb) => {
          log.set.push(pacTarget(details.value));
          log.lastValue = details.value;
          log.lastPac = (details.value && details.value.pacScript && details.value.pacScript.data) || "";
          if (!cb) return;
          // Chromium exposes rejection only during this callback.
          if (opts.setError) {
            chrome.runtime.lastError = { message: opts.setError };
            cb();
            chrome.runtime.lastError = null;
            return;
          }
          cb();
        },
        clear: (details, cb) => { log.clear.push(details.scope); if (cb) cb(); },
      },
    },
    storage: {
      local: {
        get: async () => Object.assign({ profileID: "0f8fad5b-d9cb-469f-a165-70867728950e" }, opts.local || {}),
        set: async (v) => { log.localSet.push(v); },
      },
      session: {
        get: async () => (opts.session || {}),
        set: async (v) => { log.sessionSet.push(v); },
      },
    },
    tabs: { create() {} },
    action: { setIcon: (d) => { log.icons.push(d.path && d.path[16]); } },
    alarms: { create: (name, info) => { log.alarms.push({ name: name, info: info }); }, onAlarm: mkEvent() },
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
    popupMessages: popupMessages,
    // sender defaults to the popup page; pass null or another sender for a stranger.
    openPopup(sender) {
      const port = {
        name: "popup",
        sender: sender === undefined ? { id: "tailtab-test", url: "chrome-extension://tailtab-test/popup.html" } : sender,
        onMessage: mkEvent(),
        onDisconnect: mkEvent(),
        postMessage: (m) => popupMessages.push(m),
      };
      chrome.runtime.onConnect._fire(port);
      return port;
    },
    status(fields) {
      nativePort.onMessage._fire(Object.assign(
        { event: "status", state: "", proxyPort: 0, tailnet: "" },
        fields
      ));
    },
    disconnect() { nativePort.onDisconnect._fire(); },
    popup(cmd) {
      const port = this.openPopup();
      port.onMessage._fire(typeof cmd === "string" ? { cmd: cmd } : cmd);
    },
    authRequired(details) {
      return this.authChallenge(details).answer;
    },
    authChallenge(details) {
      if (!log.authListener) throw new Error("no onAuthRequired listener was registered");
      const handle = { settled: false, value: undefined };
      handle.answer = new Promise((resolve) => {
        log.authListener.fn(details, (a) => {
          handle.settled = true;
          handle.value = a;
          resolve(a);
        });
      });
      return handle;
    },
    runAllTimers() {
      const pending = log.timers.splice(0, log.timers.length);
      for (const t of pending) t.fn();
      return pending.length;
    },
    authListener: () => log.authListener,
    fireAlarm(name) { chrome.alarms.onAlarm._fire({ name: name }); },
    setLevelOfControl(v) { log.levelOfControl = v; },
    runNextTimer() {
      const t = log.timers.shift();
      if (!t) throw new Error("no timer was scheduled");
      t.fn();
      return t.ms;
    },
  };
}

function makeFirefoxEnv() {
  const log = { onRequest: null, sent: [], sentFull: [], timers: [], sessionSet: [], localSet: [] };
  const mkEvent = () => {
    const fns = [];
    return { addListener: (f) => fns.push(f), _fire: (...a) => fns.forEach((f) => f(...a)) };
  };
  let nativePort = null;
  const browserApi = {
    runtime: {
      id: "tailtab@stocist.dev",
      getURL: (p) => "moz-extension://tailtab-test/" + p,
      lastError: null,
      connectNative() {
        nativePort = {
          onMessage: mkEvent(),
          onDisconnect: mkEvent(),
          postMessage: (m) => { log.sent.push(m.cmd); log.sentFull.push(m); },
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
    action: { setIcon() {} },
    alarms: { create() {}, onAlarm: mkEvent() },
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

// Reconnect can reuse both port and tailnet; routing must follow state transitions.
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
  env.status(RUNNING);
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
  eq(env.log.clear, [], "the setting is not handed back on a crash");
  eq(env.log.set[env.log.set.length - 1], "PROXY 0.0.0.1:1", "it is parked on a dead port instead, so nothing leaks");

  env.runNextTimer();
  env.status({ state: "Running", proxyPort: 2222, tailnet: "tail4d5e6f.ts.net" });
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:1111", "PROXY 0.0.0.1:1", "PROXY 127.0.0.1:2222"], "PAC follows the new port, parked in between");
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

// connectNative reports failure later on onDisconnect, so eager reset defeats backoff.
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

function rulesContext() {
  const ctx = vm.createContext({});
  vm.runInContext(read("rules.js"), ctx, { filename: "rules.js" });
  return ctx;
}

// Chromium's <local> bypass includes MagicDNS short names, so exclude loopback
// explicitly instead of using that token.
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

// Login URLs can arrive after the popup opens and must update that live port.
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

function openPopupUI() {
  const els = {};
  const makeEl = () => {
    const el = { hidden: false, disabled: false, value: "", children: [], listeners: {} };
    let text = "";
    Object.defineProperty(el, "textContent", {
      get: () => text,
      set: (v) => { text = v; el.children.length = 0; },
    });
    el.appendChild = (child) => el.children.push(child);
    el.addEventListener = (name, fn) => { el.listeners[name] = fn; };
    return el;
  };
  for (const id of ["state", "hint", "warnings", "details", "tailnet", "hostname", "selfip", "port", "login", "connect", "disconnect", "logout", "warning", "exitrow", "exitnode", "account", "accountname", "accounttailnet", "accountmenu", "avatar", "toggle", "search", "machines", "machinesec", "copied"]) {
    els[id] = makeEl();
  }

  let backgroundPort = null;
  const sent = [];
  const opened = [];
  const sandbox = {
    console: { log() {}, warn() {}, error() {} },
    setTimeout: () => 1,
    clearTimeout: () => {},
    URL: URL,
    window: { close() {} },
    document: {
      getElementById: (id) => els[id] || (els[id] = makeEl()),
      createElement: () => makeEl(),
    },
    chrome: {
      runtime: {
        connect: () => {
          backgroundPort = {
            listeners: [],
            onMessage: { addListener: (f) => backgroundPort.listeners.push(f) },
            postMessage: (m) => sent.push(m),
          };
          return backgroundPort;
        },
      },
      tabs: { create: (o) => { opened.push(o.url); } },
    },
  };
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  vm.runInContext(read("rules.js"), ctx, { filename: "rules.js" });
  vm.runInContext(read("popup.js"), ctx, { filename: "popup.js" });

  return {
    els: els,
    push(payload) {
      if (payload && payload.build === undefined) payload = Object.assign({ build: "__TAILTAB_BUILD__" }, payload);
      for (const fn of backgroundPort.listeners) fn(payload);
    },
    warningTexts: () => els.warnings.children.map((c) => c.textContent),
    sent: sent,
    opened: opened,
    exitOptions: () => els.exitnode.children.map((c) => ({
      value: c.value, label: c.textContent, disabled: c.disabled,
    })),
    // Located nodes sit in optgroups; only those carry a label.
    exitGroups: () => els.exitnode.children.filter((c) => c.label !== undefined).map((g) => ({
      country: g.label,
      options: g.children.map((c) => ({ value: c.value, label: c.textContent })),
    })),
    chooseExitNode(id) {
      els.exitnode.value = id;
      if (!els.exitnode.listeners.change) throw new Error("the picker has no change listener");
      els.exitnode.listeners.change({ target: els.exitnode });
    },
    accountItems: () => els.accountmenu.children.map((c) => ({
      label: c.children.length ? c.children[0].children[0].textContent : c.textContent,
      className: c.className,
      id: c.accountID,
    })),
    chooseAccount(id) {
      const item = els.accountmenu.children.find((c) => c.accountID === id);
      if (!item) throw new Error("no menu entry for " + id);
      item.listeners.click();
    },
    clickAddAccount() {
      const add = els.accountmenu.children.find((c) => c.className === "add");
      if (!add) throw new Error("no Add account entry");
      add.listeners.click();
    },
    clickToggle() {
      els.toggle.listeners.click();
    },
    search(q) {
      els.search.value = q;
      els.search.listeners.input();
    },
    machineRows: () => els.machines.children.map((li) => ({
      name: li.children.length ? li.children[0].textContent : li.textContent,
      ip: li.children.length > 1 ? li.children[1].textContent : "",
      className: li.children.length ? li.children[0].className : li.className,
    })),
  };
}

const LOGIN_ERROR = "You are logged out. The last login error was: all connection attempts failed";

// A working auth URL supersedes its prior login error without hiding other warnings.
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
  eq(ui.warningTexts(), ["Cannot reach the coordination server"], "warnings list");
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
  eq(ui.els.toggle.className, "on", "the header toggle is on while Running");
});

// Shared cases keep extension routing in parser parity with allowTailnetHost.
test("rules.js matches the shared host table", () => {
  const table = JSON.parse(fs.readFileSync(path.resolve(SRC, "..", "testdata", "tailnet-hosts.json"), "utf8"));
  const rules = require(path.join(SRC, "rules.js"));
  if (!table.cases || table.cases.length < 55) {
    throw new Error("the shared host table has shrunk: " + (table.cases || []).length + " cases");
  }
  const wrong = [];
  for (const c of table.cases) {
    const got = rules.tailtabIsTailnetHost(c.host, c.suffix, c.routes || []);
    if (got !== c.proxy) {
      wrong.push(JSON.stringify(c.host) + " with suffix " + JSON.stringify(c.suffix) +
        ": rules.js says " + got + ", the table says " + c.proxy + " (" + c.why + ")");
    }
  }
  if (wrong.length) throw new Error(wrong.length + " host(s) disagree:\n     " + wrong.join("\n     "));
});

// A custom MagicDNS suffix is trusted only after the host reports it.
test("the PAC routes a custom MagicDNS domain once the suffix is known", () => {
  const rules = require(path.join(SRC, "rules.js"));
  const pac = rules.tailtabBuildPac(64378, "my-tailnet.example.com");
  const decide = new Function("url", "host", pac + "\nreturn FindProxyForURL(url, host);");

  const proxied = decide("http://host.my-tailnet.example.com/", "host.my-tailnet.example.com");
  if (proxied === "DIRECT") throw new Error("the custom domain went DIRECT");
  if (proxied.indexOf("127.0.0.1:64378") === -1) {
    throw new Error("the custom domain was routed to " + proxied);
  }
  // Tailnet failures must not fall back to public DNS.
  if (proxied.indexOf("DIRECT") !== -1) throw new Error("the PAC offers a DIRECT fallback: " + proxied);

  eq(decide("https://github.com/", "github.com"), "DIRECT", "the public internet");
  const blind = rules.tailtabBuildPac(64378, "");
  const decideBlind = new Function("url", "host", blind + "\nreturn FindProxyForURL(url, host);");
  eq(decideBlind("http://host.my-tailnet.example.com/", "host.my-tailnet.example.com"), "DIRECT",
    "a custom domain before the suffix is known");
});

const TOKEN = "tok-AAAA1111";
const RUNNING_AUTH = Object.assign({}, RUNNING, { proxyToken: TOKEN });

// Chromium cannot authenticate SOCKS5, so its PAC must use HTTP PROXY.
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

// onAuthRequired sees every 401 and 407; never expose the token outside our port.
test("the proxy challenge is answered only for our own listener", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  const ours = { isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } };
  eq(await env.authRequired(ours), { authCredentials: { username: "tailtab", password: TOKEN } },
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
    eq(await env.authRequired(details), {}, "answer to " + what);
  }

  // Chromium proxy authentication requires top-level async registration.
  const reg = env.authListener();
  eq(reg.extra, ["asyncBlocking"], "extraInfoSpec");
  eq(reg.filter, { urls: ["<all_urls>"] }, "url filter");
});

// A proxy challenge can wake an empty worker before the host returns its token.
test("a challenge that arrives before the host has answered waits for it", async () => {
  const env = makeEnv();
  await flush();
  const challenge = env.authChallenge({ isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } });
  await flush();
  if (challenge.settled) {
    throw new Error("the challenge was declined before the host had a chance to answer: " + JSON.stringify(challenge.value));
  }

  env.status(RUNNING_AUTH);
  await flush();
  eq(await challenge.answer, { authCredentials: { username: "tailtab", password: TOKEN } },
    "the answer once the host reported its port and token");
});

test("a challenge the host never answers is declined at the deadline", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  const challenge = env.authChallenge({ isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } });
  await flush();
  if (challenge.settled) throw new Error("declined without waiting");
  env.runAllTimers();
  eq(await challenge.answer, {}, "the answer when the host never came");
});

// Do not stall unrelated browser authentication while waiting for our host.
test("a challenge from another proxy is declined without waiting", async () => {
  const env = makeEnv();
  await flush();
  const foreign = env.authChallenge({ isProxy: true, challenger: { host: "10.0.0.9", port: 3128 } });
  await flush();
  if (!foreign.settled) throw new Error("a foreign proxy challenge was parked waiting for our host");
  eq(foreign.value, {}, "the answer");

  const site = env.authChallenge({ isProxy: false, challenger: { host: "127.0.0.1", port: 64378 } });
  await flush();
  if (!site.settled) throw new Error("a site's 401 was parked waiting for our host");
  eq(site.value, {}, "the answer");
});

// Chromium can retain a PAC for a dead host across extension reloads.
test("a challenge from a port we no longer use reinstalls the proxy settings", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();
  const before = env.log.set.length;

  eq(await env.authRequired({ isProxy: true, realm: "tailtab", challenger: { host: "127.0.0.1", port: 55555 } }), { cancel: true },
    "a challenge from a stale tailtab host is cancelled, so no password dialog appears");
  await flush();
  eq(env.log.set.length, before + 1, "the proxy settings were reinstalled");
  eq(env.log.set[env.log.set.length - 1], "PROXY 127.0.0.1:64378", "and they point at the live proxy");

  // Decline rather than cancel another loopback proxy's authentication.
  eq(await env.authRequired({ isProxy: true, realm: "squid", challenger: { host: "127.0.0.1", port: 3128 } }), {},
    "a challenge from another local proxy is declined, not cancelled");
});

test("a host restart rotates both the port and the token", async () => {
  const env = makeEnv();
  await flush();
  env.status({ state: "Running", proxyPort: 1111, tailnet: "tail4d5e6f.ts.net", proxyToken: "first-token" });
  await flush();
  eq(await env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 1111 } }),
    { authCredentials: { username: "tailtab", password: "first-token" } }, "the first token");

  env.disconnect();
  await flush();
  env.runNextTimer();
  env.status({ state: "Running", proxyPort: 2222, tailnet: "tail4d5e6f.ts.net", proxyToken: "second-token" });
  await flush();

  eq(env.log.set, ["PROXY 127.0.0.1:1111", "PROXY 0.0.0.1:1", "PROXY 127.0.0.1:2222"], "the PAC follows the new port, parked in between");
  eq(await env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 2222 } }),
    { authCredentials: { username: "tailtab", password: "second-token" } }, "the new token");
  eq(await env.authRequired({ isProxy: true, challenger: { host: "127.0.0.1", port: 1111 } }), {},
    "a challenge from the dead port");
});

// The per-process token must never leave worker memory.
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

// A persisted token is stale because its host process is gone.
test("a token left in storage.session is not restored", async () => {
  const env = makeEnv({
    session: { proxyToken: "tok-STALE", status: { state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net" } },
  });
  await flush();
  const challenge = env.authChallenge({ isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } });
  await flush();
  env.runAllTimers();
  eq(await challenge.answer, {}, "a challenge answered from storage");
});

// Drop credentials with their host so a port successor cannot receive them.
test("the token is dropped when the host disconnects", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();
  const ours = { isProxy: true, challenger: { host: "127.0.0.1", port: 64378 } };
  eq(await env.authRequired(ours), { authCredentials: { username: "tailtab", password: TOKEN } },
    "while the host is up");

  env.disconnect();
  await flush();
  eq(vm.runInContext("proxyToken", env.ctx), "", "the token in memory");
  // A concurrent challenge must wait for the replacement host's token.
  const challenge = env.authChallenge(ours);
  await flush();
  if (challenge.settled) throw new Error("declined instead of waiting for the replacement host");
  env.status({ state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net", proxyToken: "tok-BBBB2222" });
  await flush();
  eq(await challenge.answer, { authCredentials: { username: "tailtab", password: "tok-BBBB2222" } },
    "the replacement host's token");
});

// Gecko supplies SOCKS5 credentials in proxy.onRequest; Chromium cannot.
test("Firefox sends the credential with the SOCKS proxy info", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  eq(env.resolve("http://wiki/"),
    { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true, username: "tailtab", password: TOKEN },
    "a tailnet host");
  eq(env.resolve("https://github.com/"), { type: "direct" }, "the public internet");

  if (env.log.localSet.some((v) => JSON.stringify(v).indexOf(TOKEN) !== -1)) {
    throw new Error("the token was written to storage.local");
  }
});

test("Firefox still proxies a tailnet host before the token arrives", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status({ state: "Running", proxyPort: 64378, tailnet: "tail4d5e6f.ts.net" });
  await flush();
  // Without credentials, fail at the proxy rather than leak to public DNS.
  eq(env.resolve("http://wiki/"),
    { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true }, "a tailnet host with no token");
});

// Chromium rejects the whole PAC if stringified source includes non-ASCII.
test("the generated PAC is pure ASCII", () => {
  const rules = rulesContext();
  const pac = rules.tailtabBuildPac(64378, "tail4d5e6f.ts.net");
  const bad = [];
  for (let i = 0; i < pac.length; i++) {
    if (pac.charCodeAt(i) > 127) bad.push(JSON.stringify(pac[i]) + " at " + i);
  }
  if (bad.length) throw new Error("the PAC has non-ASCII characters Chromium would reject: " + bad.join(", "));

  // Guard code that may later move into a stringified function too.
  const src = read("rules.js");
  for (let i = 0; i < src.length; i++) {
    if (src.charCodeAt(i) > 127) {
      const line = src.slice(0, i).split("\n").length;
      throw new Error("rules.js line " + line + " has a non-ASCII character: " + JSON.stringify(src[i]));
    }
  }
});

test("a non-ASCII character in the embedded function is refused, not shipped", () => {
  const broken = read("rules.js").replace("// Never proxy the loopback", "// Never proxy the loopback \u2014 it is ours");
  const ctx = vm.createContext({});
  vm.runInContext(broken, ctx, { filename: "rules-broken.js" });
  let threw = null;
  try {
    ctx.tailtabBuildPac(64378, "tail4d5e6f.ts.net");
  } catch (e) {
    threw = e;
  }
  if (!threw) throw new Error("a PAC Chromium would reject was built without complaint");
  if (String(threw.message).indexOf("non-ASCII") === -1) {
    throw new Error("the error does not say what is wrong: " + threw.message);
  }
});

test("a PAC that cannot be built is reported instead of installed", async () => {
  const env = makeEnv();
  await flush();
  vm.runInContext("tailtabBuildPac = function () { throw new Error('non-ASCII character'); };", env.ctx);
  env.status(RUNNING_AUTH);
  await flush();

  eq(env.log.set, [], "nothing was installed");
  const problem = vm.runInContext("proxyProblem", env.ctx);
  if (!problem || problem.indexOf("not routed") === -1) {
    throw new Error("the popup was not told routing is broken: " + JSON.stringify(problem));
  }
});

// Proxy rejection appears only as callback-scoped runtime.lastError.
test("a rejected proxy configuration is reported, not swallowed", async () => {
  const env = makeEnv({ setError: "\'pacScript.data\' supports only ASCII code (encode URLs in Punycode format)." });
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  const problem = vm.runInContext("proxyProblem", env.ctx);
  if (!problem) throw new Error("a rejected proxy configuration left proxyProblem empty");
  if (problem.indexOf("not routed") === -1 || problem.indexOf("ASCII") === -1) {
    throw new Error("the reason was lost: " + JSON.stringify(problem));
  }
  const port = env.openPopup();
  const last = env.popupMessages[env.popupMessages.length - 1];
  if (!last || last.proxyProblem !== problem) {
    throw new Error("the popup was not given the problem: " + JSON.stringify(last));
  }
  if (port.name !== "popup") throw new Error("the popup port was not opened");
});

test("a proxy configuration that takes clears an earlier problem", async () => {
  const env = makeEnv({ levelOfControl: "controlled_by_policy" });
  await flush();
  env.status(RUNNING_AUTH);
  await flush();
  if (!vm.runInContext("proxyProblem", env.ctx)) throw new Error("the policy problem was not recorded");

  vm.runInContext("proxyProblem", env.ctx);
  env.setLevelOfControl("controllable_by_this_extension");
  env.status(Object.assign({}, RUNNING_AUTH, { proxyPort: 64379 }));
  await flush();
  eq(vm.runInContext("proxyProblem", env.ctx), "", "proxyProblem after a successful write");
  eq(env.log.set, ["PROXY 127.0.0.1:64379"], "the PAC that was installed");
});

// Failed proxy setup must not report routing while names can leak to DNS.
test("the popup does not claim to be routing when the proxy did not take", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    proxyProblem: "The browser rejected tailtab's proxy configuration, so tailnet traffic is not routed: 'pacScript.data' supports only ASCII code (encode URLs in Punycode format).",
    status: { state: "Running", tailnet: "tail4d5e6f.ts.net", hostname: "mac-tailtab-edge", selfIP: "100.64.0.9", proxyPort: 64378, warnings: [] },
  });
  eq(ui.els.state.textContent, "Connected, not routing", "state line");
  eq(ui.els.warning.hidden, false, "the warning is shown");
  if (ui.els.warning.textContent.indexOf("not routed") === -1) {
    throw new Error("the warning does not say routing is broken: " + ui.els.warning.textContent);
  }
});

const EXIT_NODES = [
  { id: "nodeid-attic", name: "attic", online: false, os: "linux" },
  { id: "nodeid-server", name: "server", online: true, os: "linux" },
];
const EXIT_RUNNING = Object.assign({}, RUNNING_AUTH, {
  exitNodes: EXIT_NODES,
  exitNode: "nodeid-server",
  exitNodeActive: true,
});

test("rules.js matches the shared exit-mode table", () => {
  const table = JSON.parse(fs.readFileSync(path.resolve(SRC, "..", "testdata", "exit-mode-hosts.json"), "utf8"));
  const rules = require(path.join(SRC, "rules.js"));
  if (!table.cases || table.cases.length < 30) {
    throw new Error("the exit-mode table has shrunk: " + (table.cases || []).length + " cases");
  }
  const wrong = [];
  for (const c of table.cases) {
    const got = rules.tailtabExitModeProxies(c.host, c.routes || []);
    if (got !== c.proxy) {
      wrong.push(JSON.stringify(c.host) + ": rules.js says " + got + ", the table says " + c.proxy + " (" + c.why + ")");
    }
  }
  if (wrong.length) throw new Error(wrong.length + " host(s) disagree:\n     " + wrong.join("\n     "));
});

test("the exit-mode PAC proxies the internet and leaves the LAN alone", () => {
  const rules = rulesContext();
  const pac = rules.tailtabBuildPac(64378, "tail4d5e6f.ts.net", true);
  const decide = new Function("url", "host", pac + "\nreturn FindProxyForURL(url, host);");

  for (const host of ["github.com", "ifconfig.me", "8.8.8.8", "wiki", "server.tail1a2b3c.ts.net"]) {
    eq(decide("https://" + host + "/", host), "PROXY 127.0.0.1:64378", host);
  }
  for (const host of ["localhost", "127.0.0.1", "192.168.1.1", "10.0.0.5", "169.254.1.1", "fe80::1"]) {
    eq(decide("http://" + host + "/", host), "DIRECT", host);
  }
  // Exit-mode proxy failures must not fall back to the local connection.
  if (pac.indexOf("DIRECT\";") === -1) throw new Error("the PAC lost its DIRECT branch entirely");
  if (pac.indexOf("PROXY 127.0.0.1:64378; DIRECT") !== -1) throw new Error("the PAC offers a DIRECT fallback");
  for (let i = 0; i < pac.length; i++) {
    if (pac.charCodeAt(i) > 127) throw new Error("the exit-mode PAC is not ASCII");
  }
});

test("choosing an exit node switches the PAC and clearing it switches back", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING_AUTH);
  await flush();

  const pacRule = () => {
    const data = env.log.lastPac;
    return data.indexOf("tailtabExitModeProxies") !== -1 ? "exit" : "tailnet";
  };
  eq(pacRule(), "tailnet", "the rule before an exit node is chosen");

  env.status(EXIT_RUNNING);
  await flush();
  eq(pacRule(), "exit", "the rule once an exit node is selected");
  eq(env.log.set.length, 2, "the PAC was rewritten");

  env.status(Object.assign({}, EXIT_RUNNING, { exitNode: "", exitNodeActive: false }));
  await flush();
  eq(pacRule(), "tailnet", "the rule after clearing the exit node");
});

// An offline exit node must block browsing instead of leaking it locally.
test("an exit node going offline keeps the browser in exit mode", async () => {
  const env = makeEnv();
  await flush();
  env.status(EXIT_RUNNING);
  await flush();
  const before = env.log.set.length;

  env.status(Object.assign({}, EXIT_RUNNING, { exitNodeActive: false }));
  await flush();
  eq(vm.runInContext("exitMode()", env.ctx), true, "still in exit mode with the node offline");
  eq(env.log.lastPac.indexOf("tailtabExitModeProxies") !== -1, true, "the installed PAC is still the exit-mode one");
  eq(env.log.set.length, before, "the PAC was needlessly rewritten");

  env.status(Object.assign({}, EXIT_RUNNING, { exitNodeActive: false, proxyPort: 9999 }));
  await flush();
  if (env.log.lastPac.indexOf("tailtabExitModeProxies") === -1) {
    throw new Error("the browser fell back to the split tunnel while an exit node was still selected");
  }
});

test("Firefox keeps sending everything to the proxy when the exit node is offline", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status(Object.assign({}, EXIT_RUNNING, { exitNodeActive: false }));
  await flush();
  const via = { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true, username: "tailtab", password: TOKEN };
  eq(env.resolve("https://github.com/"), via, "the public internet with the exit node offline");
  eq(env.resolve("http://192.168.1.1/"), { type: "direct" }, "the LAN is still direct");
});

test("a host restart re-derives exit mode from the new status", async () => {
  const env = makeEnv();
  await flush();
  env.status(Object.assign({}, EXIT_RUNNING, { proxyPort: 1111 }));
  await flush();
  if (env.log.lastPac.indexOf("tailtabExitModeProxies") === -1) throw new Error("not in exit mode to begin with");

  env.disconnect();
  await flush();
  eq(env.log.set[env.log.set.length - 1], "PROXY 0.0.0.1:1", "the PAC is parked when the host dies");

  env.runNextTimer();
  env.status(Object.assign({}, EXIT_RUNNING, { proxyPort: 2222, proxyToken: "tok-BBBB2222" }));
  await flush();
  eq(env.log.set, ["PROXY 127.0.0.1:1111", "PROXY 0.0.0.1:1", "PROXY 127.0.0.1:2222"], "the PAC follows the new port, parked in between");
  if (env.log.lastPac.indexOf("tailtabExitModeProxies") === -1) {
    throw new Error("exit mode was lost across the host restart");
  }
});

test("Firefox routes through the exit node too", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status(EXIT_RUNNING);
  await flush();

  const via = { type: "socks", host: "127.0.0.1", port: 64378, proxyDNS: true, username: "tailtab", password: TOKEN };
  eq(env.resolve("https://github.com/"), via, "the public internet in exit mode");
  eq(env.resolve("http://wiki/"), via, "a tailnet host in exit mode");
  eq(env.resolve("http://192.168.1.1/"), { type: "direct" }, "the LAN in exit mode");
  eq(env.resolve("http://127.0.0.1:9000/"), { type: "direct" }, "loopback in exit mode");

  env.status(Object.assign({}, EXIT_RUNNING, { exitNode: "", exitNodeActive: false }));
  await flush();
  eq(env.resolve("https://github.com/"), { type: "direct" }, "the public internet with no exit node");
});

test("the exit-node command carries the id to the host", async () => {
  const env = makeEnv();
  await flush();
  env.status(EXIT_RUNNING);
  await flush();
  const port = env.openPopup();
  port.onMessage._fire({ cmd: "exitnode", id: "nodeid-attic" });
  eq(env.log.sentFull[env.log.sentFull.length - 1], { cmd: "exitnode", id: "nodeid-attic" }, "the message sent");
  port.onMessage._fire({ cmd: "exitnode" });
  eq(env.log.sentFull[env.log.sentFull.length - 1], { cmd: "exitnode", id: "" }, "clearing the selection");
});

test("the popup lists the exit nodes and says which one carries the traffic", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, { state: "Running" }),
  });
  eq(ui.els.state.textContent, "Connected via server", "state line");
  eq(ui.els.exitrow.hidden, false, "the picker is shown");
  eq(ui.exitOptions(), [
    { value: "", label: "None", disabled: false },
    { value: "nodeid-attic", label: "attic (offline)", disabled: true },
    { value: "nodeid-server", label: "server", disabled: false },
  ], "the options");
  eq(ui.els.exitnode.value, "nodeid-server", "the selection shown");
});

test("located exit nodes are grouped by country and labelled by city", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, {
      state: "Running",
      exitNode: "nodeid-sto",
      exitNodes: [
        { id: "nodeid-server", name: "server", online: true, os: "linux" },
        { id: "nodeid-prg1", name: "cz-prg-wg-001", online: true, os: "linux", country: "Czechia", city: "Prague" },
        { id: "nodeid-prg2", name: "cz-prg-wg-002", online: true, os: "linux", country: "Czechia", city: "Prague" },
        { id: "nodeid-sto", name: "se-sto-wg-001", online: true, os: "linux", country: "Sweden", city: "Stockholm" },
      ],
    }),
  });

  eq(ui.els.state.textContent, "Connected via Sweden — Stockholm", "state line");
  eq(ui.exitOptions().filter((o) => o.label), [
    { value: "", label: "None", disabled: false },
    { value: "nodeid-server", label: "server", disabled: false },
  ], "the tailnet's own machines stay ungrouped");
  eq(ui.exitGroups(), [
    {
      country: "Czechia",
      options: [
        // One city, two nodes: the hostname is the only thing telling them apart.
        { value: "nodeid-prg1", label: "Prague (cz-prg-wg-001)" },
        { value: "nodeid-prg2", label: "Prague (cz-prg-wg-002)" },
      ],
    },
    { country: "Sweden", options: [{ value: "nodeid-sto", label: "Stockholm" }] },
  ], "the grouped options");
});

test("the popup says browsing is blocked when the exit node is offline", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, { state: "Running", exitNodeActive: false }),
  });
  if (ui.els.state.textContent.indexOf("blocked") === -1) {
    throw new Error("the popup does not say browsing is blocked: " + ui.els.state.textContent);
  }
  if (ui.els.state.textContent.indexOf("server") !== -1 && ui.els.state.textContent.indexOf("via") !== -1) {
    throw new Error("the popup claims traffic is going via the exit node");
  }
});

test("no exit nodes means no picker", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "tail4d5e6f.ts.net", proxyPort: 64378, warnings: [], exitNodes: [] },
  });
  eq(ui.els.exitrow.hidden, true, "the picker is hidden on a tailnet with no exit node");
  eq(ui.els.state.textContent, "Connected", "state line");
});

test("picking an exit node asks the host and waits for it", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, { state: "Running", exitNode: "", exitNodeActive: false }),
  });
  ui.chooseExitNode("nodeid-server");
  eq(ui.sent[ui.sent.length - 1], { cmd: "exitnode", id: "nodeid-server" }, "the command sent");
  // Keep the displayed selection host-authoritative while the change is pending.
  eq(ui.els.state.textContent, "Connected", "the state line before the host answers");
});

// Chromium retains PAC across worker reloads, but the native host does not survive.
test("a PAC left by an earlier worker is parked at startup, not cleared", async () => {
  const env = makeEnv({ levelOfControl: "controlled_by_this_extension" });
  await flush();
  eq(env.log.clear, [], "the leftover setting was not cleared: that would leak tailnet names to DNS");
  eq(env.log.set, ["PROXY 0.0.0.1:1"], "it was parked on a dead port before any host answered");

  env.status(RUNNING);
  await flush();
  eq(env.log.set, ["PROXY 0.0.0.1:1", "PROXY 127.0.0.1:64378"], "the live host's PAC replaced the parked one once it was Running");
  eq(env.log.clear.length, 0, "and nothing was cleared along the way");
});

test("a setting nobody left behind is not touched at startup", async () => {
  const env = makeEnv();
  await flush();
  eq(env.log.clear, [], "nothing to drop");
  const policy = makeEnv({ levelOfControl: "controlled_by_policy" });
  await flush();
  eq(policy.log.clear, [], "a policy setting is not ours to drop");
});

// MV3 sleep drops reconnect timers, while alarms can wake the worker.
test("the heartbeat alarm reconnects a dead host", async () => {
  const env = makeEnv();
  await flush();
  eq(env.log.alarms, [{ name: "tailtab-heartbeat", info: { periodInMinutes: 1 } }], "the alarm is registered at startup");
  env.status(RUNNING);
  await flush();
  const before = env.log.connects;
  env.disconnect();
  await flush();
  env.fireAlarm("tailtab-heartbeat");
  await flush();
  eq(env.log.connects, before + 1, "the alarm reconnected");
  env.fireAlarm("tailtab-heartbeat");
  await flush();
  eq(env.log.connects, before + 1, "a live port is left alone");
  env.fireAlarm("some-other-alarm");
  await flush();
  eq(env.log.connects, before + 1, "another extension's alarm name is ignored");
});

test("the account switcher lists held accounts and switches on click", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, {
      state: "Running", exitNode: "", exitNodeActive: false,
      accounts: [
        { id: "p1", name: "stocist@github", tailnet: "tail1a2b3c.ts.net", active: true },
        { id: "p2", name: "bob@example.com", tailnet: "tail4d5e6f.ts.net", active: false },
      ],
    }),
  });
  eq(ui.els.accountname.textContent, "stocist@github", "header name falls back to the login name");
  eq(ui.els.accounttailnet.textContent, "tail1a2b3c.ts.net", "header tailnet");
  eq(ui.accountItems().map((i) => i.label), ["stocist@github", "bob@example.com", "", "+ Add account…"], "menu entries");
  eq(ui.accountItems()[0].className, "item on", "the active account is marked");

  ui.chooseAccount("p2");
  eq(ui.sent[ui.sent.length - 1], { cmd: "switch", id: "p2" }, "the switch command");
  eq(ui.els.state.textContent, "Switching account…", "the pill while the host works");

  ui.push({
    connected: true,
    status: Object.assign({ warnings: [] }, EXIT_RUNNING, {
      state: "Running", exitNode: "", exitNodeActive: false, tailnet: "tail4d5e6f.ts.net",
      accounts: [
        { id: "p1", name: "stocist@github", tailnet: "tail1a2b3c.ts.net", active: false },
        { id: "p2", name: "bob@example.com", tailnet: "tail4d5e6f.ts.net", active: true },
      ],
    }),
  });
  eq(ui.els.accountname.textContent, "bob@example.com", "header after the switch");
  eq(ui.els.state.textContent, "Connected", "pill after the switch");
});

test("choosing the active account again does nothing", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [],
      accounts: [{ id: "p1", name: "a@b", tailnet: "t.ts.net", active: true }] },
  });
  const before = ui.sent.length;
  ui.chooseAccount("p1");
  eq(ui.sent.length, before, "nothing sent");
});

test("Add account asks the host and shows the login once it arrives", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [],
      accounts: [{ id: "p1", name: "a@b", tailnet: "t.ts.net", active: true }] },
  });
  ui.clickAddAccount();
  eq(ui.sent[ui.sent.length - 1], { cmd: "addaccount" }, "the command");
  eq(ui.els.accountname.textContent, "Switching…", "header while the host works");
  // The host reports the new profile first and the URL once control answers.
  ui.push({ connected: true, status: { state: "NeedsLogin", authURL: "", error: "You are logged out. The last login error was: fetch control key: context canceled", warnings: ["You are logged out. The last login error was: fetch control key: context canceled"], accounts: [] } });
  eq(ui.opened, [], "nothing to open yet");
  eq(ui.els.hint.textContent, "Requesting login link…", "the popup keeps waiting for the link");
  ui.push({ connected: true, status: { state: "NeedsLogin", authURL: "https://login.tailscale.com/a/1", warnings: ["You are logged out. The last login error was: fetch control key: context canceled"], accounts: [] } });
  eq(ui.opened, ["https://login.tailscale.com/a/1"], "the login page was opened without another click");
  eq(ui.els.state.textContent, "Not logged in", "pill once the new profile is ready");
  eq(ui.warningTexts(), [], "the cancelled attempt's warning is not shown beside a working login");
  eq(ui.els.accountname.textContent, "Not logged in", "the header does not show the old account or the OS hostname");
});

test("a login URL that nobody asked for is offered, not opened", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, status: { state: "NeedsLogin", authURL: "https://login.tailscale.com/a/2", warnings: [], accounts: [] } });
  eq(ui.opened, [], "nothing opened on its own");
  if (ui.els.login.hidden) throw new Error("the Log in button is hidden");
});

test("a node that never logged in has no accounts and says so", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, status: { state: "NeedsLogin", warnings: [], accounts: [] } });
  eq(ui.els.accountname.textContent, "Not logged in", "header");
  eq(ui.accountItems().map((i) => i.label), ["+ Add account…"], "menu has only Add account");
});

test("the machine search filters peers and offers their addresses", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [],
      peers: [
        { name: "server", dnsName: "server.t.ts.net", ip: "100.80.1.7", online: true },
        { name: "relay", dnsName: "relay.t.ts.net", ip: "100.80.1.65", online: true },
        { name: "pc", dnsName: "pc.t.ts.net", ip: "100.80.1.94", online: false },
      ] },
  });
  eq(ui.els.machinesec.hidden, false, "the search box is shown when there are peers");
  eq(ui.machineRows().map((r) => r.name), ["server", "relay", "pc"], "the first few are listed up front, online first");
  ui.search("ser");
  eq(ui.machineRows().map((r) => r.name + " " + r.ip), ["server 100.80.1.7"], "a name match");
  ui.search("1.94");
  eq(ui.machineRows().map((r) => r.name), ["pc"], "an address match");
  eq(ui.machineRows()[0].className, "name off", "an offline peer is marked");
  ui.search("zzz");
  eq(ui.machineRows().map((r) => r.name), ["No machine matches"], "no match");
});

test("the toggle disconnects a running node and connects a stopped one", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [] } });
  eq(ui.els.toggle.className, "on", "toggle on while Running");
  ui.clickToggle();
  eq(ui.sent[ui.sent.length - 1], { cmd: "down" }, "down");
  ui.push({ connected: true, status: { state: "Stopped", warnings: [], accounts: [] } });
  eq(ui.els.toggle.className, "", "toggle off while Stopped");
  ui.clickToggle();
  eq(ui.sent[ui.sent.length - 1], { cmd: "up" }, "up");
});

test("more machines than the preview shows are counted, not listed", () => {
  const ui = openPopupUI();
  const peers = [];
  for (let i = 0; i < 21; i++) peers.push({ name: "m" + i, dnsName: "m" + i + ".t.ts.net", ip: "100.64.0." + i, online: i % 2 === 0 });
  ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [], peers: peers } });
  const rows = ui.machineRows();
  eq(rows.length, 4, "three machines and the count line");
  eq(rows[3].name, "18 more · type to filter", "the count line");
  eq(rows.slice(0, 3).every((r) => r.className === "name"), true, "online machines come first");

  ui.els.machines.children[3].children[1].listeners.click();
  const all = ui.machineRows();
  eq(all.length, 22, "21 machines and the fold line");
  eq(all[21].name, "21 machines", "the fold line");
  ui.els.machines.children[21].children[1].listeners.click();
  eq(ui.machineRows().length, 4, "folded back to the preview");
});

test("the header shows the display name and picture when the account has them", () => {
  const ui = openPopupUI();
  ui.push({
    connected: true,
    status: { state: "Running", tailnet: "tail1a2b3c.ts.net", hostname: "laptop-tailtab-edge", proxyPort: 1, warnings: [],
      accounts: [{ id: "p1", name: "alice@github", displayName: "Alice", picture: "https://avatars.githubusercontent.com/u/1?v=4", tailnet: "tail1a2b3c.ts.net", active: true }] },
  });
  eq(ui.els.accountname.textContent, "Alice", "display name, not the login name");
  eq(ui.els.avatar.children.length, 1, "the avatar is a picture");
  eq(ui.els.avatar.children[0].src, "https://avatars.githubusercontent.com/u/1?v=4", "picture url");
  eq(ui.accountItems()[0].label, "Alice", "menu entry uses the display name too");
});

test("a worker from another build is reported instead of silently ignored", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, build: "older", status: { state: "Running", tailnet: "t.ts.net", hostname: "mac-tailtab-edge", proxyPort: 1, warnings: [] } });
  eq(ui.els.warning.hidden, false, "the banner is shown");
  if (!/Reload the extension/.test(ui.els.warning.textContent)) throw new Error("banner text: " + ui.els.warning.textContent);
  eq(ui.els.accountname.textContent, "mac-tailtab-edge", "the header does not pretend to know the account");
  // Missing build metadata also identifies a stale worker.
  const ui2 = openPopupUI();
  ui2.push({ connected: true, build: null, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [] } });
  eq(ui2.els.warning.hidden, false, "a worker with no build id is treated as old");
});

test("the toolbar icon follows the routing state", async () => {
  const env = makeEnv();
  await flush();
  const last = () => env.log.icons[env.log.icons.length - 1];
  env.status({ state: "NeedsLogin" });
  await flush();
  eq(last(), "icons/attention/icon16.png", "login needed");
  env.status(RUNNING);
  await flush();
  eq(last(), "icons/connected/icon16.png", "routing");
  env.status(Object.assign({}, RUNNING, { exitNode: "n1", exitNodeActive: false, exitNodes: [{ id: "n1", name: "server", online: false }] }));
  await flush();
  eq(last(), "icons/blocked/icon16.png", "exit node offline blocks browsing");
  env.status({ state: "Stopped" });
  await flush();
  eq(last(), "icons/idle/icon16.png", "stopped by the user");
  env.disconnect();
  await flush();
  eq(last(), "icons/attention/icon16.png", "host gone, reconnecting");
  const before = env.log.icons.length;
  env.disconnect();
  await flush();
  eq(env.log.icons.length, before, "an unchanged state does not set the icon again");
});

test("routed subnets are proxied, in both modes, and the PAC knows them", () => {
  const rules = require(path.join(SRC, "rules.js"));
  const routes = ["192.168.1.0/24", "fd00:1:2::/64"];
  eq(rules.tailtabInRoutes("192.168.1.77", routes), true, "v4 inside");
  eq(rules.tailtabInRoutes("192.168.2.77", routes), false, "v4 outside");
  eq(rules.tailtabInRoutes("fd00:1:2:0:aa::1", routes), true, "v6 inside");
  eq(rules.tailtabInRoutes("fd00:1:3::1", routes), false, "v6 outside");
  eq(rules.tailtabInRoutes("192.168.1.77", ["0.0.0.0/0"]), false, "a /0 is not a subnet route and is ignored");
  eq(rules.tailtabInRoutes("192.168.1.77", ["128.0.0.0/1"]), false, "so is anything broader than /8");
  eq(rules.tailtabInRoutes("127.0.0.1", ["127.0.0.0/8"]), false, "a route covering loopback is ignored");
  eq(rules.tailtabInRoutes("169.254.169.254", ["169.254.0.0/16"]), false, "a link-local route is ignored");
  eq(rules.tailtabInRoutes("fe80::1", ["fe80::/10"]), false, "an IPv6 link-local route is ignored");
  eq(rules.tailtabInRoutes("::1", ["::1/128"]), false, "a loopback /128 route is ignored, as the host ignores it");
  eq(rules.tailtabInRoutes("::1", ["::/16"]), false, "a route starting at :: is ignored");
  eq(rules.tailtabInRoutes("ff02::1", ["ff02::/16"]), false, "a multicast route is ignored");
  eq(rules.tailtabInRoutes("10.0.0.1", ["10.0.0.0/8"]), true, "/8 is honoured");
  eq(rules.tailtabInRoutes("192.168.1.77", ["192.168.1.0/33", "junk", "192.168.1/24", "300.1.1.1/8"]), false, "malformed routes are ignored");
  const pac = rules.tailtabBuildPac(64378, "tail4d5e6f.ts.net", false, routes);
  const find = vm.runInNewContext(pac + "\nFindProxyForURL", {});
  eq(find("http://192.168.1.10/", "192.168.1.10"), "PROXY 127.0.0.1:64378", "routed address via the PAC");
  eq(find("http://192.168.2.10/", "192.168.2.10"), "DIRECT", "unrouted private address via the PAC");
  eq(find("http://[fd00:1:2::5]/", "fd00:1:2::5"), "PROXY 127.0.0.1:64378", "routed IPv6 via the PAC");
  if (!/^[\x00-\x7f]*$/.test(pac)) throw new Error("the PAC is not ASCII");
  const exit = vm.runInNewContext(rules.tailtabBuildPac(64378, "", true, routes) + "\nFindProxyForURL", {});
  eq(exit("http://192.168.1.10/", "192.168.1.10"), "PROXY 127.0.0.1:64378", "routed LAN still via the tailnet in exit mode");
  eq(exit("http://192.168.2.10/", "192.168.2.10"), "DIRECT", "unrouted LAN stays local in exit mode");
  // Malformed routes must not enter generated PAC source.
  const dirty = rules.tailtabBuildPac(64378, "", false, ["192.168.1.0/24", "evil\"); alert(1); //"]);
  if (dirty.indexOf("alert") !== -1) throw new Error("an unvalidated route reached the PAC");
});

test("a change in subnet routes rewrites the PAC", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  const before = env.log.set.length;
  env.status(Object.assign({}, RUNNING, { subnetRoutes: ["192.168.1.0/24"] }));
  await flush();
  eq(env.log.set.length, before + 1, "the PAC was reinstalled");
  if (env.log.lastPac.indexOf("192.168.1.0/24") === -1) throw new Error("the new PAC does not carry the route");
  env.status(Object.assign({}, RUNNING, { subnetRoutes: ["192.168.1.0/24"] }));
  await flush();
  eq(env.log.set.length, before + 1, "the same routes again do not rewrite it");
});

test("Firefox proxies a routed subnet address", async () => {
  const env = makeFirefoxEnv();
  await flush();
  env.status(Object.assign({}, RUNNING, { proxyToken: "tok", subnetRoutes: ["10.42.0.0/16"] }));
  await flush();
  eq(env.resolve("http://10.42.7.1:8080/").type, "socks", "routed address");
  eq(env.resolve("http://10.43.0.1/").type, "direct", "unrouted address");
});

test("the popup lists subnet routes when there are any", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [], subnetRoutes: ["192.168.1.0/24", "10.0.0.0/8"] } });
  eq(ui.els.routesrow.hidden, false, "row shown");
  eq(ui.els.routes.textContent, "192.168.1.0/24, 10.0.0.0/8", "routes listed");
  ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [], subnetRoutes: [] } });
  eq(ui.els.routesrow.hidden, true, "row hidden without routes");
});

test("the control server from settings goes to the host on init and add account", async () => {
  const env = makeEnv({ local: { controlURL: "https://headscale.example.com" } });
  await flush();
  const init = env.log.sentFull.find((m) => m.cmd === "init");
  eq(init.controlURL, "https://headscale.example.com", "init carries it");
  env.status(RUNNING);
  await flush();
  const popup = env.openPopup();
  popup.onMessage._fire({ cmd: "addaccount" });
  await flush();
  const add = env.log.sentFull.filter((m) => m.cmd === "addaccount").pop();
  eq(add.controlURL, "https://headscale.example.com", "addaccount carries it");

  const plain = makeEnv();
  await flush();
  const init2 = plain.log.sentFull.find((m) => m.cmd === "init");
  eq(init2.controlURL, undefined, "no field at all for Tailscale's server");
});

test("the popup shows a custom control server and hides Tailscale's", () => {
  const ui = openPopupUI();
  ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [], controlURL: "https://controlplane.tailscale.com" } });
  eq(ui.els.controlrow.hidden, true, "default server hidden");
  ui.push({ connected: true, status: { state: "Running", tailnet: "hs.example.com", proxyPort: 1, warnings: [], accounts: [], controlURL: "https://headscale.example.com/" } });
  eq(ui.els.controlrow.hidden, false, "custom server shown");
  eq(ui.els.control.textContent, "headscale.example.com", "shown without the scheme");
});

test("the settings page validates a control server like the host does", () => {
  const opts = require(path.join(SRC, "options.js"));
  eq(opts.tailtabValidControlURL(""), "", "empty is Tailscale's");
  eq(opts.tailtabValidControlURL("https://headscale.example.com"), "", "https ok");
  eq(opts.tailtabValidControlURL("http://10.0.0.5:8080"), "", "http with port ok");
  for (const bad of ["headscale.example.com", "ftp://x.example", "https://u:p@hs.example.com", "https://hs.example.com/?x=1", "https://hs.example.com/#f"]) {
    if (!opts.tailtabValidControlURL(bad)) throw new Error("accepted " + bad);
  }
});

test("a user disconnect hands the proxy setting back; a crash parks it", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  env.popup("down");
  env.status({ state: "Stopped" });
  await flush();
  eq(env.log.clear, ["regular"], "Stopped by the user clears the setting");
  env.status(RUNNING);
  await flush();
  env.disconnect();
  await flush();
  eq(env.log.clear.length, 1, "a crash does not clear it");
  eq(env.log.set[env.log.set.length - 1], "PROXY 0.0.0.1:1", "it parks it");
  if (env.log.lastPac.indexOf("tailtabIsTailnetHost") === -1) throw new Error("the parked PAC lost the rules");
});

test("the parked PAC keeps exit mode, which is the kill switch", async () => {
  const env = makeEnv();
  await flush();
  env.status(Object.assign({}, RUNNING, { exitNodes: [{ id: "n1", name: "server", online: true }], exitNode: "n1", exitNodeActive: true }));
  await flush();
  env.disconnect();
  await flush();
  const find = vm.runInNewContext(env.log.lastPac + "\nFindProxyForURL", {});
  eq(find("https://github.com/", "github.com"), "PROXY 0.0.0.1:1", "public traffic stays blocked while the host is gone");
  eq(find("http://192.168.0.5/", "192.168.0.5"), "DIRECT", "the LAN stays local");
});

test("a logout the user asked for hands the setting back; a switch that lands on login parks it", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  const popup = env.openPopup();
  popup.onMessage._fire({ cmd: "switch", id: "p2" });
  env.status({ state: "NeedsLogin" });
  await flush();
  eq(env.log.clear, [], "mid-switch the setting is not cleared");
  eq(env.log.set[env.log.set.length - 1], "PROXY 0.0.0.1:1", "it is parked");

  env.status(RUNNING);
  await flush();
  popup.onMessage._fire({ cmd: "logout" });
  env.status({ state: "NeedsLogin" });
  await flush();
  eq(env.log.clear, ["regular"], "a logout the user asked for clears it");

  env.status(RUNNING);
  await flush();
  env.disconnect();
  await flush();
  eq(env.log.clear.length, 1, "a crash after that still parks rather than clears");
});

test("the rules snapshot in storage.local feeds the startup park after a browser restart", async () => {
  const first = makeEnv();
  await flush();
  first.status(Object.assign({}, RUNNING, { tailnet: "corp.example.com", subnetRoutes: ["10.42.0.0/16"] }));
  await flush();
  const snap = first.log.localSet.filter((v) => v.rules).pop();
  eq(snap.rules, { tailnet: "corp.example.com", subnetRoutes: ["10.42.0.0/16"], exitNode: "" }, "the snapshot holds the routing facts and nothing else");

  const env = makeEnv({ levelOfControl: "controlled_by_this_extension", local: { rules: snap.rules } });
  await flush();
  const find = vm.runInNewContext(env.log.lastPac + "\nFindProxyForURL", {});
  eq(find("http://nas.corp.example.com/", "nas.corp.example.com"), "PROXY 0.0.0.1:1", "the custom domain is parked, not sent to DNS");
  eq(find("http://10.42.7.1/", "10.42.7.1"), "PROXY 0.0.0.1:1", "the routed subnet too");
});


test("the startup park carries the last known rules, not blank ones", async () => {
  const env = makeEnv({ levelOfControl: "controlled_by_this_extension", session: { status: { tailnet: "corp.example.com", subnetRoutes: ["10.42.0.0/16"], exitNode: "n1" } } });
  await flush();
  eq(env.log.set, ["PROXY 0.0.0.1:1"], "parked");
  const find = vm.runInNewContext(env.log.lastPac + "\nFindProxyForURL", {});
  eq(find("https://github.com/", "github.com"), "PROXY 0.0.0.1:1", "exit mode from before the restart is still in force (kill switch)");
  eq(find("http://192.168.0.5/", "192.168.0.5"), "DIRECT", "the LAN stays local");
});

test("a parked configuration does not fight a policy", async () => {
  const env = makeEnv({ levelOfControl: "controlled_by_policy" });
  await flush();
  env.status(RUNNING);
  await flush();
  env.disconnect();
  await flush();
  eq(env.log.set, [], "nothing was installed over the policy");
});

test("the installed PAC is mandatory, so a script failure blocks instead of going DIRECT", async () => {
  const env = makeEnv();
  await flush();
  env.status(RUNNING);
  await flush();
  eq(env.log.lastValue.pacScript.mandatory, true, "split tunnel");
  env.status(Object.assign({}, RUNNING, { exitNodes: [{ id: "n1", name: "server", online: true }], exitNode: "n1", exitNodeActive: true }));
  await flush();
  eq(env.log.lastValue.pacScript.mandatory, true, "exit mode");
  env.disconnect();
  await flush();
  eq(env.log.lastValue.pacScript.mandatory, true, "parked");
});

test("a popup port from anywhere but the popup page is ignored", async () => {
  const env = makeEnv();
  await flush();
  env.status({ state: "Running", proxyPort: 64378 });
  await flush();
  const before = env.popupMessages.length;
  const strangers = [
    null,
    { id: "tailtab-test", url: "https://example.com/" },
    { id: "someone-else", url: "chrome-extension://someone-else/popup.html" },
    { id: "tailtab-test", url: "chrome-extension://tailtab-test/options.html" },
  ];
  for (const sender of strangers) env.openPopup(sender).onMessage._fire({ cmd: "logout" });
  await flush();
  if (env.log.sent.includes("logout")) throw new Error("a stranger's logout reached the host");
  eq(env.popupMessages.length, before, "strangers are not answered");
  env.openPopup().onMessage._fire({ cmd: "status" });
  await flush();
  if (env.popupMessages.length <= before) throw new Error("the popup page itself was not answered");
});

test("a login link that is not https is never opened", () => {
  const ui = openPopupUI();
  const needsLogin = (authURL) => ui.push({ connected: true, status: { state: "NeedsLogin", authURL: authURL, warnings: [], accounts: [] } });
  needsLogin("");
  ui.clickToggle();
  needsLogin("javascript:alert(1)");
  ui.els.login.listeners.click();
  ui.clickToggle();
  needsLogin("data:text/html,hi");
  ui.els.login.listeners.click();
  eq(ui.opened, [], "neither javascript: nor data: was opened");
  needsLogin("https://login.tailscale.com/a/1");
  ui.els.login.listeners.click();
  eq(ui.opened, ["https://login.tailscale.com/a/1"], "https is opened");
});

test("a machine opens only as a bare tailnet host", () => {
  const ui = openPopupUI();
  const push = (peers) => ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [], accounts: [], peers: peers } });
  const click = (i) => ui.els.machines.children[i].children[0].listeners.click();
  push([
    { name: "server", dnsName: "server.t.ts.net", ip: "100.80.1.7", online: true },
    { name: "evil", dnsName: "evil.com", ip: "100.80.1.8", online: true },
    { name: "foo", dnsName: "foo@127.0.0.1", ip: "100.80.1.9", online: true },
  ]);
  eq(ui.els.machines.children[0].children[0].title, "Open http://server.t.ts.net/", "the link offered matches what opens");
  eq(ui.els.machines.children[1].children[0].title, "", "no link is offered for a name that will not open");
  click(0); click(1); click(2);
  push([
    { name: "local", dnsName: "127.0.0.1:8080", ip: "100.80.1.10", online: true },
    { name: "path", dnsName: "server.t.ts.net/admin", ip: "100.80.1.11", online: true },
    { name: "", dnsName: "", ip: "100.80.1.12", online: true },
  ]);
  click(0); click(1); click(2);
  eq(ui.opened, ["http://server.t.ts.net/", "http://100.80.1.12/"], "only the tailnet name and the tailnet address were opened");
});

test("an avatar loads only over https from a public host", () => {
  const ui = openPopupUI();
  const pictures = [
    "http://avatars.example/a.png",
    "https://127.0.0.1/a.png",
    "https://localhost/a.png",
    "https://192.168.1.1/a.png",
    "https://[::1]/a.png",
    "https://user:pw@avatars.example/a.png",
    "https://avatars.example:8443/a.png",
    "javascript:alert(1)",
  ];
  for (const picture of pictures) {
    ui.push({ connected: true, status: { state: "Running", tailnet: "t.ts.net", proxyPort: 1, warnings: [],
      accounts: [{ id: "p1", name: "alice@github", displayName: "Alice", picture: picture, tailnet: "t.ts.net", active: true }] } });
    eq(ui.els.avatar.children.length, 0, "no picture for " + picture);
    eq(ui.els.avatar.textContent, "A", "the initial stands in for " + picture);
  }
});

test("Connect while logged out opens the link even when a logged-out warning lands first", () => {
  const ui = openPopupUI();
  const needsLogin = (fields) => ui.push({ connected: true, status: Object.assign({ state: "NeedsLogin", authURL: "", warnings: [], accounts: [] }, fields) });
  needsLogin({});
  ui.clickToggle();
  eq(ui.sent[ui.sent.length - 1], { cmd: "up" }, "the command");
  needsLogin({ error: "You are logged out. The last login error was: fetch control key: context canceled" });
  eq(ui.opened, [], "nothing to open yet");
  eq(ui.els.hint.textContent, "Requesting login link…", "still waiting");
  eq(ui.els.connect.disabled, true, "Connect stays disabled while waiting");
  needsLogin({ authURL: "https://login.tailscale.com/a/3" });
  eq(ui.opened, ["https://login.tailscale.com/a/3"], "the link was opened when it arrived");
});

test("a real error ends the wait for a login link", () => {
  const ui = openPopupUI();
  const needsLogin = (fields) => ui.push({ connected: true, status: Object.assign({ state: "NeedsLogin", authURL: "", warnings: [], accounts: [] }, fields) });
  needsLogin({});
  ui.clickToggle();
  needsLogin({ error: "starting interactive login: control is unreachable" });
  eq(ui.els.hint.textContent, "starting interactive login: control is unreachable", "the error is shown");
  eq(ui.els.connect.disabled, false, "Connect is offered again");
  needsLogin({ authURL: "https://login.tailscale.com/a/4" });
  eq(ui.opened, [], "a link nobody is waiting for is offered, not opened");
  eq(ui.els.login.hidden, false, "the Log in button is offered");
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
