<h1 align="center">ccwrap</h1>

<p align="center">English · <a href="README.md">简体中文</a></p>

<p align="center">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
  <a href="https://www.npmjs.com/package/ccwrap-cli"><img alt="npm" src="https://img.shields.io/npm/v/ccwrap-cli"></a>
  <a href="https://github.com/Hoper-J/ccwrap/releases"><img alt="Release" src="https://img.shields.io/github/v/release/Hoper-J/ccwrap"></a>
  <a href="CONTRIBUTING.md"><img alt="PRs Welcome" src="https://img.shields.io/badge/PRs-welcome-brightgreen.svg"></a>
</p>

ccwrap owns the network boundary between Claude Code and the upstream API: it routes to any Anthropic-compatible gateway while keeping Claude Code on what it believes is the first-party path, swaps models per provider, and inspects every request and response.

## Background

Thanks to the officially "open-sourced" 2.1.88 source, a number of restrictions aimed at third-party APIs and non-official models were found and shared — which is how this project came to be. Back then Claude Code gated some new features behind the first-party API path. For example: Auto Mode required *both* a non-third-party provider (not BEDROCK / VERTEX / FOUNDRY) *and* a model matching `/^claude-(opus|sonnet)-4-6/`, which also meant other vendors' models couldn't turn on auto mode and would error out. ccwrap started as nothing more than a path + model-alias mapping to get around the auto-mode gate :)

## Install

```bash
npm install -g ccwrap-cli

# or: install script (downloads the prebuilt binary)
curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh

# or: install with Go
go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest
```

Or build from source:

```bash
git clone https://github.com/Hoper-J/ccwrap && cd ccwrap && go build -o ccwrap ./cmd/ccwrap
```

### Updating

```bash
ccwrap upgrade          # channel-aware: npm installs upgrade via npm; release binaries self-replace in place (sha256-verified); go-install builds re-run go install
ccwrap version --check  # check only, install nothing
```

New releases are also surfaced in the launch banner and on session exit (checked at most once daily; disable with `CCWRAP_NO_UPDATE_CHECK=1`). Manual equivalents: `npm i -g ccwrap-cli@latest`, re-run install.sh, or `go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest`.

`ccwrap update` is an alias for `ccwrap upgrade` and upgrades ccwrap itself; to update Claude Code, run `claude update` directly (or `ccwrap -- update` to route it through the ccwrap session environment).

## Quick start

ccwrap can launch Claude Code directly, or load a saved configuration (profile):

```bash
# 1) Launch directly: uses your existing Claude auth (first-party); ccwrap only inspects in the middle, no rerouting
ccwrap

# 2) Route through a gateway: save it as a profile, then launch by name
ccwrap profile add gateway \
  --base-url https://gateway.example \
  --auth-mode ccwrap_bearer --auth-key sk-xxxxxxxx
ccwrap --profile gateway

# 3) Set it as default
ccwrap profile set-default gateway && ccwrap
```

### Wiring up a provider (DeepSeek as an example)

#### CLI

DeepSeek exposes an `https://api.deepseek.com/anthropic` endpoint:

```bash
ccwrap profile add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --auth-mode ccwrap_bearer \
  --auth-key sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx \
  --model-alias claude-opus-4-8=deepseek-v4-flash
ccwrap --profile deepseek
```

You can also pass an env var name: `--auth-key-env DEEPSEEK_KEY`.

#### web

Run `ccwrap open` to open ccwrap ui (or Ctrl+click the link after `inspect` in the launch banner), then click `New profile` on the Profiles page:

![image-20260620180955100](assets/image-20260620180955100.png)

## Features

1. **Make Claude Code believe it's talking to `api.anthropic.com`**

   For cost, region, self-hosted model, or compliance reasons, you sometimes need to route Claude Code to an Anthropic-compatible gateway. ccwrap keeps Claude Code's behavior matched to that choice, instead of silently downgrading the request body because the first-party check didn't pass.

   For example, on a third-party path Claude Code drops some request features — e.g. `advanced-tool-use` — and inlines every MCP tool (spending a lot of tokens) instead of lazy-loading them via `ToolSearch`, which can nearly double the first request's tokens:

   | Official | ccwrap disguise | Plain third-party |
   | --- | --- | --- |
   | ![image-20260701145024608](assets/20260701160500187.png) | ![image-20260701144101020](assets/20260701160452215.png) | ![image-20260701143808100](assets/20260701160456859.png) |

   The clip below (built on 2.1.193, capturing `claude -p hi` from an official subscription account vs. a plain third-party path) shows the third-party first request carrying up to 82% more tokens:

   ![Third-Party Downgrade](assets/third-party-downgrade-en.webp)

