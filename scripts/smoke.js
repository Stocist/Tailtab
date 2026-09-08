#!/usr/bin/env node
// Drives a built host binary over the native-messaging protocol, the way a
// browser would, and checks it reaches the control plane. No browser needed.
//
//   node scripts/smoke.js bin/tailtab
//
// Sends `init` for a fresh profile and waits for a status event that carries
// a proxy port and a login URL, which proves tsnet started, the loopback
// proxy is listening and the coordination server answered.
"use strict";
const { spawn } = require("node:child_process");
const { randomUUID } = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const bin = process.argv[2];
if (!bin) {
  console.error("usage: smoke.js <host binary>");
  process.exit(2);
}
const timeoutMs = Number(process.env.SMOKE_TIMEOUT_MS || 90000);
const profileID = randomUUID();

// The host keeps node state per profile under the user's config directory
// (see docs/architecture.md). The profile is ours and random, so remove it
// afterwards rather than leave a directory behind on every run.
function stateDir() {
  const home = os.homedir();
  switch (process.platform) {
    case "darwin":
      return path.join(home, "Library", "Application Support", "tailtab", profileID);
    case "win32":
      return path.join(process.env.APPDATA || path.join(home, "AppData", "Roaming"), "tailtab", profileID);
    default:
      return path.join(process.env.XDG_CONFIG_HOME || path.join(home, ".config"), "tailtab", profileID);
  }
}

function frame(obj) {
  const body = Buffer.from(JSON.stringify(obj));
  const len = Buffer.alloc(4);
  len.writeUInt32LE(body.length, 0);
  return Buffer.concat([len, body]);
}

// The manifest path and add-on id are what a Gecko browser passes; any
// arguments keep the binary in host mode.
const child = spawn(path.resolve(bin), ["smoke-test", "tailtab@stocist.dev"], {
  stdio: ["pipe", "pipe", "inherit"],
});

let buf = Buffer.alloc(0);
let sawProxy = false;
let done = false;

function finish(code, msg) {
  if (done) return;
  done = true;
  console.log(msg);
  clearTimeout(timer);
  child.stdin.end(); // the browser closing the port is the host's normal exit
  const kill = setTimeout(() => child.kill(), 5000);
  child.on("exit", (exit) => {
    clearTimeout(kill);
    console.log(`host exited with ${exit}`);
    fs.rmSync(stateDir(), { recursive: true, force: true });
    process.exitCode = code || (exit === 0 ? 0 : 1);
  });
}

child.stdout.on("data", (chunk) => {
  buf = Buffer.concat([buf, chunk]);
  while (buf.length >= 4) {
    const n = buf.readUInt32LE(0);
    if (buf.length < 4 + n) return;
    const ev = JSON.parse(buf.subarray(4, 4 + n).toString());
    buf = buf.subarray(4 + n);
    // Never print credentials: the proxy token, or the one-time login link.
    const { proxyToken, authURL, ...shown } = ev;
    if (authURL) shown.authURL = "<redacted>";
    console.log("event:", JSON.stringify(shown));
    if (ev.event === "error" && ev.error) {
      finish(1, `FAIL: host reported an error: ${ev.error}`);
      return;
    }
    if (ev.proxyPort > 0) sawProxy = true;
    // NeedsLogin alone only says there is no node key yet. A login URL is
    // minted by the coordination server, so it proves the round trip.
    if (sawProxy && ev.authURL) {
      finish(0, `OK: proxy listening on ${ev.proxyPort}, control plane issued a login URL`);
      return;
    }
  }
});

child.on("error", (err) => finish(1, `FAIL: could not start host: ${err}`));
child.on("exit", (code) => {
  if (!done) {
    done = true;
    console.log(`FAIL: host exited early with ${code}`);
    clearTimeout(timer);
    process.exitCode = 1;
  }
});

const timer = setTimeout(() => {
  finish(1, `FAIL: no login URL within ${timeoutMs} ms (proxy seen: ${sawProxy})`);
}, timeoutMs);

child.stdin.write(frame({ cmd: "init", profileID, browser: "smoke" }));
