# Security Policy

## Reporting a vulnerability

Please report security issues **privately** via GitHub's **"Report a vulnerability"**
button on the repository's *Security* tab (Security Advisories) — not in public
issues or pull requests. Include reproduction steps and the affected version
(`ccwrap version`). Please allow a reasonable window for a fix before any public
disclosure.

## What this tool does (handle its provenance with care)

ccwrap runs a local man-in-the-middle TLS proxy: it installs a local Certificate
Authority into your trust store, terminates TLS for the launched child process,
and re-originates the upstream connection while preserving the child's native TLS
fingerprint. Because it installs a CA and intercepts TLS, its supply-chain
integrity matters:

- Install only from the official channels: GitHub Releases,
  npm `ccwrap-cli`, or `go install github.com/Hoper-J/ccwrap/cmd/ccwrap`.
- Verify the published `checksums.txt` against its cosign signature before
  trusting a binary download. The signature is keyless (Sigstore/OIDC — there
  is no public key to distribute), so verification pins the signing identity:

  ```bash
  cosign verify-blob checksums.txt \
    --certificate checksums.txt.pem \
    --signature checksums.txt.sig \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/Hoper-J/ccwrap/\.github/workflows/release\.yml@refs/tags/v.*$'
  # then check your download against the now-trusted checksums:
  sha256sum -c checksums.txt --ignore-missing
  ```

  The npm packages are published with build provenance (`npm audit signatures`).
  Each release archive also ships an attached SBOM (syft-generated) for
  dependency / vulnerability scanning with tools like grype or syft.

## Disclaimer

- **Not affiliated.** ccwrap is an independent, community project. It is **not
  affiliated with, endorsed by, or sponsored by Anthropic**. "Claude",
  "Claude Code", and "Anthropic" are trademarks of their respective owners, used
  here only for identification and interoperability.
- **Authorized use only.** ccwrap is intended for interoperability, debugging,
  and instrumentation of software you are authorized to run. **You are solely
  responsible for complying with the terms of service of any provider you connect
  to, and with applicable law.** Use it only where you have the right to do so.
- **No warranty.** Provided "as is" under the MIT License, without warranty of any
  kind.
