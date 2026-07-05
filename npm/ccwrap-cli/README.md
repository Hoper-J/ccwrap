# ccwrap (npm distribution)

Per-session launcher and local instrumentation proxy for Claude Code that
preserves the client's native TLS fingerprint. It installs a local CA and
man-in-the-middles the launched process for inspection/instrumentation.

```sh
npm install -g ccwrap-cli   # provides the `ccwrap` command
# or
npx ccwrap-cli version
```

This package is a thin launcher: the platform binary ships in a per-OS/arch
optional dependency (`@hoper-j/ccwrap-cli-<os>-<arch>`) and npm installs only the one
matching your machine (macOS/Linux, x64/arm64). Other install methods (`install.sh`, `go install`, prebuilt GitHub Release binaries) are documented in
the [project README](https://github.com/Hoper-J/ccwrap).

## Disclaimer

Independent community project — **not affiliated with, endorsed by, or sponsored
by Anthropic**; "Claude" and "Anthropic" are trademarks of their respective
owners. For interoperability/instrumentation of software you are authorized to
run; you are responsible for complying with the terms of any service you connect
to. See [SECURITY.md](https://github.com/Hoper-J/ccwrap/blob/main/SECURITY.md).
