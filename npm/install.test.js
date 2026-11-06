"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { artifactFor, parseChecksums } = require("./install");

test("maps Node targets to release assets", () => {
  assert.equal(artifactFor("linux", "x64", "0.7.0"), "draftcat-v0.7.0-linux-x64.gz");
  assert.equal(artifactFor("darwin", "arm64", "0.7.0"), "draftcat-v0.7.0-darwin-arm64.gz");
  assert.equal(artifactFor("win32", "x64", "0.7.0"), "draftcat-v0.7.0-win32-x64.exe.gz");
});

test("rejects unsupported targets", () => {
  assert.throws(() => artifactFor("freebsd", "x64"), /Unsupported platform/);
  assert.throws(() => artifactFor("linux", "ia32"), /Unsupported platform/);
});

test("reads GNU and binary-style checksum lines", () => {
  const a = "a".repeat(64);
  const b = "b".repeat(64);
  const checksums = parseChecksums(`${a}  draftcat-a.gz\n${b} *draftcat-b.gz\n`);
  assert.equal(checksums.get("draftcat-a.gz"), a);
  assert.equal(checksums.get("draftcat-b.gz"), b);
});

const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const http = require("node:http");
const crypto = require("node:crypto");
const zlib = require("node:zlib");
const { spawn, spawnSync } = require("node:child_process");

