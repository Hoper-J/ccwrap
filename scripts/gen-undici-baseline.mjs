// gen-undici-baseline.mjs — capture a REAL undici ClientHello as committed test data.
//
// Run ONLY intentionally (NOT in CI). It runs Node's built-in undici (fetch)
// against a throwaway local TCP listener; the listener records the very first
// bytes undici writes — the raw TLS ClientHello record(s) — and commits them.
// We deliberately use the hostname "localhost" (never 127.0.0.1) so undici emits
// the SNI extension, matching what CC's runtime sends to api.anthropic.com.
//
// Outputs (commit these):
//   internal/supervisor/testdata/undici_clienthello.bin   raw ClientHello record(s)
//   internal/supervisor/testdata/undici_baseline.json      provenance metadata
//
// The handshake never completes (the listener is not a TLS server); we only need
// the ClientHello, so the fetch is expected to reject and is swallowed.
//
// Regenerate when ccwrap's target Node/undici moves (and re-run the offline
// parity test in internal/supervisor/nativetls_baseline_test.go).
//
//   node scripts/gen-undici-baseline.mjs

import net from "node:net";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const outDir = resolve(here, "..", "internal", "supervisor", "testdata");
const binPath = resolve(outDir, "undici_clienthello.bin");
const jsonPath = resolve(outDir, "undici_baseline.json");

mkdirSync(outDir, { recursive: true });

function captureClientHello() {
  return new Promise((resolvePromise, rejectPromise) => {
    const chunks = [];
    let settled = false;

    const finish = (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(guard);
      try {
        server.close();
      } catch {}
      if (err) rejectPromise(err);
      else resolvePromise(Buffer.concat(chunks));
    };

    const server = net.createServer((sock) => {
      // The first inbound bytes on the TCP connection are the TLS ClientHello
      // record(s). Capture the first data event (a single ClientHello fits in
      // one segment over loopback), then tear down — the handshake will not and
      // need not complete.
      sock.on("data", (buf) => {
        chunks.push(buf);
        // Give the kernel a beat in case the hello spans multiple records, then
        // finish on the next tick.
        setTimeout(() => finish(null), 50);
      });
      sock.on("error", () => {});
    });

    server.on("error", finish);

    const guard = setTimeout(
      () => finish(new Error("timed out waiting for the ClientHello")),
      5000,
    );

    server.listen(0, "127.0.0.1", async () => {
      const { port } = server.address();
      // Hostname (not 127.0.0.1) so undici includes the SNI extension.
      await fetch(`https://localhost:${port}/`).catch(() => {});
    });
  });
}

const raw = await captureClientHello();

if (raw.length < 64 || raw[0] !== 0x16 || raw[1] !== 0x03) {
  console.error(
    `captured ${raw.length} bytes; first two = 0x${raw[0]?.toString(16)} 0x${raw[1]?.toString(16)} ` +
      "(expected a TLS handshake record starting 0x16 0x03). NOT writing outputs.",
  );
  process.exit(1);
}

writeFileSync(binPath, raw);

// Preserve any existing ja3/ja4/peetprint so a failed cross-check below leaves
// them intact rather than dropping them from the rewritten JSON.
let prior = {};
try {
  prior = JSON.parse(readFileSync(jsonPath, "utf8"));
} catch {}

const baseline = {
  node_version: process.version,
  undici_version: process.versions.undici,
  captured_at: new Date().toISOString(),
  note: "raw ClientHello for the offline utls parity test; ja3/ja4/peetprint are Go-computed (internal/tlsfp) AND tls.peet.ws-cross-checked on this node/undici; regenerate together when ccwrap's target Node/undici moves",
};
for (const k of ["ja3", "ja4", "peetprint"]) {
  if (typeof prior[k] === "string") baseline[k] = prior[k];
}

// Cross-check the live fingerprint via tls.peet.ws using the SAME undici that
// produced the .bin above. This is run intentionally (non-CI), so reaching a
// live host is fine. Guarded so a network failure only WARNS and leaves any
// existing ja3/ja4/peetprint values intact (the Go-side internal/tlsfp anchor
// test is the offline source of truth; these are the external cross-check).
try {
  const resp = await fetch("https://tls.peet.ws/api/all");
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  const peet = await resp.json();
  const tls = peet?.tls ?? {};
  const ja3 = tls.ja3_hash;
  const ja4 = tls.ja4;
  const peetprint = tls.peetprint_hash;
  if (ja3) baseline.ja3 = ja3;
  if (ja4) baseline.ja4 = ja4;
  if (peetprint) baseline.peetprint = peetprint;
  console.log(
    `tls.peet.ws cross-check: ja3=${ja3} ja4=${ja4} peetprint=${peetprint}`,
  );
} catch (err) {
  console.warn(
    `WARNING: tls.peet.ws cross-check failed (${err?.message ?? err}); ` +
      "leaving any existing ja3/ja4/peetprint in undici_baseline.json intact.",
  );
}

writeFileSync(jsonPath, JSON.stringify(baseline, null, 2) + "\n");

console.log(
  `wrote ${raw.length} bytes to ${binPath} (first bytes 0x${raw[0].toString(16)} 0x${raw[1].toString(16)})`,
);
console.log(`wrote baseline ${JSON.stringify(baseline)}`);