2. **Model aliases**

   Make Claude Code think it's talking to an official model — which may get around some non-official-model restrictions.

   ![image-20260618143526134](assets/image-20260618143526134.png)

   Locally, Claude Code only ever sees Claude model IDs (`claude-sonnet-4-6`, `claude-opus-4-8`, etc.); the actual request is rewritten to the mapped model. Source precedence:

   1. The active profile's `model_aliases`
   2. `--model-alias-file PATH` or `CCWRAP_MODEL_ALIASES_FILE`
   3. `--model-alias LOGICAL=PROVIDER` or `CCWRAP_MODEL_ALIASES_JSON`

   The MITM proxy rewrites:

   - the top-level `model` field of `/v1/messages` and `/v1/messages/count_tokens`
   - `requests[*].params.model` of `/v1/messages/batches`

   Response-side model fields (JSON / SSE / batch JSONL) are normalized back to the Claude model too, so Claude only ever sees official model IDs.

   A Claude model with no alias goes to the upstream unchanged. If the upstream only knows its own model names (say, a GPT-only gateway), give "every other Claude model" a default target with `*`:

   ```bash
   ccwrap profile edit <name> --model-alias '*=gpt-5.5'
   ```

   Models without their own alias, like a `fable` set in your settings, then land on that target, and responses still come back under their own names. If the model Claude Code is about to start on (`--model` > `ANTHROPIC_MODEL` > the `model` setting) has no alias on the current route, the launch banner and `ccwrap doctor` (the `model_aliases` row) print that command for you. When none of those sets the model, ccwrap can't see Claude Code's built-in default ahead of time, so it goes by the traffic instead: once a session sends a Claude model with no alias, ccwrap says so after Claude Code exits, and later launches on that profile keep saying so until the route covers it. ccwrap ui then shows an issue card and turns the Models cell amber; one click picks a target model, or keeps the Claude names, and it applies right away.

   If the upstream does serve Claude model names and only a few need aliases, `'*=*'` keeps every other Claude model's name on purpose, and ccwrap stops flagging them:

   ```bash
   ccwrap profile edit <name> --model-alias '*=*'
   ```

   **Bonus: study a model's request shape**

   With a model name that hasn't "shipped" yet, you can see how Claude Code organizes a request for it, then use an alias to route the actual call to a model that can answer:

   ```bash
   ccwrap --model-alias claude-fable-5=claude-opus-4-8 --model claude-fable-5 --capture-bodies
   ```

   `--model claude-fable-5` makes Claude Code assemble the request as `fable-5`, which opens up request shapes it wouldn't otherwise send (model-related betas, system / harness blocks). The alias rewrites the upstream call to `claude-opus-4-8` so the request can actually return (`claude-fable-5` is currently retired). Together with `--capture-bodies`, you can see the request body Claude Code actually emits, for study.

   Note that newer clients (>2.1.176) hard-intercept the fable-5 model, so you need to downgrade to 2.1.176:

   | 2.1.176 | >2.1.176 |
   | --- | --- |
   | ![image-20260618145054775](assets/image-20260618145054775.png) | ![image-20260618144849100](assets/image-20260618144849100.png) |

3. **Fix third-party caching**

   Remove the cache-breaking key from requests rewritten to a non-`claude-*` provider. In every recent version, Claude Code's request body looks like this:

   ![image-20260618143246261](assets/image-20260618143246261.png)

   The `cch` in there differs on every request, which invalidates the KV cache for non-official (unhandled) models — so ccwrap removes this block whenever an alias rewrites the model to a non-`claude-*` provider.

4. **Pinned egress proxy**

   For official setups you usually want a fixed proxy, so ccwrap supports an egress config that routes the selected provider plus official telemetry (`http-intake.logs.us5.datadoghq.com`) through a chosen network egress:

   ![image-20260618142721442](assets/image-20260618142721442.png)

5. **Live config hot-swap**

   Switching the upstream profile in a session in ccwrap ui (or via `ccwrap profile switch <name>`), or editing a model alias on its Profiles page, takes effect in the current session immediately:

   ![image-20260619210619928](assets/image-20260619210619928.png)

6. **Keep Claude Code's native TLS fingerprint**

   A vanilla Go forward would swap the TLS fingerprint for Go's `crypto/tls` one — Claude Code-shaped headers under a Go fingerprint, and that mismatch is itself a tell. ccwrap uses [utls](https://github.com/refraction-networking/utls) to replay Claude Code's own ClientHello upstream byte-for-byte. Claude Code runs more than one TLS stack (in 2.1.296, `/v1/messages` and most other calls use different ClientHellos), so ccwrap mirrors per connection: every request goes upstream with the fingerprint of the connection it came in on. The same goes for the HTTP layer: a Go forward re-sorts the request headers and canonicalizes their casing; ccwrap sends them in Claude Code's own order and casing.

7. **Align the timezone so the request date doesn't reveal your region**

   When Claude Code decides `ANTHROPIC_BASE_URL` points somewhere non-official, it steganographically encodes three identity bits into the `Today's date is …` line of the system prompt, using near-indistinguishable **apostrophe variants** (`'` U+0027 / `'` U+2019 / `ʼ` U+02BC / `ʹ` U+02B9) and the **date separator** (`-` swapped for `/`); one of them checks whether the system timezone is China (`Asia/Shanghai` / `Asia/Urumqi`).

   Because that steganography is gated on `isFirstParty()`, ccwrap keeps Claude Code believing it talks straight to `api.anthropic.com`, so the whole path is skipped and the timezone bit is never written either.

   On top of that, ccwrap optionally lets you set Claude Code's timezone to a non-China one (without touching your actual terminal environment), so the system timezone itself also falls outside that check — and it incidentally fixes the region tell where a UTC+8 date can be a day ahead of a US session.

   The first run in a China timezone currently prompts:

   ```text
   ccwrap: 检测到当前为中国时区，Claude Code 会把本机当天日期写进请求，
   ccwrap:   这可能暴露会话所在地区（+8 时区的日期可能比美国早一天）。
   ccwrap:   (Detected a China timezone; the stamped date can reveal your region.)
   ccwrap:   是否为 Claude Code 指定时区（让请求里的日期按该时区计算）？
   ccwrap:   回车=America/Los_Angeles，或输入一个 IANA 时区名，输入 n 跳过：
   ```

   Enter accepts the default `America/Los_Angeles`, or type any [IANA](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) name (e.g. `Europe/Paris`), or `n` to skip. The choice is remembered and never re-prompted. You can also set it directly via `--timezone IANA` or `CCWRAP_TZ`; precedence is `--timezone` > `CCWRAP_TZ` > remembered choice > first-run prompt. An invalid zone warns and is ignored (no injection). `ccwrap capture` reuses the same alignment, so a captured snapshot's dates match a real session's.

