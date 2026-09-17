#!/usr/bin/env node

"use strict";

const crypto = require("node:crypto");
const fs = require("node:fs");
const https = require("node:https");
const path = require("node:path");
const zlib = require("node:zlib");
const { version } = require("../package.json");

const repository = "renezander030/draftcat";
const maximumDownloadBytes = 128 * 1024 * 1024;
const supported = new Set([
  "darwin-arm64",
  "darwin-x64",
  "linux-arm64",
  "linux-x64",
  "win32-arm64",
  "win32-x64",
]);

function artifactFor(platform, arch, releaseVersion = version) {
  const target = `${platform}-${arch}`;
  if (!supported.has(target)) {
    throw new Error(`Unsupported platform: ${target}`);
  }
  const extension = platform === "win32" ? ".exe" : "";
  return `draftcat-v${releaseVersion}-${target}${extension}.gz`;
}

function parseChecksums(contents) {
  const checksums = new Map();
  for (const line of contents.trim().split(/\r?\n/)) {
    const match = /^([a-f0-9]{64})\s+\*?(.+)$/.exec(line.trim());
    if (match) {
      checksums.set(match[2], match[1]);
    }
  }
  return checksums;
}

function download(url, redirects = 0) {
  if (redirects > 5) {
    return Promise.reject(new Error("Too many redirects while downloading Draftcat"));
  }
  return new Promise((resolve, reject) => {
    const request = https.get(url, {
      headers: { "User-Agent": `draftcat-npm/${version}` },
    }, (response) => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        const next = new URL(response.headers.location, url);
        if (next.protocol !== "https:") {
          reject(new Error("Refusing a non-HTTPS release redirect"));
          return;
        }
        download(next, redirects + 1).then(resolve, reject);
        return;
      }
      if (response.statusCode !== 200) {
        response.resume();
        reject(new Error(`Download failed with HTTP ${response.statusCode}`));
        return;
      }
      const chunks = [];
      let size = 0;
      response.on("data", (chunk) => {
        size += chunk.length;
        if (size > maximumDownloadBytes) {
          request.destroy(new Error("Draftcat release asset is unexpectedly large"));
          return;
        }
        chunks.push(chunk);
      });
      response.on("end", () => resolve(Buffer.concat(chunks)));
    });
    request.on("error", reject);
    request.setTimeout(30_000, () => request.destroy(new Error("Draftcat download timed out")));
  });
}

async function install() {
  const artifact = artifactFor(process.platform, process.arch);
  const releaseBase = `https://github.com/${repository}/releases/download/v${version}`;
  const [archive, checksumFile] = await Promise.all([
    download(`${releaseBase}/${artifact}`),
    download(`${releaseBase}/SHA256SUMS`),
  ]);

  const expected = parseChecksums(checksumFile.toString("utf8")).get(artifact);
  if (!expected) {
    throw new Error(`No checksum was published for ${artifact}`);
  }
  const actual = crypto.createHash("sha256").update(archive).digest("hex");
  if (!crypto.timingSafeEqual(Buffer.from(actual), Buffer.from(expected))) {
    throw new Error(`Checksum verification failed for ${artifact}`);
  }

  const executable = process.platform === "win32" ? "draftcat.exe" : "draftcat";
  const binaryDirectory = path.join(__dirname, "bin");
  const destination = path.join(binaryDirectory, executable);
  fs.mkdirSync(binaryDirectory, { recursive: true });
  fs.writeFileSync(destination, zlib.gunzipSync(archive), { mode: 0o755 });
  if (process.platform !== "win32") {
    fs.chmodSync(destination, 0o755);
  }
  console.log(`Installed Draftcat v${version} for ${process.platform}-${process.arch}`);
}

if (require.main === module) {
  install().catch((error) => {
    console.error(`Unable to install Draftcat: ${error.message}`);
    process.exit(1);
  });
}

module.exports = { artifactFor, parseChecksums };
