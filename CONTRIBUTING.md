# Contributing to ccwrap

English · [简体中文](CONTRIBUTING.zh-CN.md)

All contributions are welcome — bug reports, documentation improvements, code — and every bit of feedback is appreciated. This guide helps you land your first issue or PR smoothly.

## Table of contents

- [Before you start](#before-you-start)
- [Reporting bugs](#reporting-bugs)
- [Proposing features](#proposing-features)
- [Reporting security vulnerabilities](#reporting-security-vulnerabilities)
- [Development setup](#development-setup)
- [Running tests](#running-tests)
- [Commit conventions](#commit-conventions)
- [About AI assistance](#about-ai-assistance)
- [Submitting a pull request](#submitting-a-pull-request)
- [Areas with special constraints](#areas-with-special-constraints)
- [License](#license)

## Before you start

- Search [existing issues](https://github.com/Hoper-J/ccwrap/issues) first to avoid duplicate reports or duplicated work.
- Small fixes (typos, docs, obvious bug fixes) can go straight to a PR.
- For changes to behavior or architecture, please **open an issue first** — ccwrap sits on the trust boundary between Claude Code and the upstream, and some designs that look odd (fail-closed, for example) are deliberate.
- Be kind and respectful.

## Reporting bugs

When opening an issue, please include as much of the following as you can:

- **Steps to reproduce**: the command you started from, what you expected, what actually happened
- **Version info**: the output of `ccwrap version`, and your OS (macOS / Linux)
- **Routing shape**: first-party passthrough or a third-party gateway (do **not** paste API keys or any credentials)
- Relevant terminal output or dashboard behavior

## Proposing features

Open an issue describing your scenario and the behavior you expect. Explaining *why you need it* matters more than *how to implement it*.

## Reporting security vulnerabilities

**Do not open a public issue.** Report privately via "Report a vulnerability" (Security Advisories) on the repository's Security tab — see [SECURITY.md](SECURITY.md).

## Development setup

Prerequisites:

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.24+ | required by `go.mod` |
| Node.js | LTS | the web dashboard's behavioral tests run the rendered JS under a real `node` process; without node they silently `t.Skip` |
| shellcheck | any recent | optional, only needed when touching `install.sh` (CI runs it) |

Clone and build:

```bash
git clone https://github.com/Hoper-J/ccwrap && cd ccwrap
go build -o ccwrap ./cmd/ccwrap
./ccwrap version
```

## Running tests

```bash
gofmt -l $(git ls-files '*.go')   # should print nothing
go vet ./...
go test ./...
go test -race ./...
```

## Commit conventions

Commit messages follow the [Conventional Commits](https://www.conventionalcommits.org/) style: `type(scope): description`. `type` and `scope` are English; the description after the colon may be **Chinese or English**. Real examples from the repo:

```
build(npm): 将 npm 包重命名为无 scope 的 ccwrap-cli
fix(envpolicy): scrub the anthropic_aws/mantle/gateway families
feat(launcher): inject an aligned timezone into the Claude Code child
```

Common types: `feat` / `fix` / `docs` / `ci` / `build` / `refactor` / `test`. The scope is usually the affected package or area (`envpolicy`, `launcher`, `readme`, `npm`, …).

## About AI assistance

ccwrap itself exists to serve Claude Code, and we have nothing against AI-assisted coding. Two rules:

- You must **understand and personally verify** every change you submit — "the AI wrote it, I didn't look closely" is not an acceptable state for a PR.
- If a change is largely AI-generated, say so honestly in the PR description.

## Submitting a pull request

1. Fork the repository and create a feature branch off `main`
2. Make your changes and **add tests for behavior changes**
3. Run the [self-checks above](#running-tests)
4. Push to your fork and open a PR against `main`

What we expect from a PR:

- **One PR does one thing** — easier to review and to roll back
- State the **motivation** (what problem it solves) and the **verification** (how you confirmed it works); link fixed issues with `Fixes #N`
- All CI must be green before merging, except `govulncheck` (advisory only — its findings are almost always Go standard-library CVEs that clear with a newer Go patch release)

This is a personally maintained project, so review may take a while — thanks for your patience. Review comments address the code only; an initial pre-review may be performed by fable-5 / gpt-5.6-sol (labeled as such).

## Areas with special constraints

Before touching these, read the surrounding comments and the corresponding tests:

- **The TLS fingerprint path** (`internal/tlsfp` and the supervisor's upstream dialing) — ccwrap's core value is that the upstream sees Claude Code's native undici fingerprint. Failing **closed** when mirroring fails (blocking that dial rather than falling back to Go's fingerprint) is deliberate; please don't "fix" it into fail-open. The undici baseline is generated by `scripts/gen-undici-baseline.mjs`.
- **Bilingual docs** — `README.md` (Simplified Chinese, default) pairs with `README.en.md`; keep both sides in sync.

## License

The project is under the [MIT License](LICENSE). By submitting a PR you agree that your contribution is released under the same license.