// Run real postinstall and launcher processes; substitute only transport and
// filesystem faults. Test deadlines are accelerated in the child process.
async function installFixture(t, options = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "draftcat-install-test-"));
  const npmDir = path.join(dir, "npm"), binDir = path.join(npmDir, "bin");
  fs.mkdirSync(binDir, { recursive: true });
  const executable = process.platform === "win32" ? "draftcat.exe" : "draftcat";
  const destination = path.join(binDir, executable);
  const previous = Buffer.from('#!/bin/sh\nprintf "previous-version\\n"\n');
  const replacement = Buffer.from('#!/bin/sh\nprintf "replacement-version\\n"\n');
  fs.writeFileSync(destination, previous, { mode: 0o755 });
  for (const name of ["install.js", "draftcat.js"]) fs.copyFileSync(path.join(__dirname, name), path.join(npmDir, name));
  const { version } = require("../package.json");
  fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify({ version }));
  const archive = options.invalidGzip ? Buffer.from("not gzip") : zlib.gzipSync(replacement);
  const hash = options.badChecksum ? "0".repeat(64) : crypto.createHash("sha256").update(archive).digest("hex");
  const sums = Buffer.from(`${hash}  ${artifactFor(process.platform, process.arch, version)}\n`);
  const timers = new Set();
  let redirected = false;
  const later = (fn, ms) => { const timer = setTimeout(fn, ms); timers.add(timer); };
  const server = http.createServer((req, res) => {
    if (req.url.endsWith("/SHA256SUMS")) { res.end(sums); return; }
    if (options.network === "insecure-redirect") {
      res.writeHead(302, { Location: "http://example.test/asset" }); res.end(); return;
    }
    if (options.network === "redirect-loop" || options.network === "redirect-five") {
      const hop = Number((/\/hop\/(\d+)$/.exec(req.url) || [null, 0])[1]);
      if (options.network === "redirect-loop" || hop < 5) {
        res.writeHead(302, { Location: `https://example.test/hop/${hop + 1}` }); res.end(); return;
      }
    }
    if (options.network === "http-error") { res.writeHead(503); res.end("unavailable"); return; }
    if (options.network === "oversize") {
      let remaining = 128 * 1024 * 1024 + 1;
      const chunk = Buffer.alloc(1024 * 1024);
      res.writeHead(200, { "Content-Length": remaining });
      const send = () => {
        while (remaining > 0) {
          const size = Math.min(remaining, chunk.length);
          remaining -= size;
          if (!res.write(chunk.subarray(0, size))) { res.once("drain", send); return; }
        }
        res.end();
      };
      send(); return;
    }
    if (options.network === "idle") return;
    if (options.network === "redirect-open" && req.url !== "/asset") {
      redirected = true;
      res.writeHead(302, { Location: "https://example.test/asset", "Content-Length": 1000 });
      res.write("unfinished redirect body");
      later(() => res.destroy(), 40);
      return;
    }
    if (options.network === "redirect-open") { later(() => res.end(archive), 100); return; }
    if (options.network === "redirect-drip" && req.url !== "/asset") {
      later(() => { redirected = true; res.writeHead(302, { Location: "https://example.test/asset" }); res.end(); }, 140);
      return;
    }
    res.writeHead(200, { "Content-Length": archive.length });
    if (options.network === "abort") { res.write(archive.subarray(0, 5)); later(() => res.destroy(), 20); return; }
    if (options.network === "redirect-drip") {
      let offset = 1;
      res.write(archive.subarray(0, offset));
      const timer = setInterval(() => res.write(archive.subarray(offset, ++offset)), 25);
      timers.add(timer);
      later(() => { clearInterval(timer); res.end(archive.subarray(offset)); }, 210);
      return;
    }
    res.end(archive);
  });
  t.after(async () => {
    for (const timer of timers) clearTimeout(timer);
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const preload = path.join(dir, "preload.cjs");
  fs.writeFileSync(preload, `
    const fs = require("node:fs"), path = require("node:path");
    const http = require("node:http"), https = require("node:https");
    const options = JSON.parse(process.env.DRAFTCAT_TEST_OPTIONS);
    const timer = global.setTimeout;
    global.setTimeout = (fn, ms, ...args) => timer(fn, ms === 120000 && options.network !== "oversize" ? 240 : ms, ...args);
    https.get = (url, opts, callback) => {
      const request = http.get(process.env.DRAFTCAT_TEST_ORIGIN + new URL(url).pathname, opts, callback);
      if (options.network === "idle") {
        const timeout = request.setTimeout.bind(request);
        request.setTimeout = (ms, fn) => timeout(ms === 30000 ? 80 : ms, fn);
      }
      return request;
    };
    const binDir = path.join(__dirname, "npm", "bin");
    const inBin = file => typeof file === "string" && path.dirname(file) === binDir;
    const write = fs.writeFileSync, open = fs.openSync;
    let stagedDescriptor;
    fs.openSync = function(file, flags) {
      if (inBin(file) && flags === "wx") {
        if (options.fault === "collision") write(file, "unrelated staging file");
        stagedDescriptor = open.apply(this, arguments);
        return stagedDescriptor;
      }
      return open.apply(this, arguments);
    };
    fs.writeFileSync = function(file, data, opts) {
      if (options.fault === "write" && (file === stagedDescriptor || inBin(file))) {
        write(file, Buffer.from(data).subarray(0, 10), opts);
        const error = new Error("injected partial write failure"); error.code = "ENOSPC"; throw error;
      }
      return write.apply(this, arguments);
    };
    const chmod = fs.chmodSync;
    fs.chmodSync = function(file) {
      if (options.fault === "chmod" && inBin(file)) throw new Error("injected chmod failure");
      return chmod.apply(this, arguments);
    };
    const rename = fs.renameSync;
    fs.renameSync = function(from, to) {
      if (options.fault === "rename" && inBin(to)) throw new Error("injected rename failure");
      return rename.apply(this, arguments);
    };
    const remove = fs.rmSync;
    fs.rmSync = function(file) {
      if (options.cleanupFailure && inBin(file)) throw new Error("injected cleanup failure");
      return remove.apply(this, arguments);
    };
  `);
  const child = spawn(process.execPath, ["--require", preload, path.join(npmDir, "install.js")], {
    env: { ...process.env, DRAFTCAT_TEST_OPTIONS: JSON.stringify(options), DRAFTCAT_TEST_ORIGIN: `http://127.0.0.1:${server.address().port}` },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "", stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk; });
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const watchdog = setTimeout(() => child.kill("SIGKILL"), options.network === "oversize" ? 10000 : 3000);
  const exit = await new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (code, signal) => resolve({ code, signal }));
  });
  clearTimeout(watchdog);
  return {
    ...exit, stdout, stderr, redirected, previous, replacement, actual: fs.readFileSync(destination),
    temporaryFiles: fs.readdirSync(binDir).filter((name) => name !== executable),
    temporaryContents: fs.readdirSync(binDir).filter((name) => name !== executable).map((name) => fs.readFileSync(path.join(binDir, name), "utf8")),
    launch: () => spawnSync(process.execPath, [path.join(npmDir, "draftcat.js"), "--version"], { encoding: "utf8" }),
  };
}

