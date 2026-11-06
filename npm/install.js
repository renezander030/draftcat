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

function download(url) {
  return new Promise((resolve, reject) => {
    let activeRequest;
    let settled = false;
    const deadline = setTimeout(() => finish(new Error("Draftcat download exceeded 120 seconds")), 120_000);

    function finish(error, data) {
      if (settled) return;
      settled = true;
      clearTimeout(deadline);
      if (activeRequest) activeRequest.setTimeout(0);
      if (error) {
        if (activeRequest) activeRequest.destroy();
        reject(error);
      } else {
        resolve(data);
      }
    }

    function requestAsset(target, redirects) {
      if (settled) return;
      if (redirects > 5) {
        finish(new Error("Too many redirects while downloading Draftcat"));
        return;
      }
      try {
        let followed = false;
        const request = https.get(target, {
          headers: { "User-Agent": `draftcat-npm/${version}` },
        }, (response) => {
          if (settled) { response.destroy(); return; }
          response.on("error", (error) => { if (!followed) finish(error); });
          response.on("aborted", () => { if (!followed) finish(new Error("Draftcat download was interrupted")); });
          if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
            let next;
            try {
              next = new URL(response.headers.location, target);
            } catch (error) {
              finish(error);
              return;
            }
            if (next.protocol !== "https:") {
              finish(new Error("Refusing a non-HTTPS release redirect"));
              return;
            }
            // The redirect body is irrelevant; retire its handlers before
            // closing it so an old hop cannot fail the new request.
            followed = true;
            request.setTimeout(0);
            response.destroy();
            requestAsset(next, redirects + 1);
            return;
          }
          if (response.statusCode !== 200) {
            response.resume();
            finish(new Error(`Download failed with HTTP ${response.statusCode}`));
            return;
          }
          const chunks = [];
          let size = 0;
          response.on("data", (chunk) => {
            size += chunk.length;
            if (size > maximumDownloadBytes) {
              finish(new Error("Draftcat release asset is unexpectedly large"));
              return;
            }
            chunks.push(chunk);
          });
          response.on("end", () => {
            if (!response.complete) {
              finish(new Error("Draftcat download ended before completion"));
              return;
            }
            finish(null, Buffer.concat(chunks));
          });
          response.on("close", () => {
            if (!response.complete) finish(new Error("Draftcat download closed before completion"));
          });
        });
        activeRequest = request;
        request.on("error", (error) => { if (!followed) finish(error); });
        request.setTimeout(30_000, () => { if (!followed) finish(new Error("Draftcat download timed out")); });
      } catch (error) {
        finish(error);
      }
    }

    requestAsset(url, 0);
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
  const contents = zlib.gunzipSync(archive);
  const temporary = path.join(binaryDirectory, `.${executable}.${process.pid}.${crypto.randomBytes(8).toString("hex")}.tmp`);
  fs.mkdirSync(binaryDirectory, { recursive: true });
  let descriptor;
  let created = false;
  try {
    descriptor = fs.openSync(temporary, "wx", 0o755);
    created = true;
    fs.writeFileSync(descriptor, contents);
    fs.closeSync(descriptor);
    descriptor = undefined;
    if (process.platform !== "win32") fs.chmodSync(temporary, 0o755);
    fs.renameSync(temporary, destination);
  } catch (error) {
    if (descriptor !== undefined) {
      try { fs.closeSync(descriptor); } catch { /* Preserve the original failure. */ }
    }
    if (created) {
      try {
        fs.rmSync(temporary, { force: true });
      } catch (cleanupError) {
        error.message += ` (temporary file cleanup failed: ${cleanupError.message})`;
      }
    }
    throw error;
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
