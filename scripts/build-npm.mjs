// build-npm.mjs — stage the npm packages for a release from GoReleaser output.
//
// Reads the cross-built archives in dist/ (produced by `goreleaser release` or
// `goreleaser release --snapshot`), extracts each platform binary into its
// per-platform npm package, and stamps the version into every package.json
// (main + 4 platform packages + the main package's optionalDependencies).
//
// It does NOT publish — it prints the `npm publish` commands to run when you are
// ready. Publish the platform packages BEFORE the main package so the main
// package's optionalDependencies resolve.
//
//   node scripts/build-npm.mjs <version>      # e.g. 0.2.0  (no leading v)
//
// Prereq: `goreleaser release --snapshot --clean --skip=publish,sign,sbom`
// (or a real release) has populated dist/.
import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const version = process.argv[2];
if (!version) {
  console.error("usage: node scripts/build-npm.mjs <version>   (e.g. 0.2.0)");
  process.exit(1);
}

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");
const dist = resolve(root, "dist");

const PLATFORMS = [
  { os: "darwin", goarch: "arm64" },
  { os: "darwin", goarch: "amd64" },
  { os: "linux", goarch: "arm64" },
  { os: "linux", goarch: "amd64" },
];

function setVersion(pkgJsonPath, mutate) {
  const pkg = JSON.parse(readFileSync(pkgJsonPath, "utf8"));
  pkg.version = version;
  if (mutate) mutate(pkg);
  writeFileSync(pkgJsonPath, JSON.stringify(pkg, null, 2) + "\n");
}

// 1. Per-platform packages: extract the binary + stamp the version.
for (const { os, goarch } of PLATFORMS) {
  const archive = resolve(dist, `ccwrap_${version}_${os}_${goarch}.tar.gz`);
  if (!existsSync(archive)) {
    console.error(`missing archive: ${archive}\n(run goreleaser first)`);
    process.exit(1);
  }
  const pkgDir = resolve(root, "npm", "platforms", `${os}-${goarch}`);
  const binDir = resolve(pkgDir, "bin");
  mkdirSync(binDir, { recursive: true });
  // Extract just the ccwrap binary out of the archive into the package's bin/.
  execFileSync("tar", ["-xzf", archive, "-C", binDir, "ccwrap"]);
  chmodSync(resolve(binDir, "ccwrap"), 0o755);
  setVersion(resolve(pkgDir, "package.json"));
  console.log(`staged @hoper-j/ccwrap-${os}-${goarch}@${version}`);
}

// 2. Main package: stamp version + pin optionalDependencies to the same version.
setVersion(resolve(root, "npm", "ccwrap", "package.json"), (pkg) => {
  for (const k of Object.keys(pkg.optionalDependencies || {})) {
    pkg.optionalDependencies[k] = version;
  }
});
console.log(`staged @hoper-j/ccwrap@${version}`);

// 3. Main package README: generate it from the whole project README so the npm
//    page mirrors GitHub (overview + screenshots + the folded reference
//    sections) instead of a bare stub. npm doesn't serve the repo's assets/,
//    so relative images/links are rewritten to absolute GitHub URLs; an
//    npm-distribution note + disclaimer are appended. Overwrites the committed
//    placeholder (restored by `git checkout` after a local rehearsal).
const SLUG = "Hoper-J/ccwrap";
const RAW = `https://raw.githubusercontent.com/${SLUG}/main`;
const BLOB = `https://github.com/${SLUG}/blob/main`;
const fullReadme = readFileSync(resolve(root, "README.md"), "utf8");
const npmReadme =
  fullReadme.trimEnd()
    .replaceAll("](assets/", `](${RAW}/assets/`)
    .replaceAll("](docs/", `](${RAW}/docs/`)
    .replaceAll("](README.zh-CN.md)", `](${BLOB}/README.zh-CN.md)`)
    .replaceAll("](LICENSE)", `](${BLOB}/LICENSE)`) +
  `

---

## npm distribution

This package is a thin launcher: the actual binary ships in a per-OS/arch optional dependency (\`@hoper-j/ccwrap-<os>-<arch>\`) and npm installs only the one matching your machine (macOS / Linux, x64 / arm64). \`npm install -g @hoper-j/ccwrap\` is all you need.

Full documentation — commands, flags, profiles, the security model, and more — lives in the [project README](https://github.com/${SLUG}).

## Disclaimer

Independent community project — **not affiliated with, endorsed by, or sponsored by Anthropic**; "Claude" and "Anthropic" are trademarks of their respective owners. For interoperability / instrumentation of software you are authorized to run; you are responsible for complying with the terms of any service you connect to. See [SECURITY.md](https://github.com/${SLUG}/blob/main/SECURITY.md).
`;
writeFileSync(resolve(root, "npm", "ccwrap", "README.md"), npmReadme);
console.log("generated npm/ccwrap/README.md from README.md front matter");

console.log(
  [
    "",
    "Staged. To publish (platform packages FIRST, then the launcher):",
    ...PLATFORMS.map(
      ({ os, goarch }) =>
        `  npm publish --access public ./npm/platforms/${os}-${goarch}`
    ),
    "  npm publish --access public ./npm/ccwrap",
    "",
  ].join("\n")
);
