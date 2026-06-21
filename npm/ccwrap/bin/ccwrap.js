#!/usr/bin/env node
// Thin launcher for the ccwrap CLI distributed over npm. The actual binary ships
// in a per-platform optional dependency (@hoper-j/ccwrap-<os>-<arch>); npm installs
// only the one matching the host's os/cpu. This shim resolves that package's binary
// and execs it, forwarding argv, stdio, and the exit code.
"use strict";

const { spawnSync } = require("node:child_process");
const path = require("node:path");
const fs = require("node:fs");

function resolveBinary() {
  const platform = process.platform; // 'darwin' | 'linux'
  const archMap = { x64: "amd64", arm64: "arm64" };
  const goarch = archMap[process.arch];

  if ((platform !== "darwin" && platform !== "linux") || !goarch) {
    throw new Error(
      `ccwrap: unsupported platform ${platform}/${process.arch} ` +
        "(supported: darwin/linux on x64/arm64)"
    );
  }

  const pkg = `@hoper-j/ccwrap-${platform}-${goarch}`;
  let pkgJson;
  try {
    pkgJson = require.resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `ccwrap: the platform package ${pkg} is not installed.\n` +
        "If you installed with --no-optional or --omit=optional, reinstall without it."
    );
  }
  const bin = path.join(path.dirname(pkgJson), "bin", "ccwrap");
  if (!fs.existsSync(bin)) {
    throw new Error(`ccwrap: binary missing from ${pkg} (expected ${bin})`);
  }
  return bin;
}

let bin;
try {
  bin = resolveBinary();
} catch (err) {
  console.error(err && err.message ? err.message : String(err));
  process.exit(1);
}

const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(result.error.message);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
