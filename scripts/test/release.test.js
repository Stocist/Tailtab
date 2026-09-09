"use strict";

const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { createHash } = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const workflow = fs.readFileSync(path.join(__dirname, "../../.github/workflows/release.yml"), "utf8")
  .replace(/\r\n/g, "\n");
const signing = "Sign the Firefox extension through AMO (unlisted)";
const manifest = "Firefox update manifest";
const publishing = "Publish the release";

function shellStep(name) {
  const step = workflow.split(/^      - name: /m).find((part) => part.startsWith(`${name}\n`));
  assert.ok(step, `Missing workflow step: ${name}`);
  const run = step.match(/^        run: \|\n((?:          .*\n|\n)+)/m);
  assert.ok(run, `Missing shell block: ${name}`);
  return run[1].replace(/^          /gm, "");
}

function fixture(t) {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "tailtab-release-"));
  t.after(() => fs.rmSync(cwd, { recursive: true, force: true }));
  fs.mkdirSync(path.join(cwd, "release"));
  fs.writeFileSync(path.join(cwd, "release/tailtab-firefox-1.2.3.zip"), "unsigned fixture");
  const env = {
    ...process.env,
    AMO_JWT_ISSUER: "",
    AMO_JWT_SECRET: "",
    TAILTAB_VERSION: "1.2.3",
    GITHUB_REF_NAME: "v1.2.3",
    GITHUB_REPOSITORY: "example/tailtab",
    TEST_NODE: process.execPath,
  };
  return {
    cwd,
    env,
    run(name, mocks = "") {
      // Never invoke real signing/publishing; hash real fixture bytes without GNU coreutils.
      const result = spawnSync("bash", ["--noprofile", "--norc", "-eo", "pipefail", "-c", `
npx() { : > signing-called; return 97; }
gh() { printf '%s\\n' "$@" > publish-args; }
sha256sum() {
  "$TEST_NODE" -e '
    const fs = require("node:fs");
    const { createHash } = require("node:crypto");
    const file = process.argv[1];
    console.log(createHash("sha256").update(fs.readFileSync(file)).digest("hex") + "  " + file);
  ' "$1"
}
${mocks}
${shellStep(name)}
`], { cwd, env, encoding: "utf8", timeout: 10000 });
      assert.ifError(result.error);
      assert.equal(result.signal, null, result.stderr);
      return result;
    },
  };
}

for (const [issuer, secret] of [["", ""], ["issuer", ""], ["", "secret"]]) {
  test(`signing rejects missing credentials (issuer=${!!issuer}, secret=${!!secret})`, (t) => {
    const f = fixture(t);
    f.env.AMO_JWT_ISSUER = issuer;
    f.env.AMO_JWT_SECRET = secret;
    const result = f.run(signing);
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /AMO_JWT_ISSUER and AMO_JWT_SECRET are required/);
    assert.equal(fs.existsSync(path.join(f.cwd, "signing-called")), false);
  });
}

for (const empty of [false, true]) {
  test(`manifest rejects ${empty ? "empty" : "missing"} versioned signed XPI`, (t) => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.cwd, "release/tailtab-0.0.1.xpi"), "wrong version");
    if (empty) fs.writeFileSync(path.join(f.cwd, "release/tailtab-1.2.3.xpi"), "");
    const result = f.run(manifest);
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /Missing or empty signed XPI/);
    assert.equal(fs.existsSync(path.join(f.cwd, "release/updates.json")), false);
  });

  test(`publishing rejects ${empty ? "empty" : "missing"} Firefox feed`, (t) => {
    const f = fixture(t);
    if (empty) fs.writeFileSync(path.join(f.cwd, "release/updates.json"), "");
    const result = f.run(publishing);
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /updates.json is required before publishing/);
    assert.equal(fs.existsSync(path.join(f.cwd, "publish-args")), false);
  });
}

test("signed fixture produces the version, tagged XPI URL, and post-sign hash before publishing", (t) => {
  const f = fixture(t);
  f.env.AMO_JWT_ISSUER = "issuer";
  f.env.AMO_JWT_SECRET = "secret";
  const signed = f.run(signing, `
npx() {
  mkdir -p release/xpi
  printf '%s' 'signed fixture' > release/xpi/amo-signed.xpi
}
`);
  assert.equal(signed.status, 0, signed.stderr);
  assert.equal(fs.readFileSync(path.join(f.cwd, "release/tailtab-1.2.3.xpi"), "utf8"), "signed fixture");
  const generated = f.run(manifest);
  assert.equal(generated.status, 0, generated.stderr);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(f.cwd, "release/updates.json"), "utf8")), {
    addons: {
      "tailtab@stocist.dev": {
        updates: [{
          version: "1.2.3",
          update_link: "https://github.com/example/tailtab/releases/download/v1.2.3/tailtab-1.2.3.xpi",
          update_hash: `sha256:${createHash("sha256").update("signed fixture").digest("hex")}`,
        }],
      },
    },
  });
  const published = f.run(publishing);
  assert.equal(published.status, 0, published.stderr);
  assert.ok(fs.readFileSync(path.join(f.cwd, "publish-args"), "utf8").split("\n").includes("release/updates.json"));
});
