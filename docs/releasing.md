# Releasing Draftcat

Draftcat is a Go CLI and service. Version tags publish six native binaries with checksums to GitHub Releases, a container image to GHCR, and an npm installer that downloads the matching binary. The Go module uses the same version tag. This repository does not provide a Python package or an MCP protocol server to publish to PyPI or the MCP Registry.

## Review and version

Merge the reviewed release PR only after its checks pass. Keep `version.go`, `package.json`, and both version entries in `package-lock.json` consistent with the intended tag. The release workflow verifies this before building. Create the version tag on the reviewed merge commit; pushing `v*` triggers `.github/workflows/release.yml`.

The workflow runs lean and voice tests, validates the configuration, tests the npm installer, and checks the package contents. It builds Linux, macOS, and Windows binaries for x64 and arm64. The npm job waits for the GitHub assets and smoke-tests their installer before publishing.

## npm authentication

The npm job has `id-token: write` and runs on a GitHub-hosted runner with Node 24. For trusted publishing, the `draftcat` package settings on npm must authorize GitHub owner `renezander030`, repository `draftcat`, workflow filename `release.yml`, and direct `npm publish`. No GitHub environment is configured in this job. npm requires CLI 11.5.1 or newer for this route. See the [npm trusted publishing documentation](https://docs.npmjs.com/trusted-publishers/).

The workflow also accepts a configured `NPM_TOKEN` repository secret as a token fallback. A successful build or GitHub release does not establish npm publication: inspect the npm job separately. If it fails with `ENEEDAUTH`, verify the exact package-side trusted publisher settings or use an authenticated maintainer account. Do not publish a second copy when the version already exists.

## Verify the published version

Check that the release has all six compressed assets and `SHA256SUMS`, the container version tag exists, and npm reports the intended version. Install the exact npm version in a clean prefix and run `draftcat --version` to verify the downloaded binary. Confirm the Go module tag is discoverable through the Go proxy. Retain the release workflow URL and report any destination that failed independently.