8. **Read requests and responses**

   With `--capture-bodies` (or Bodies turned on for the session in ccwrap ui), you can read every `/v1/messages` Claude Code emits, verbatim:

   ![image-20260620115343451](assets/image-20260620115343451.png)

   You can see why a request failed even without body capture: every request shows its model mapping (`claude-opus-4-8 → gpt-5.5`, or `(no alias)` when unmapped), and a failed one keeps the error the upstream returned plus ccwrap's suggested fix — expand the row to read them. The line under the Traffic cell counts failed model requests, and when Claude Code retries, the same error folds into one Errors entry with a repeat count. `ccwrap dashboard` in the terminal shows the same.

Note: using this tool may trigger some first-party features (e.g. sending `cache_control: { scope: "global" }`, or adding `eager_input_streaming: true` to tool schemas to avoid hangs on large tool inputs), but if the upstream server doesn't handle those requests, unexpected behavior may occur.

Also, ccwrap can't get around capabilities that depend on the server side: Fast Mode needs to pass an `orgStatus` check, `max` effort requires upstream model capability, Web Search depends on Anthropic's backend search service, the Advisor tool needs its beta header. What ccwrap does is make Claude Code's emitted request body match what it sends when the upstream is `api.anthropic.com` — so whether it works depends on whether the gateway itself handles those requests.

## Commands

```bash
ccwrap [CCWRAP_FLAGS...] [CLAUDE_ARGS...]      # launch a Claude session
ccwrap [CCWRAP_FLAGS...] -- [CLAUDE_ARGS...]   # explicit boundary
ccwrap run [CCWRAP_FLAGS...] [--] [CLAUDE_ARGS...]   # disambiguate
ccwrap status     [--json] [--session ID]
ccwrap open       [--print] [SESSION-ID | PROJECT]
ccwrap ui         [status | stop]
ccwrap dashboard  [--session ID] [--view overview|requests|errors|diagnostics]
ccwrap doctor     [--json] [--verbose] [--session ID] [--profile NAME]
ccwrap stop       [--session ID | --all]
ccwrap gc         [--json]
ccwrap upgrade    [--egress-proxy auto|direct|URL]
ccwrap version    [--check] [--egress-proxy auto|direct|URL]
ccwrap capture    [--with-tls|--tls-only] [--main-inference] [--full] [--headers]
                  [--no-response] [--unmask] [--host H] [--path P] [--timeout DUR]
                  [--print-diff-filter] [--claude-bin PATH] [--timezone IANA] [-- CLAUDE_ARGS]
ccwrap profile    {ls | status | switch <name> | test [name] | test-egress [name] |
                   add <name> | edit <name> | rm <name> | set-default <name>}
                  [--session ID]
```

Without a management subcommand, `ccwrap` launches Claude Code directly. In launch mode it consumes only the flags it recognizes; unknown flags, positional arguments, or `--` are passed to Claude verbatim.

`ccwrap ui` is one local web app per user: look at sessions, manage profiles, and start sessions by project there. A launch starts it in the background (`CCWRAP_NO_UI=1` turns that off; it also stays off when stderr is not a terminal), and it exits on its own after 10 minutes with no page open and no session running. `ccwrap open` starts it when needed and opens the session you are in, or the Projects page when no session is running.

