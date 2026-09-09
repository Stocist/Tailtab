"use strict";

const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { EventEmitter } = require("node:events");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { PassThrough } = require("node:stream");
const test = require("node:test");
const vm = require("node:vm");

const script = path.join(__dirname, "../smoke.js");
const source = fs.readFileSync(script, "utf8");
const profileID = "0f8fad5b-d9cb-469f-a165-70867728950e";
const loginURL = "https://login.tailscale.com/a/test-secret";
const token = "private-proxy-token";

function frame(event) {
  const body = Buffer.from(JSON.stringify(event));
  const header = Buffer.alloc(4);
  header.writeUInt32LE(body.length);
  return Buffer.concat([header, body]);
}

function harness() {
  const child = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    pid: 123, exitCode: null, signalCode: null,
    kill(signal) { this.signalCode = signal; },
  });
  const messages = [];
  const removed = [];
  const timers = new Map();
  const proc = { argv: ["node", script, "test-host"], platform: "linux", env: {} };
  const modules = {
    "node:child_process": { spawn: (_bin, _args, options) => {
      assert.deepEqual(Array.from(options.stdio), ["pipe", "pipe", "pipe"]);
      return child;
    } },
    "node:crypto": { randomUUID: () => profileID },
    "node:os": { homedir: () => "/smoke-test-home" },
    "node:fs": { rmSync: (dir) => removed.push(dir) },
  };
  vm.runInNewContext(source, {
    require: (name) => modules[name] || require(name), Buffer, process: proc,
    console: {
      log: (...args) => messages.push(args.join(" ")),
      error: (...args) => messages.push(args.join(" ")),
    },
    setTimeout: (fn, ms) => { timers.set(fn, ms); return fn; },
    clearTimeout: (id) => timers.delete(id),
  }, { filename: script });
  return {
    child, proc, messages, removed, timers,
    send: (event) => child.stdout.write(frame(event)),
    expire(ms) {
      const timer = Array.from(timers).find(([, delay]) => delay === ms);
      assert.ok(timer, `missing ${ms}ms timer`);
      timers.delete(timer[0]);
      timer[0]();
    },
    async close(code) {
      child.exitCode = code;
      child.stdout.end();
      child.stderr.end();
      await new Promise((resolve) => setImmediate(resolve));
      child.emit("close", code);
      assert.deepEqual(removed, [path.join("/smoke-test-home", ".config", "tailtab", profileID)]);
      assert.equal(timers.size, 0);
    },
  };
}

test("a missing executable fails even though spawn never emits exit", (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tailtab-smoke-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const result = spawnSync(process.execPath, [script, path.join(dir, "missing-host")], {
    env: { ...process.env, HOME: dir, USERPROFILE: dir, APPDATA: dir, XDG_CONFIG_HOME: dir },
    encoding: "utf8", timeout: 10000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /FAIL: could not start host/);
});

test("success handles split frames and redacts stderr URLs across chunks and EOF", async () => {
  const h = harness();
  const event = frame({ event: "status", proxyPort: 12345, authURL: loginURL, proxyToken: token });
  h.child.stderr.write("AuthURL is https://login.tailscale.com/a/");
  h.child.stderr.write("test-secret\nunterminated " + loginURL);
  h.child.stdout.write(event.subarray(0, 3));
  h.child.stdout.write(event.subarray(3));
  assert.equal(h.child.stdin.writableEnded, true);
  await h.close(0);
  assert.equal(h.proc.exitCode, 0);
  const output = h.messages.join("\n");
  assert.match(output, /OK: proxy listening/);
  assert.match(output, /AuthURL is <redacted URL>/);
  assert.match(output, /unterminated <redacted URL>/);
  assert.ok(!output.includes("test-secret"), output);
  assert.ok(!output.includes(token), output);
});

test("a host-reported error fails and redacts URLs in diagnostics", async () => {
  const h = harness();
  h.send({ event: "error", error: "login failed: " + loginURL, authURL: loginURL, proxyToken: token });
  assert.equal(h.proc.exitCode, 1);
  await h.close(0);
  assert.equal(h.proc.exitCode, 1);
  assert.match(h.messages.join("\n"), /FAIL: host reported an error/);
  assert.ok(!h.messages.join("\n").includes("test-secret"));
  assert.ok(!h.messages.join("\n").includes(token));
});

for (const code of [0, 1]) {
  test(`early host exit ${code} fails and cleans up the profile`, async () => {
    const h = harness();
    await h.close(code);
    assert.equal(h.proc.exitCode, 1);
    assert.match(h.messages.join("\n"), /FAIL: host exited early/);
  });
}

test("a proxy without a login URL times out and removes its state", async () => {
  const h = harness();
  h.send({ event: "status", proxyPort: 12345, state: "NeedsLogin" });
  h.expire(90000);
  assert.equal(h.proc.exitCode, 1);
  assert.equal(h.child.stdin.writableEnded, true);
  await h.close(0);
  assert.match(h.messages.join("\n"), /FAIL: no login URL/);
});

test("a host that will not shut down is killed and fails the smoke test", async () => {
  const h = harness();
  h.send({ event: "status", proxyPort: 12345, authURL: loginURL });
  h.expire(5000);
  assert.equal(h.child.signalCode, "SIGKILL");
  await h.close(null);
  assert.equal(h.proc.exitCode, 1);
});

test("a failed shutdown overrides the successful login round trip", async () => {
  const h = harness();
  h.send({ event: "status", proxyPort: 12345, authURL: loginURL });
  await h.close(1);
  assert.equal(h.proc.exitCode, 1);
});

test("a broken stdin pipe fails without skipping cleanup", async () => {
  const h = harness();
  h.child.stdin.emit("error", new Error("EPIPE"));
  await h.close(1);
  assert.equal(h.proc.exitCode, 1);
  assert.match(h.messages.join("\n"), /FAIL: could not send to host/);
});

for (const malformed of [Buffer.from([1, 0, 0, 0, 123]), frame(null), Buffer.from([1, 0, 16, 0])]) {
  test(`malformed host event ${malformed.toString("hex")} fails with cleanup`, async () => {
    const h = harness();
    h.child.stdout.write(malformed);
    await h.close(0);
    assert.equal(h.proc.exitCode, 1);
    assert.match(h.messages.join("\n"), /FAIL: (invalid native-messaging event|host message exceeds)/);
  });
}
