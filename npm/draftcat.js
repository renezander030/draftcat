#!/usr/bin/env node

"use strict";

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const executable = process.platform === "win32" ? "draftcat.exe" : "draftcat";
const binary = path.join(__dirname, "bin", executable);

if (!fs.existsSync(binary)) {
  console.error("Draftcat is not installed. Reinstall the package to download its binary.");
  process.exit(1);
}

const result = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`Unable to start Draftcat: ${result.error.message}`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
} else {
  process.exit(result.status === null ? 1 : result.status);
}
