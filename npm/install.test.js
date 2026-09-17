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