function assertPrevious(result) {
  assert.equal(result.code, 1, result.stderr);
  assert.equal(result.signal, null);
  assert.deepEqual(result.actual, result.previous);
  if (process.platform !== "win32") assert.equal(result.launch().stdout, "previous-version\n");
}

test("postinstall replaces the executable after a verified download", async (t) => {
  const result = await installFixture(t);
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(result.actual, result.replacement);
  assert.match(result.stdout, /Installed Draftcat v/);
  assert.deepEqual(result.temporaryFiles, []);
  if (process.platform !== "win32") assert.equal(result.launch().stdout, "replacement-version\n");
});

test("an interrupted response fails installation and preserves the executable", async (t) => {
  const result = await installFixture(t, { network: "abort" });
  assertPrevious(result);
  assert.match(result.stderr, /Unable to install Draftcat:.*(interrupted|completion|aborted)/);
  assert.deepEqual(result.temporaryFiles, []);
});

test("one elapsed deadline covers redirects and continually arriving bytes", async (t) => {
  const result = await installFixture(t, { network: "redirect-drip" });
  assertPrevious(result);
  assert.equal(result.redirected, true);
  assert.match(result.stderr, /download exceeded 120 seconds/);
});

test("the inactivity timeout still rejects a stalled connection", async (t) => {
  const result = await installFixture(t, { network: "idle" });
  assertPrevious(result);
  assert.match(result.stderr, /download timed out/);
});

for (const fault of ["write", "chmod", "rename"]) {
  test(`a ${fault} failure preserves the old executable and removes staging files`, { skip: fault === "chmod" && process.platform === "win32" }, async (t) => {
    const result = await installFixture(t, { fault });
    assertPrevious(result);
    assert.match(result.stderr, /injected .*failure/);
    assert.deepEqual(result.temporaryFiles, []);
  });
}

test("a cleanup failure reports both errors without touching the old executable", async (t) => {
  const result = await installFixture(t, { fault: "write", cleanupFailure: true });
  assertPrevious(result);
  assert.match(result.stderr, /partial write failure.*cleanup failed.*cleanup failure/);
  assert.equal(result.temporaryFiles.length, 1);
});

for (const option of ["badChecksum", "invalidGzip"]) {
  test(`${option} fails before replacing the executable`, async (t) => {
    const result = await installFixture(t, { [option]: true });
    assertPrevious(result);
    assert.deepEqual(result.temporaryFiles, []);
  });
}


test("an unfinished redirect body cannot fail the next successful download", async (t) => {
  const result = await installFixture(t, { network: "redirect-open" });
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.redirected, true);
  assert.deepEqual(result.actual, result.replacement);
  assert.deepEqual(result.temporaryFiles, []);
});

test("an exclusive staging-file collision preserves the unrelated file", async (t) => {
  const result = await installFixture(t, { fault: "collision" });
  assertPrevious(result);
  assert.match(result.stderr, /EEXIST/);
  assert.deepEqual(result.temporaryContents, ["unrelated staging file"]);
});

for (const [network, diagnostic] of [
  ["insecure-redirect", /Refusing a non-HTTPS release redirect/],
  ["redirect-loop", /Too many redirects/],
  ["http-error", /Download failed with HTTP 503/],
  ["oversize", /release asset is unexpectedly large/],
]) {
  test(`${network} fails before replacing the executable`, async (t) => {
    const result = await installFixture(t, { network });
    assertPrevious(result);
    assert.match(result.stderr, diagnostic);
    assert.deepEqual(result.temporaryFiles, []);
  });
}

test("five HTTPS redirects still allow a verified installation", async (t) => {
  const result = await installFixture(t, { network: "redirect-five" });
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(result.actual, result.replacement);
  assert.deepEqual(result.temporaryFiles, []);
});