On the Projects page each folder offers Continue (the latest conversation there), Resume… (pick one from the folder's conversations) and New (a new conversation). While a session runs in the folder, Resume… is in the row's ⋯ menu; the list marks the conversation that is open and opens its session instead of a second Claude Code on the same conversation. More in Claude Code… at the bottom opens Claude Code's own list. Once a session has ended, Continue on its page resumes that session's own conversation.

![image-20261030111515843](assets/image-20261030111515843.png)

Over SSH or without a graphical session, `ccwrap open` opens no browser: it prints the link and an `ssh -L <port>:127.0.0.1:<port> <this host>` command to run on your own machine. To keep that command valid, pin the port with `CCWRAP_UI_PORT`. Starting a session in a terminal from the Projects page works on macOS only for now (Terminal, iTerm, Ghostty).

## Launch flags

| Flag | Description |
|------|-------------|
| `--upstream URL` | Equivalent to the base URL (otherwise auto-loaded from profile / env). |
| `--egress-proxy auto\|direct\|URL` | Route outbound traffic through a proxy. |
| `--session-name NAME` | Session label (shown in status / dashboard). |
| `--claude-bin PATH` | Path to the Claude Code binary (otherwise resolved from PATH). |
| `--profile NAME` | Pick a profile from `profiles.json`; can be a profile name or a provider group. |
| `--model-alias-file PATH` / `--model-alias LOGICAL=PROVIDER` | Inline / file model alias map. Profile-defined aliases take precedence. |
| `--upstream-headers-file PATH` / `--upstream-header NAME=VALUE` | ccwrap-owned upstream headers, never Claude-visible. |
| `--capture-bodies` (or `CCWRAP_CAPTURE_BODIES=1`) | Capture request + response bodies for the inspector. Default OFF. Alias: `--capture-request-bodies`. |
| `--capture-telemetry` (or `CCWRAP_CAPTURE_TELEMETRY=1`) | Transparently MITM Claude Code's allowlisted telemetry hosts (datadog us5, sentry) and capture those bodies for the inspector. Default OFF — telemetry is otherwise blind-tunneled. |
| `--quiet` (or `CCWRAP_QUIET=1`) | Collapse the launch banner to one line (`ccwrap → host · profile · inspect URL`). Default off. |
| `--timezone IANA` (or `CCWRAP_TZ`) | Set Claude Code's timezone so the request's `Today's date` is computed in that zone. The first run in a China timezone prompts whether to align to `America/Los_Angeles`; suppress the prompt with `CCWRAP_NO_TZ_PROMPT=1` or `--no-init`. No injection by default. |
| `--no-init` (or `CCWRAP_NO_INIT=1`) | Skip the first-run env → profiles.json auto-migration prompt, and the first-run timezone prompt. Default off. |
| `--no-update-check` (or `CCWRAP_NO_UPDATE_CHECK=1`) | Disable the daily update check and all update notices (explicit `upgrade` / `version --check` are unaffected). Default: enabled |
| `--allow-provider-model-passthrough` | Let Claude Code see provider-side model IDs. |
| `--allow-auth-passthrough-to-third-party` | Debug-only: let Claude-side auth pass through to a third-party upstream. |

## Profiles

<details>
<summary>schema · resolution precedence · CLI · hot-swap · egress self-test</summary>

A profile is a named bundle of config: base URL, auth, model aliases, upstream headers, egress mode. Profiles live in `~/Library/Application Support/ccwrap/profiles.json` on macOS, `~/.config/ccwrap/profiles.json` or `$XDG_STATE_HOME/ccwrap/profiles.json` on Linux.

### Schema

```json
{
  "default": "gateway",
  "profiles": {
    "official": {
      "provider": "anthropic",
      "base_url": "",
      "egress": {"mode": "inherit"}
    },
    "gateway": {
      "provider": "openrouter",
      "base_url": "https://gateway.example",
      "auth": {"mode": "ccwrap_bearer", "key": "sk-..."},
      "model_aliases": {"claude-opus-4-8": "gpt-5.5"},
      "upstream_headers": {"X-Gateway-Tenant": "team-a"},
      "egress": {"mode": "inherit"}
    }
  }
}
```

Set `base_url` to the gateway's root, without `/v1`: Claude Code appends `/v1/messages` itself, so `https://gateway.example/v1` sends requests to `/v1/v1/messages` (many gateway docs quote the OpenAI-style `/v1` address). The launch banner and `ccwrap doctor` (the `upstream_path` row) flag it with the corrected address. They also flag a provider's OpenAI-compatible address (such as Alibaba Cloud Model Studio's `/compatible-mode` or Volcengine Ark's `/api/v3`) and give that provider's Anthropic-compatible address; the Profiles page in ccwrap ui swaps it in with "Use it".

Auth modes:

- `ccwrap_bearer` — inject `Authorization: Bearer <key>`
- `ccwrap_x_api_key` — inject `X-API-Key: <key>`
- omit the `auth` block entirely — ccwrap does not own auth for this profile (Claude's own OAuth / API key reaches the upstream directly)

Auth key source: inline `key` value OR `key_env` (env var name; resolved at launch). The two are mutually exclusive.

Egress modes: `inherit` (use the launch-time resolved proxy), `direct`, `http` (URL scheme must be `http://` or `https://`), `socks5` (URL scheme must be `socks5://`, DNS resolved locally), `socks5h` (URL scheme must be `socks5h://`, DNS resolved at the proxy). The URL scheme must match the mode — a `mode=socks5` + `url=http://...` combination is rejected.

### Reserved profile: `official`

The `official` profile is auto-seeded on first launch when `profiles.json` does not exist. It represents the first-party Claude Code path: no `base_url`, no `auth` block, Claude's own credentials reach `api.anthropic.com` directly. When you remove the current default in ccwrap ui, it falls back to `official` if it is still present (else the first remaining profile, else inherit-env); the CLI `ccwrap profile rm` simply resets the default to inherit-env, and `official` is re-seeded on the next launch.

### Resolution precedence

1. `--profile NAME`
2. `CCWRAP_PROFILE=NAME` env var
3. `file.default` from `profiles.json`
4. `official` (first-party path)

### CLI

```bash
ccwrap profile ls                          # list profiles + which is default
ccwrap profile status                      # the profile the active session is using
ccwrap profile add gateway \
  --base-url https://gateway.example \
  --auth-mode ccwrap_bearer --auth-key "$KEY" \
  --model-alias claude-opus-4-8=gpt-5.5 \
  --upstream-header X-Gateway-Tenant=team-a
ccwrap profile edit gateway --auth-key-env GATEWAY_KEY
ccwrap profile switch gateway              # live hot-swap (no Claude relaunch)
ccwrap profile test                        # probe the upstream with a no-op request
ccwrap profile test gateway                # specific profile
ccwrap profile set-default gateway         # persist as file.default
ccwrap profile rm old-gateway
```

### Hot-swap vs needs-relaunch

Most profile switches (1P → gateway, gateway → gateway, etc.) are hot-swaps: the proxy rebinds the route internally and the Claude process keeps running. The one transition that needs a relaunch is switching from a third-party back to first-party, where ccwrap ui / the CLI shows `refused_needs_relaunch`.

### Egress self-test

`ccwrap profile test-egress [name]` probes each profile's egress connectivity and returns:

- status, latency
- the public IP the egress is exiting through
- geographic location (country, region, city)
- ASN / organization

Default probe target: `https://ipinfo.io/json`. Override with `CCWRAP_EGRESS_TEST_URL=<your-endpoint>` — any HTTPS endpoint that returns the ipinfo schema (`{ip, country, region, city, org}`) works.

The probe sends NO Claude API traffic and carries NONE of the profile's credentials — it tests only the egress path. It complements `ccwrap profile test` (which tests upstream auth).

For a profile whose egress is inherit, both tests (and the tests in ccwrap ui) resolve the proxy the way a session does: the current environment with the proxy from your user-level Claude settings laid over it (project settings are not included). The egress result then reads "Claude settings · <proxy>". Malformed Claude settings are reported as an error.

```
$ ccwrap profile test-egress gateway
PROFILE  STATUS  LATENCY  EGRESS_VIA                 PUBLIC_IP   GEO              ORG                       ERR
gateway  OK        142ms  socks5h://corp-proxy:1080  52.34.x.x   Seattle, WA, US  AS16509 Amazon.com, Inc.  -
```

You can also click the ⚡ button on the Egress cell in the web UI to probe the current session's actual network egress.

**Privacy note:** out of the box, ipinfo.io sees your egress IP — that's inherent to how the probe works. If you don't want a third party to see it, set `CCWRAP_EGRESS_TEST_URL` to a self-hosted endpoint.

</details>

## Path-aware third-party routing

<details>
<summary>path-aware third-party routing</summary>

ccwrap does not rewrite every `api.anthropic.com` request to the gateway. Only the actual model gateway paths are routed upstream:

- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `/v1/messages/batches` create / retrieve / results / cancel paths

`GET /v1/models` is served locally from the active alias map so provider IDs do not leak into Claude. First-party service paths that need shaped responses (`/api/claude_cli/bootstrap`, `/v1/mcp_servers`, `/mcp-registry/v0/servers`) are answered locally by ccwrap too.

Other non-gateway `api.anthropic.com` paths receive a silent `204 No Content`: recorded as synthetic requests, no error, no session degradation, no gateway call.

`--allow-provider-model-passthrough` remains a compatibility / debug mode. In that mode `/v1/models` may be passed through and provider model IDs may be Claude-visible.

</details>

## Claude Code system-prompt stripping

<details>
<summary>why strip · what gets stripped</summary>

When the alias-rewritten model is **not** `claude-*` (typical with gateway routing like `claude-opus-4-8 → gpt-5.5`, `deepseek`, etc.), ccwrap additionally drops two Claude Code-specific system blocks from the request body:

1. `x-anthropic-billing-header: cc_version=...; cc_entrypoint=cli; cch=...` — Anthropic's billing/attestation pixel; non-Anthropic upstreams treat it as a literal system instruction.
2. `You are Claude Code, Anthropic's official CLI for Claude.` (and the two Agent-SDK variants) — Claude Code's identity preamble; for a non-Claude model it amounts to telling it to impersonate Claude Code.

Detection is content-based (exact-match for the identity, prefix-match for the billing-header), so a user-crafted `You are Claude Code, but speak Spanish.` override is not killed by mistake, and a later Claude Code release that reorders the system prompt does not over-strip.

In the same case ccwrap also sets `CLAUDE_CODE_TOASTY_THIMBLE=0` and `CLAUDE_CODE_GENTLE_PARASOL=0` for Claude Code, switching off the reminders it inserts mid-conversation for the Claude model it believes it is talking to. They are tuned for Claude, and a non-Claude model takes them as its latest instruction: aliased to `claude-fable-5-1`, the turn after a tool call carries "request every item that doesn't depend on another's result", and GPT literally asks the user for "items" instead of answering. Values you set yourself win.

Set `CCWRAP_KEEP_CLAUDE_METADATA=1` to turn all of this off.

In the web UI, when a body is captured the drawer offers a side-by-side toggle: **received** (what Claude Code sent) and **forwarded** (what the upstream actually receives).

### Q: why is stripping needed?

The system prompt sits at the start of the body, before user content and tools. A per-request hash (`cch`) there means the first ~80 bytes of every request differ, which defeats prefix-based KV caching on non-official upstreams. unsloth and vLLM both document this:

- unsloth[^1]:

  > *"Claude Code recently prepends and adds a Claude Code Attribution header, which **invalidates the KV Cache, making inference 90% slower with local models**."*

  Their workaround is editing `~/.claude/settings.json` to add `"CLAUDE_CODE_ATTRIBUTION_HEADER": "0"` (and they note `export CLAUDE_CODE_ATTRIBUTION_HEADER=0` does NOT work — the env has to be set inside Claude's own settings.json to apply in all code paths).

- vLLM[^2]:

  > *"Claude Code recently started injecting a per-request hash in the system prompt, which can defeat prefix caching because the prompt changes on every request, causing greatly reduced performance."*

  vLLM > 0.17.1 strips it server-side automatically.

[^1]: [🕵️Fixing 90% slower inference in Claude Code](https://unsloth.ai/docs/basics/claude-code#fixing-90-slower-inference-in-claude-code)
[^2]: [Configuring Claude Code](https://docs.vllm.ai/en/v0.20.0/serving/integrations/claude_code/#configuring-claude-code)

ccwrap's strip is the same fix applied at the proxy layer, before the request reaches the gateway:

- Works against any non-Anthropic upstream (vLLM, sglang, llama.cpp server, OpenRouter Anthropic-compatible endpoints, Anthropic-API-compatible proxies), not just gateways that ship their own server-side strip
- Composes with `CLAUDE_CODE_ATTRIBUTION_HEADER=0`: if the client already disabled attribution, ccwrap leaves it alone
- Does not require the user to edit `~/.claude/settings.json` per machine

</details>

## Egress proxy

<details>
<summary>pinned egress · SOCKS5 · DNS resolution location</summary>

```text
Claude Code -> ccwrap session proxy -> [egress proxy] -> real upstream
```

Resolution order:

- `--egress-proxy=direct` → direct
- `--egress-proxy=<URL>` → explicit proxy (`http://`, `https://`, `socks5://`, `socks5h://`)
- `--egress-proxy=auto` or omitted → resolved from local settings

SOCKS5 supports RFC 1929 username/password auth. `socks5://` resolves DNS locally; `socks5h://` sends the domain for proxy-side resolution.

```bash
ccwrap --egress-proxy socks5://user:pass@proxy.example:1080
```

</details>

## Enterprise proxy / CA notes

<details>
<summary>enterprise proxy & CA trust notes</summary>

Claude Code does **not** protect proxy/CA env under `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`, so policy-managed proxy/CA can override ccwrap's session-proxy routing and trust bundle. With Claude Code, do **not** place these keys in `managed-settings.json`, remote managed settings, or MDM / HKCU policy:

- `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY` (and lowercase variants)
- `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `NODE_TLS_REJECT_UNAUTHORIZED`

Recommended deployment patterns, in order of preference:

1. Set the outbound enterprise proxy explicitly with `--egress-proxy`
2. Set it in the launcher shell environment before invoking `ccwrap`
3. Set it in non-policy Claude settings (`~/.claude.json`, `~/.claude/settings.json`, project / local settings, or user `--settings`)
4. Install the enterprise root CA into the host OS trust store

ccwrap only performs best-effort local / cache policy inspection before launch: file-based managed settings plus the local `remote-settings.json` cache. Remote managed settings fetched after startup, MDM policy, and Windows HKCU policy are not fully verifiable pre-launch. A clean `ccwrap doctor` result only means "no detectable local / cache policy network / trust env."

</details>

## Headless capture (`ccwrap capture`)

<details>
<summary>ccwrap capture one-shot export usage</summary>

`ccwrap capture` is the headless counterpart of the request inspector: it launches a one-shot Claude session through the proxy, waits for the first matching `/v1/messages` exchange, and prints a single JSON object (request body; optionally response, headers, TLS fingerprint) to stdout. Built for diffing what different Claude Code versions put on the wire — system prompt, tool schemas, beta flags.

```bash
ccwrap capture --full -- -p "hi" > v2.1.30.json   # body + response + headers + TLS
ccwrap capture --tls-only                         # JA3 / JA4 / peetprint only
ccwrap capture --main-inference -- -p "hi"        # skip quota/title/Warmup calls; grab the real agent inference
ccwrap capture --print-diff-filter                # canonical jq filter that strips per-run noise for diffing
```

Everything after `--` is passed to Claude verbatim (always pass `-p "..."` so exactly one exchange fires and capture exits). Credential headers are masked structure-preservingly by default; `--unmask` emits credential VALUES in cleartext for BOTH request headers and OAuth body fields (`refresh_token`/`access_token`) — nothing is redacted, so don't share the output. stdout carries only the JSON; progress and errors go to stderr. Under `--main-inference`, if the real inference never lands, the relaxed fallback still emits the closest exchange and flags the degradation in `meta.notes`.

`ccwrap capture` also aligns the timezone like a normal session (`--timezone` / `CCWRAP_TZ` / the remembered choice), but being non-interactive it never shows the first-run prompt.

</details>

## Activity feed

<details>
<summary>request classes & filtering</summary>

The Activity section is a single live feed with class filters:

| Class | What it contains |
|-------|------------------|
| `forwarded-api` | Requests forwarded to the upstream gateway (model API calls). Expandable. |
| `synthetic` | Requests ccwrap answered locally (e.g. `/v1/models`, MCP registry shims, OAuth bootstrap). |
| `tunnel` | Blind CONNECT tunnels — host CONNECT'd through ccwrap without MITM. |
| `telemetry` | Claude Code's own datadog / statsig telemetry traffic. Visible but flagged. |
| `errors` | Upstream / route resolution / preflight errors. |
| `trace` | Profile switches, posture changes, refused transitions. |

Filter-aware capping: switching to "Forwarded API" rebuilds the visible window from the newest forwarded-api rows (rather than slicing the current display), so high-volume synthetic/tunnel traffic never buries the gateway requests.

Live updates flow via SSE (ccwrap ui's `/api/v1/events`).

</details>

## Child process environment

<details>
<summary>how the env passed to Claude is scrubbed / injected</summary>

Claude and inherited child processes are launched with:

- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` — **only when ccwrap owns the upstream credential** (profile-declared auth → placeholder bootstrap). Current Claude Code treats this flag as "credentials come from the host" and stops reading local credentials entirely (stored subscription OAuth, the `/login` key, `apiKeyHelper`), so in first-party passthrough ccwrap omits it and your subscription login keeps working
- `HTTPS_PROXY` / `https_proxy` / `HTTP_PROXY` / `http_proxy` → session proxy
- `NODE_EXTRA_CA_CERTS` → ccwrap root certificate
- a composite CA bundle env (`system roots + ccwrap root`) for Python / curl / Git
- loopback-only `NO_PROXY`
- `CCWRAP_SESSION_ID` → this ccwrap session's ID, so commands and hooks run inside Claude Code can find their session
- `CLAUDE_CODE_GZIP_REQUEST_BODIES=0` → uncompressed request bodies. Model aliasing, system-block stripping and the request inspector all read them; Claude Code skips gzip behind a proxy anyway, so this only overrides an explicit `=1`
- a ccwrap-generated `--settings` mirroring the values above into Claude flag settings. It is passed as inline JSON: Claude Code reports the directory of a `--settings` file upstream, and for inline JSON it reports its own temp dir, so no ccwrap path leaves the machine. If you pass your own `--settings` file, the merged result goes into a 0600 file under `/tmp/claude-<uid>/` instead, keeping its contents out of the process list

ccwrap **strips** these env keys from the child process and generated settings (they would create a second provider control path bypassing ccwrap):

- `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`
- Bedrock / Vertex / Foundry routing keys, `VERTEX_REGION_CLAUDE_*`
- `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (OTEL header-class env is kept unless a blocked endpoint is present)

That strip list covers the child env and ccwrap's generated settings. Claude Code also reads two sources on its own that can take its requests straight around ccwrap:

- its own settings files (`~/.claude/settings.json`, a project's `.claude/settings.json`, …): `ANTHROPIC_BASE_URL`, or a provider switch such as `CLAUDE_CODE_USE_*`
- Anthropic SDK credential profiles (`configs/<name>.json` under `ANTHROPIC_CONFIG_DIR`, `$XDG_CONFIG_HOME/anthropic` or `~/.config/anthropic`), which apply with no env at all; a profile's `base_url` decides where requests go

When either would send requests anywhere but api.anthropic.com, ccwrap **refuses to launch**, listing every such entry and how to clear it. Settings are judged after merging them the way Claude Code does, so overriding a project's value in its `.claude/settings.local.json` (the official URL, or a switch set to `0`) clears it. Third-party routes where ccwrap supplies the credential are unaffected (Claude Code ignores both there). `ccwrap doctor` reports them as `claude_settings_route` and `anthropic_profile`.

ccwrap **preserves but does not pre-execute**:

- `apiKeyHelper` — arbitrary shell code; not run during preflight
- `awsAuthRefresh`, `awsCredentialExport`, `gcpAuthRefresh`, `otelHeadersHelper` — request-time helpers. On first-party routes `ccwrap doctor` warns about them; when routing to a third-party upstream they are blocked at launch (use a ccwrap-owned upstream auth helper instead)

ccwrap **resolves and re-injects** model preference env (`ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL*`, `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL`): resolved from parent env + trusted Claude settings, then injected into the child as host-mediated user intent rather than discarded.

`ANTHROPIC_CUSTOM_HEADERS` is Claude-visible. When routing to a third-party upstream, auth-style header names (`Authorization`, `X-API-Key`, `Api-Key`, `X-Gateway-Key`, `X-LitellM-Key`, `X-Provider-Key`, anything token/secret/credential-style) are blocked before launch; values never appear in diagnostics. Gateway-only headers should use `CCWRAP_UPSTREAM_HEADERS_JSON` / `CCWRAP_UPSTREAM_HEADERS_FILE` / repeatable `--upstream-header` so they stay ccwrap-owned.

</details>

## Environment variables

<details>
<summary>full CCWRAP_* and compatibility variable list</summary>

User-facing:

| Variable | Description |
|----------|-------------|
| `CCWRAP_UPSTREAM` | Upstream base URL (compat alias: `ANTHROPIC_BASE_URL`). |
| `CCWRAP_UPSTREAM_API_KEY` | API key for `X-API-Key` injection (compat alias: `ANTHROPIC_API_KEY`). |
| `CCWRAP_UPSTREAM_AUTH_TOKEN` | Token for `Authorization: Bearer ...` injection (compat alias: `ANTHROPIC_AUTH_TOKEN`). |
| `CCWRAP_UPSTREAM_HEADERS` / `_JSON` / `_FILE` | ccwrap-owned upstream header map, three input formats. |
| `CCWRAP_MODEL_ALIASES_FILE` / `_JSON` | Model alias map, file path or inline JSON. |
| `CCWRAP_PROFILE` | Profile name (= `--profile`). |
| `CCWRAP_CAPTURE_BODIES=1` | Enable request + response body capture (= `--capture-bodies`). Default OFF. |
| `CCWRAP_CAPTURE_TELEMETRY=1` | Opt-in telemetry MITM + body capture (= `--capture-telemetry`). Default OFF. |
| `CCWRAP_NATIVE_TLS=0` | ⚠ Kill switch for native TLS fingerprint mirroring. Prints a stderr danger notice. |
| `CCWRAP_NATIVE_TLS_HELLO` | Path to a `clienthello.bin` that pins the mirrored fingerprint instead of live capture. Fail-fast validation at launch. |
| `CCWRAP_WEB_ALLOWED_HOSTS` | Comma-separated extra `Host` values admitted to ccwrap ui. Default loopback-only (DNS-rebinding guard); set this when serving ccwrap ui through a tunnel / reverse proxy. ccwrap ui reads it when it starts, so run `ccwrap ui stop` after changing it. |
| `CCWRAP_NO_UI=1` | Don't start ccwrap ui when a session launches (`ccwrap open` still starts it). |
| `CCWRAP_UI_PORT` | Pin ccwrap ui's port. By default it reuses its last port, or picks another when that one is taken. |
| `CCWRAP_LOCAL_PEER_CHECK=0` | ⚠ Turn off the local-user check on session ports and ccwrap ui (by default only processes running as ccwrap's own uid, and root, may connect). Only needed when a tunnel / reverse proxy runs as another non-root user. |
| `CCWRAP_QUIET=1` | Collapse the launch banner to one line (= `--quiet`). Default off. |
| `CCWRAP_KEEP_CLAUDE_METADATA=1` | Disable Claude Code system-prompt stripping and the mid-conversation reminder switch-off on non-claude-* upstreams. |
| `CCWRAP_UNMASK_CREDENTIALS=1` | ⚠ Disable header + body credential redaction in the inspector. ccwrap prints a stderr warning. |
| `CCWRAP_TZ` | Timezone injected into Claude Code (= `--timezone`). `--timezone` wins. |
| `CCWRAP_NO_TZ_PROMPT=1` | Suppress only the first-run timezone prompt (keeps the profiles-migration prompt). |
| `CCWRAP_NO_INIT=1` | Skip the first-run auto-migration prompt (env → profiles.json) and the timezone prompt. |
| `CCWRAP_NO_UPDATE_CHECK=1` | Disable the daily update check and notices (same as `--no-update-check`) |
| `CCWRAP_UPDATE_CHECK_URL` | Override the version-discovery endpoint (default `registry.npmjs.org/ccwrap-cli/latest`; any HTTPS endpoint returning `{"version":"x.y.z"}` works — for self-hosting/mirrors) |

Internal / advanced:

| Variable | Description |
|----------|-------------|
| `CCWRAP_RUNTIME_DIR` / `CCWRAP_STATE_DIR` | Override default runtime / state directories. |
| `CCWRAP_MANAGED_SETTINGS_DIR` | Override managed-settings inspection path. |
| `CCWRAP_SYSTEM_CA_BUNDLE` | Override the system CA bundle path used by the composite trust store. |

</details>

## Filesystem layout

<details>
<summary>runtime dirs · socket · cache · CA files</summary>

Runtime:

```text
<runtime>/sessions/<session-id>/
  manifest.json
  control.sock
  ccwrap-session-settings.json   # copy of the generated settings, for inspection only (not passed to Claude)
  bodies/<spill-id>.json   # only when --capture-bodies is on (request + response)
<runtime>/ui/
  ui.json                  # ccwrap ui's address and process info (no token)
  ui.lock
  ui.sock                  # unix socket ccwrap commands use to reach ccwrap ui
  ui.log
  launch/                  # one-shot scripts the Projects page starts sessions with; they delete themselves
```

State:

```text
<state>/profiles.json        # named profiles (auto-seeded with `official` on first run)
<state>/projects.json        # the profile each project used last
<state>/ui-port.json         # ccwrap ui's last port
<state>/terminal.json        # the terminal picked on the Projects page
<state>/unmapped-models.json # Claude models each profile sent without a mapping
<state>/timezone             # the answer to the first-run timezone question
<state>/certs/
  ca-cert.pem
  ca-key.pem
  ca-bundle.pem
<state>/locks/
  ca.lock
```

Default `<runtime>` and `<state>`:

- macOS: state in `~/Library/Application Support/ccwrap/`, runtime in `$TMPDIR/ccwrap-<UID>/` (typically under `/var/folders/...`)
- Linux: state in `~/.local/state/ccwrap/` (or `$XDG_STATE_HOME/ccwrap/`), runtime in `$XDG_RUNTIME_DIR/ccwrap/`, or `/tmp/ccwrap-<UID>/` without `XDG_RUNTIME_DIR`
- Override with `CCWRAP_STATE_DIR` / `CCWRAP_RUNTIME_DIR`

The runtime dir must be your own: not a symlink, owned by you, and not writable by other users. Otherwise ccwrap refuses to use it and suggests setting `CCWRAP_RUNTIME_DIR`.

</details>

## Security

- Gateway credentials (API keys, bearer tokens, upstream-only headers) never enter the Claude child process env, the generated `--settings` file, the manifest, the status output, or the browser UI.
- When routing to a third-party upstream, Claude only ever sees Claude model IDs; provider-specific IDs are confined to ccwrap-internal data.
- OAuth host bodies have their credential JSON fields redacted before being saved to disk for the inspector, but the original bytes still reach the upstream untouched.
- Auth-style request headers (`Authorization`, `X-API-Key`, etc.) are masked **store-side**: the raw value is erased before any record reaches the inspector wire, so it never appears in the session API (activity, event stream), `ccwrap capture`, or a saved body — only a structure-preserving `Bearer sk-…‹redacted N chars›` form. `CCWRAP_UNMASK_CREDENTIALS=1` is the launch-time opt-in to keep raw values (the ribbon shows a persistent UNMASKED marker).
- Anthropic-host upstream dials carry Claude Code's own TLS fingerprint (native-TLS mirror, **fail-closed**: a failed mirror blocks the dial instead of exposing a Go fingerprint), and request headers keep Claude Code's own order and casing.
- ccwrap ui refuses requests whose `Host` header is not loopback-shaped (DNS-rebinding guard, HTTP 421), and its writes also require a same-site request carrying the page's token. Tunnel / reverse-proxy hostnames must be allowlisted explicitly via `CCWRAP_WEB_ALLOWED_HOSTS`.
- **Tunneled access = an unauthenticated ccwrap ui.** Once you allowlist tunnel / reverse-proxy access via `CCWRAP_WEB_ALLOWED_HOSTS`, **anyone with the URL** can see every session's requests (metadata + redacted bodies), take the token from the page, and make changes: switch and edit profiles (upstream URLs included), toggle capture, and start sessions in your terminal for known projects. Credentials are redacted, but prompt / response bodies, profile names, and upstream hosts are plaintext — only expose it on a trusted network / trusted tunnel, and don't post the URL anywhere public.
- Session ports and ccwrap ui only accept connections from your own account: for every incoming connection ccwrap asks the kernel which uid owns the other end (Linux `/proc/net/tcp`, macOS `net.inet.tcp.pcblist_n`) and admits only processes running as ccwrap's own uid, plus root. Other users' connections are closed and noted in the log, so other accounts on the machine can neither use your gateway key through the session proxy nor read captured conversations in ccwrap ui. When the owner can't be determined, a session port allows the connection and ccwrap ui refuses it (it can start sessions and edit profiles); `ccwrap doctor`'s `local_access` row shows whether the check is active. A tunnel / reverse proxy running as another non-root user is refused too — set `CCWRAP_LOCAL_PEER_CHECK=0` in that case.
- On Linux, when `XDG_RUNTIME_DIR` is unset the runtime dir falls back to `/tmp/ccwrap-<uid>`, where request/response bodies are written **in plaintext** (files `0600`, dirs `0700` — not readable by others on the machine, cleared when the session ends). If that directory is not one you created (owned by someone else, a symlink, or writable by others), ccwrap refuses to use it. Set `XDG_RUNTIME_DIR` on bare SSH / `su` setups.
- When ccwrap owns the upstream credential it injects `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`, and Claude-protected env keys (proxy, CA) cannot be overridden by Claude's own settings sources — except for the unprotected categories listed under "Enterprise proxy / CA notes." In first-party passthrough the flag is omitted (current Claude Code would otherwise refuse local login credentials entirely); ccwrap's spawn scrub and generated-settings sanitisation still close the routing/auth env paths on ccwrap's side.
- Session ports only proxy: a request addressed to the port itself gets a 404 with a one-line hint and no session data. A session's data and actions go over its control socket (guarded by file permissions), which ccwrap ui and `ccwrap capture` use.
- The update check is a single unidentified plain GET (to `registry.npmjs.org` by default, at most once daily, through your egress path, with the versionless UA `ccwrap-update-check`, response capped at 1 MiB). A one-time banner notice precedes the first check; disable with `CCWRAP_NO_UPDATE_CHECK=1`, or point `CCWRAP_UPDATE_CHECK_URL` at a self-hosted endpoint. `ccwrap upgrade`'s binary self-replace matches install.sh's rigor: the sha256 must match checksums.txt (cosign verification: see SECURITY.md)

## TODO

- Pinned device fingerprint to allow shared use.
- `cc-switch` config quick-import and sync.

Hope this helps. If the project has gaps or you have better ideas, issues and PRs are welcome.
