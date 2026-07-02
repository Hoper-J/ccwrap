# ccwrap

简体中文 · [English](README.md)

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE) [![npm](https://img.shields.io/npm/v/@hoper-j/ccwrap)](https://www.npmjs.com/package/@hoper-j/ccwrap)

ccwrap 接管 Claude Code 与上游 API 之间的网络边界，路由到任何 Anthropic 兼容的网关并让 Claude Code 认为是第一方路径、允许按 provider 替换模型并检查每一次请求与响应。

## 背景

得益于官方“开源”的 2.1.88 版本源码，一些针对第三方 API 和非官方模型的限制被发现和分享，故也有了当前项目的诞生。当时 Claude Code 把一些新的特性限制在了第一方 API 路径上才能打开。比如：Auto mode 需要同时满足非第三方（BEDROCK / VERTEX / FOUNDRY）和 `/^claude-(opus|sonnet)-4-6/` 才能使用，这同样使得当时其他厂商的模型无法直接开启 auto mode，会直接报错。最初 ccwrap 只是为了简单地做个路径和模型别名映射绕过 auto mode 的限制 :)

## 安装

```bash
npm install -g @hoper-j/ccwrap

# 或：安装脚本（下载预编译二进制）
curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh

# 或：用 Go 直接装
go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest
```

或从源码构建：

```bash
git clone https://github.com/Hoper-J/ccwrap && cd ccwrap && go build -o ccwrap ./cmd/ccwrap
```

## 快速开始

ccwrap 可以直接启动 Claude Code，也可以加载一个已保存的配置（profile）：

```bash
# 1) 直接启动：走你现有的 Claude 认证（第一方），ccwrap 只在中间做检查、不改路由
ccwrap

# 2) 接一个网关：存成 profile，再按名字启动
ccwrap profile add gateway \
  --base-url https://gateway.example \
  --auth-mode ccwrap_bearer --auth-key sk-xxxxxxxx
ccwrap --profile gateway

# 3) 设为默认
ccwrap profile set-default gateway && ccwrap
```

### 怎么接一个 provider（以 DeepSeek 为例）

#### 命令行

DeepSeek 有 `https://api.deepseek.com/anthropic` 端点：

```bash
ccwrap profile add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --auth-mode ccwrap_bearer \
  --auth-key sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx \
  --model-alias claude-opus-4-8=deepseek-v4-flash
ccwrap --profile deepseek
```

也可以写环境变量名：`--auth-key-env DEEPSEEK_KEY`。

#### web

Ctrl + 点击启动横幅里 `inspect` 后面那个本地 URL 打开 dashboard，然后点 `profile` -> `new profile`：

![image-20260620180955100](assets/image-20260620180955100.png)

## 特性

1. **让 Claude Code 认为自己在和  `api.anthropic.com`  进行交互**

   出于成本、地域、自托管模型、合规等原因，有时候需要把 Claude Code 路由到一个 Anthropic 兼容网关，ccwrap 负责让 Claude Code 的行为继续匹配这个选择，而不会因为第一方判定没过就静默降级请求体。

2. **使用 Model 别名**

   让 Claude Code 认为自己在和官方模型对话，这可能会绕过某些非官模的限制

   ![image-20260618143526134](assets/image-20260618143526134.png)

   本地的 Claude Code 只会看到 Claude model id（`claude-sonnet-4-6`、`claude-opus-4-8` 等)，实际请求的时候会被替换为映射的模型。来源优先级：

   1. 活动 profile 的 `model_aliases`
   2. `--model-alias-file PATH` 或 `CCWRAP_MODEL_ALIASES_FILE`
   3. `--model-alias LOGICAL=PROVIDER` 或 `CCWRAP_MODEL_ALIASES_JSON`

   具体由 MITM 代理改写：

   - `/v1/messages` 和 `/v1/messages/count_tokens` 顶层 `model` 字段
   - `/v1/messages/batches` 的 `requests[*].params.model`

   响应端（JSON / SSE / batch JSONL）的 model 字段也会反向归一化回 Claude 模型，Claude 始终只看到官方模型 id。

   **拓展：研究某个模型的请求形态**

   用一个还没“开放”的模型名，可以看到 Claude Code 会怎么为它组织请求，接着用 alias 把实际调用路由到一个能应答的模型上：

   ```bash
   ccwrap --model-alias claude-fable-5=claude-opus-4-8 --model claude-fable-5 --capture-bodies
   ```

   `--model claude-fable-5` 让 Claude Code 按 `fable-5` 来组装请求，会因此打开一些原本不会发的请求形态（model 相关的 beta、system / harness 块）。alias 会实际把上游调用改写成 `claude-opus-4-8`，这样请求才能返回（因为 `claude-fable-5` 目前已经停用）。配合 `--capture-bodies`，能看到 Claude Code 实际发出的请求体，供参考学习。

   不过需要注意，新版本的客户端（>2.1.176）会硬拦截 fable-5 模型，所以你需要降到 2.1.176：

   | 2.1.176                                                      | >2.1.176                                                     |
   | ------------------------------------------------------------ | ------------------------------------------------------------ |
   | ![image-20260618145054775](assets/image-20260618145054775.png) | ![image-20260618144849100](assets/image-20260618144849100.png) |

3. **修复第三方缓存**

   对改写到非 `claude-*` provider 的请求移除破坏缓存的键。在近期所有的版本中，Claude Code 的请求体都类似于下方：

   ![image-20260618143246261](assets/image-20260618143246261.png)

   其中的 cch 每次请求都不一样，这会让非官方版本（未特殊处理）模型的 KV 缓存失效，所以当 alias 把模型改写成非 `claude-*` provider 时，ccwrap 会去除这一 block。

4. **固定出口代理**

   对于官方配置，我们通常更希望走一个固定的代理，所以 ccwrap 也同样支持 egress 出口的配置项，这会让选定的提供商以及官方遥测的 `http-intake.logs.us5.datadoghq.com` 都走指定的网络出口：

   ![image-20260618142721442](assets/image-20260618142721442.png)

5. **配置热切换**

   在浏览器检查器里（或命令行 `ccwrap profile switch <name>`）切换上游 profile、改 model alias，会在当前会话立即生效：

   ![image-20260619210619928](assets/image-20260619210619928.png)

6. **保持 Claude Code 的原生 TLS 指纹**

   普通 Go 转发时会把 TLS 指纹换成 Go `crypto/tls` 的——header 是 undici 的形状、指纹却是 Go 的，这个错位本身是识别特征。ccwrap 用 [utls](https://github.com/refraction-networking/utls) 把 Claude Code 自己的 ClientHello 原样复刻到上游。

7. **对齐时区，避免请求日期暴露地区**

   Claude Code 在判定 `ANTHROPIC_BASE_URL` 指向非官方时，会在注入系统提示词的 `Today's date is …` 里，用肉眼几乎无法分辨的**撇号变体**（`'` U+0027 / `'` U+2019 / `ʼ` U+02BC / `ʹ` U+02B9）和**日期分隔符**（`-` 换成 `/`）隐写 3 个身份判断，其中一位查看系统时区是不是中国（`Asia/Shanghai` / `Asia/Urumqi`）。

   由于这套隐写基于 `isFirstParty()` 判断，ccwrap 能够让 Claude Code 照常认为自己在直连 `api.anthropic.com`，不会进入这套逻辑，时区这一位自然也不会写入。

   这里再额外做一层，允许用户可选地把 Claude Code 的时区改为非中国时区（不影响当前终端环境），让系统时区本身也不落在判断范围内，同时解决在 UTC+8 下当天日期可能比美国会话早一天的地区标识。

   当前首次在中国时区启动时会弹出：

   ```text
   ccwrap: 检测到当前为中国时区，Claude Code 会把本机当天日期写进请求，
   ccwrap:   这可能暴露会话所在地区（+8 时区的日期可能比美国早一天）。
   ccwrap:   (Detected a China timezone; the stamped date can reveal your region.)
   ccwrap:   是否为 Claude Code 指定时区（让请求里的日期按该时区计算）？
   ccwrap:   回车=America/Los_Angeles，或输入一个 IANA 时区名，输入 n 跳过：
   ```

   回车用默认 `America/Los_Angeles`，或输入任意 [IANA](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) 名（如 `Europe/Paris`），或 `n` 跳过。选择会记住，以后不再问。也可以直接用 `--timezone IANA` 或 `CCWRAP_TZ` 指定，优先级 `--timezone` > `CCWRAP_TZ` > 记住的选择 > 首次提示；非法时区会警告并忽略（不注入）。`ccwrap capture` 也沿用这套对齐，保证抓到的快照日期和真实会话一致。

8. **请求和响应阅读**

   使用 `--capture-bodies`（或在 dashboard 勾选 bodies），可以逐字读到 Claude Code 发出的每一条 `/v1/messages`：

   ![image-20260620115343451](assets/image-20260620115343451.png)

注意：使用当前工具有可能会触发一些官方特性（比如发送 `cache_control: { scope: "global" }`，tool schema 里加 `eager_input_streaming: true`，避免大 tool input 卡死），但如果上游服务器并没有处理这方面的请求，可能会出现预期外的情况。

另外，ccwrap 绕不开一些依赖于服务端的额外能力：Fast Mode 需要通过 `orgStatus` 检查、`max` effort 对上游模型能力有要求、Web Search 依赖 Anthropic 后端的搜索服务、Advisor tool 的 beta header。ccwrap 本身做的是让 Claude Code 发出去的请求体和上游是 `api.anthropic.com` 时发的一致，所以能否工作取决于网关本身是否处理了这些请求。

## 命令

```bash
ccwrap [CCWRAP_FLAGS...] [CLAUDE_ARGS...]      # 启动一个 Claude 会话
ccwrap [CCWRAP_FLAGS...] -- [CLAUDE_ARGS...]   # 显式分隔
ccwrap run [CCWRAP_FLAGS...] [--] [CLAUDE_ARGS...]   # 消除歧义
ccwrap status     [--json] [--session ID]
ccwrap dashboard  [--session ID] [--view overview|requests|errors|diagnostics]
ccwrap doctor     [--json] [--verbose] [--session ID] [--profile NAME]
ccwrap stop       [--session ID | --all]
ccwrap gc         [--json]
ccwrap capture    [--with-tls|--tls-only] [--main-inference] [--full] [--headers]
                  [--no-response] [--unmask] [--host H] [--path P] [--timeout DUR]
                  [--print-diff-filter] [--claude-bin PATH] [--timezone IANA] [-- CLAUDE_ARGS]
ccwrap profile    {ls | status | switch <name> | test [name] | test-egress [name] |
                   add <name> | edit <name> | rm <name> | set-default <name>}
                  [--session ID]
```

不带管理子命令时，`ccwrap` 会直接启动 Claude Code。在启动模式下，ccwrap 只消费它认识的flag，不认识的 flag、位置参数或 `--` 会原封不动传给 Claude。

## 启动 flags

| Flag | 说明 |
|------|------|
| `--upstream URL` | 等价于 base URL（不指定则从 profile / env 自动加载） |
| `--egress-proxy auto\|direct\|URL` | 出站流量走 proxy |
| `--session-name NAME` | 会话名（status / dashboard 里显示） |
| `--claude-bin PATH` | 指定 Claude Code 可执行文件路径（默认从 PATH 解析）|
| `--profile NAME` | 从 `profiles.json` 选 profile，可以是 profile 名或 provider 组名 |
| `--model-alias-file PATH` / `--model-alias LOGICAL=PROVIDER` | 内联 / 文件的 model alias 表。profile 里定义的 alias 优先 |
| `--upstream-headers-file PATH` / `--upstream-header NAME=VALUE` | ccwrap 所有的上游 header，永不对 Claude 可见 |
| `--capture-bodies`（或 `CCWRAP_CAPTURE_BODIES=1`）| 抓取请求与响应 body 查看。默认关。别名：`--capture-request-bodies` |
| `--capture-telemetry`（或 `CCWRAP_CAPTURE_TELEMETRY=1`）| 对 Claude Code 的遥测 host 白名单（datadog us5、sentry）做透明 MITM 并抓取 body 查看。默认关，不开启时遥测走盲隧道 |
| `--quiet`（或 `CCWRAP_QUIET=1`）| 把启动横幅收成一行（`ccwrap → host · profile · inspect URL`）。默认关 |
| `--timezone IANA`（或 `CCWRAP_TZ`）| 给 Claude Code 指定时区，让请求里的 `Today's date` 按该时区计算。首次在中国时区启动会提示是否对齐到 `America/Los_Angeles`，可用 `CCWRAP_NO_TZ_PROMPT=1` 或 `--no-init` 关掉提示。默认不注入 |
| `--no-init`（或 `CCWRAP_NO_INIT=1`）| 跳过首次启动 env → profiles.json 的自动迁移提示，以及首次时区提示。默认关 |
| `--allow-provider-model-passthrough` | 允许 Claude Code 看到 provider 端的 model id |
| `--allow-auth-passthrough-to-third-party` | 调试用：让 Claude-side auth 透传到第三方上游 |

## Profiles

<details>
<summary>schema · 解析优先级 · CLI · 热切换 · Egress 自检</summary>

profile 是预定义的配置：base URL、auth、model alias、上游 header、egress mode。macOS 放在 `~/Library/Application Support/ccwrap/profiles.json`，Linux 放在 `~/.config/ccwrap/profiles.json` 或 `$XDG_STATE_HOME/ccwrap/profiles.json`。

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

Auth 模式：

- `ccwrap_bearer` — 注入 `Authorization: Bearer <key>`
- `ccwrap_x_api_key` — 注入 `X-API-Key: <key>`
- 整个 `auth` 块省略 — ccwrap 不为这个 profile 接管 auth（Claude 自己的 OAuth / API key 直达上游）

Auth key 来源：内联 `key` 字段或 `key_env`（环境变量名，启动时解析）。两者互斥。

Egress 模式：`inherit`（用启动时解析的 proxy）、`direct`、`http`（URL scheme 必须是 `http://` 或 `https://`）、`socks5`（URL scheme 必须是 `socks5://`，DNS 在本地解析）、`socks5h`（URL scheme 必须是 `socks5h://`，DNS 在 proxy 端解析）。URL scheme 必须跟 mode 匹配，`mode=socks5` + `url=http://...` 这种组合会被拒绝。

### 默认配置：`official`

`official` profile 在 `profiles.json` 不存在时首次启动自动 seed。它代表官方 Claude Code 路径：无 `base_url`、无 `auth` 块、Claude 自己的凭据直达 `api.anthropic.com`。删掉当前 default 时，dashboard 的 `chooseFallbackDefault` 会优先回到 `official`（若它还在；否则取剩下的第一个 profile，再否则 inherit-env）；而 CLI 的 `ccwrap profile rm` 删 default 时直接重置为 inherit-env，`official` 下次启动自动重新 seed。

### 解析优先级

1. `--profile NAME`
2. `CCWRAP_PROFILE=NAME` 环境变量
3. `profiles.json` 里的 `file.default`
4. `official`（官方路径）

### CLI 相关

```bash
ccwrap profile ls                          # 列出 profile + 哪个是 default
ccwrap profile status                      # 当前会话用的 profile
ccwrap profile add gateway \
  --base-url https://gateway.example \
  --auth-mode ccwrap_bearer --auth-key "$KEY" \
  --model-alias claude-opus-4-8=gpt-5.5 \
  --upstream-header X-Gateway-Tenant=team-a
ccwrap profile edit gateway --auth-key-env GATEWAY_KEY
ccwrap profile switch gateway              # 实时热切换（不需重启 Claude）
ccwrap profile test                        # 用一个空请求探测上游
ccwrap profile test gateway                # 指定 profile
ccwrap profile set-default gateway         # 持久化为 file.default
ccwrap profile rm old-gateway
```

### 热切换 vs 需要重启

profile 切换大多数（1P → 网关、网关 → 网关等）是热切换：代理在内部重新绑定路由，Claude 进程会正常运行。唯一一种需要重启的转换是从第三方切回第一方，这时 dashboard / CLI 会显示 `refused_needs_relaunch`。

### Egress 出口自检

`ccwrap profile test-egress [name]` 探测每个 profile 的 egress 出口连通性，返回:

- 状态、延迟
- 出口公网 IP
- 地理位置（国家、地区、城市）
- ASN / 运营商

默认探测目标 `https://ipinfo.io/json`。可用 `CCWRAP_EGRESS_TEST_URL=<自托管端点>` 覆盖 — 任何返回 ipinfo schema (`{ip, country, region, city, org}`) 的 HTTPS endpoint 都可以。

该探测不发送任何 Claude API 流量，也不携带 profile 的任何凭据，只测出口路径。与 `ccwrap profile test`（测上游 auth）互补。

```
$ ccwrap profile test-egress gateway
PROFILE  STATUS  LATENCY  EGRESS_VIA                 PUBLIC_IP   GEO              ORG                       ERR
gateway  OK        142ms  socks5h://corp-proxy:1080  52.34.x.x   Seattle, WA, US  AS16509 Amazon.com, Inc.  -
```

也可以点击 Web UI 中 Egress 格的⚡按钮，它会检测当前 session 的实际网络出口。

**隐私提示**：开箱即用时 ipinfo.io 会看到你的 egress 出口 IP，这是探测方式决定的。如不希望第三方可见，设置 `CCWRAP_EGRESS_TEST_URL` 指向自托管端点。

</details>

## 第三方路由

<details>
<summary>按路径区分第三方路由</summary>

ccwrap 不会把每个 `api.anthropic.com` 请求都改写到网关。只有实际的模型网关路径才路由到上游：

- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `/v1/messages/batches` 的 create / retrieve / results / cancel 路径

`GET /v1/models` 由 ccwrap 本地从活动 alias 表回，避免 provider id 泄漏给 Claude。需要 shaped response 的 first-party 服务路径（`/api/claude_cli/bootstrap`、`/v1/mcp_servers`、`/mcp-registry/v0/servers`）也在 ccwrap 本地回。

其他非网关的 `api.anthropic.com` 路径收到一个无声的 `204 No Content`：记录为 synthetic 请求，不报错、不降级会话、不打到网关。

`--allow-provider-model-passthrough` 保持为兼容 / 调试模式。该模式下 `/v1/models` 可能透传到 provider，provider model id 也可能对 Claude 可见。

</details>

## Claude Code 系统提示词裁剪

<details>
<summary>为什么裁剪 · 裁剪什么</summary>

当 alias 改写后的 model 不是 `claude-*` 开头时（如 `claude-opus-4-8 → gpt-5.5`、`deepseek` 等），ccwrap 还会从请求 body 里额外裁掉两类 Claude Code 特有的 system block：

1. `x-anthropic-billing-header: cc_version=...; cc_entrypoint=cli; cch=...` — Anthropic 用的计费/签名 pixel。非 Anthropic 上游会当作字面 system 指令理解
2. `You are Claude Code, Anthropic's official CLI for Claude.`（以及两个 Agent SDK 变体）— Claude Code 的身份前缀。因为对非 Claude 模型而言，这是在让它扮演 Claude Code

识别按内容（identity 用精确匹配、billing-header 用前缀匹配），所以用户自定义的 "You are Claude Code, but speak Spanish." 不会被误杀，后续 Claude Code 版本升级改了 system prompt 顺序也不会过度裁剪。

设 `CCWRAP_KEEP_CLAUDE_METADATA=1` 可关闭这个行为。

Web UI 中 body 被捕获时会并排切换的视图：**received**（Claude Code 发出）和 **forwarded**（上游实际收到的）。

### Q：为什么需要裁剪？

system prompt 在 body 最前面，排在用户内容和 tools 之前。这里塞个 per-request hash（cch）意味着每次请求的前 ~80 字节都不一样，非官方上游基于 prefix 的 KV cache 会因此失效，unsloth 和 vllm 官方都提及了这一点：

- unsloth[^1]：

  > *"Claude Code recently prepends and adds a Claude Code Attribution header, which **invalidates the KV Cache, making inference 90% slower with local models**."*

  编辑 `~/.claude/settings.json` 加 `"CLAUDE_CODE_ATTRIBUTION_HEADER": "0"`（使用 `export CLAUDE_CODE_ATTRIBUTION_HEADER=0` 不行，env 必须放进 Claude 自己的 settings.json 才能在所有代码路径生效）。

- vLLM[^2] ：

  > *"Claude Code recently started injecting a per-request hash in the system prompt, which can defeat prefix caching because the prompt changes on every request"*

  目前 vLLM > 0.17.1 会在服务端自动剥离。

[^1]: [🕵️Fixing 90% slower inference in Claude Code](https://unsloth.ai/docs/basics/claude-code#fixing-90-slower-inference-in-claude-code)
[^2]: [Configuring Claude Code](https://docs.vllm.ai/en/v0.20.0/serving/integrations/claude_code/#configuring-claude-code)

ccwrap 的裁剪是类似的修复方式，实现在代理层，会在请求到达网关之前处理：

- 适用于任何非 Anthropic 上游（vLLM、sglang、llama.cpp server、OpenRouter Anthropic 兼容端点、Anthropic-API 兼容代理），不限于自带服务端剥离逻辑的网关
- 跟 `CLAUDE_CODE_ATTRIBUTION_HEADER=0` 可以叠加：如果客户端已经关了 attribution，ccwrap 就不会处理
- 不需要用户在每台机器手改 `~/.claude/settings.json`

</details>

## Egress proxy

<details>
<summary>固定出口 · SOCKS5 · DNS 解析位置</summary>

```text
Claude Code -> ccwrap 会话代理 -> [egress proxy] -> 真实上游
```

解析顺序：

- `--egress-proxy=direct` → 直连
- `--egress-proxy=<URL>` → 显式 proxy（`http://`、`https://`、`socks5://`、`socks5h://`）
- `--egress-proxy=auto` 或省略 → 遵从本地设置

SOCKS5 支持 RFC 1929 用户名密码鉴权。`socks5://` 本地解析 DNS；`socks5h://` 把域名发给 proxy 解析。

```bash
ccwrap --egress-proxy socks5://user:pass@proxy.example:1080
```

</details>

## 企业 proxy / CA 注意事项

<details>
<summary>企业 proxy 与 CA 信任注意事项</summary>

Claude Code 在 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` 下不会保护 proxy/CA env，所以 policy 管理的 proxy/CA 可能盖过 ccwrap 注入的会话代理路由和信任 bundle。用 Claude Code 时，**不要**把这些 key 放进 `managed-settings.json`、远端管理设置、MDM / HKCU policy：

- `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY`（及对应小写）
- `NODE_EXTRA_CA_CERTS`、`SSL_CERT_FILE`、`SSL_CERT_DIR`、`REQUESTS_CA_BUNDLE`、`CURL_CA_BUNDLE`、`GIT_SSL_CAINFO`、`NODE_TLS_REJECT_UNAUTHORIZED`

推荐部署模式，按偏好顺序：

1. 用 `--egress-proxy` 显式指定出站代理
2. 在启动 ccwrap 之前的 shell 环境里设
3. 设在非 policy 的 Claude 设置里（`~/.claude.json`、`~/.claude/settings.json`、project / local 设置或用户 `--settings`）
4. 把企业根 CA 装到宿主 OS 的信任库里

ccwrap 只做尽力而为的本地 / 缓存 policy 检查：基于文件的 managed-settings + 本地 `remote-settings.json` 缓存。运行后才拉取的远端管理设置、MDM policy、Windows HKCU policy 都没法 launch 前完全核验。`ccwrap doctor` 干净只代表"没检测到本地 / 缓存的 policy 网络 / trust env"。

</details>

## 无头抓取（`ccwrap capture`）

<details>
<summary>ccwrap capture 一次性导出用法</summary>

`ccwrap capture` 是请求检查器的无头版：它通过代理拉起一个一次性 Claude 会话，等到第一个匹配的 `/v1/messages` 交换后，把单个 JSON 对象（请求 body；可选响应、header、TLS 指纹）打到 stdout。用途是 diff 不同 Claude Code 版本实际发出的内容：system prompt、tool schema、beta flag。

```bash
ccwrap capture --full -- -p "hi" > v2.1.30.json   # body + 响应 + header + TLS
ccwrap capture --tls-only                         # 只要 JA3 / JA4 / peetprint
ccwrap capture --main-inference -- -p "hi"        # 跳过 quota/title/Warmup 调用，抓真正的 agent 推理
ccwrap capture --print-diff-filter                # 输出剥离逐次噪声的标准 jq diff 过滤器
```

`--` 之后的参数原样传给 Claude（务必带 `-p "..."`，恰好触发一次交换后 capture 即退出）。凭据 header 默认做保结构掩码；`--unmask` 会让 header 与 OAuth body 字段（`refresh_token`/`access_token`）**全部明文**——什么都不打码，别分享输出。stdout 只有 JSON；进度与错误走 stderr。`--main-inference` 下若真正的推理始终没出现，宽松的兜底路径仍会输出最接近的交换，并在 `meta.notes` 里标记降级。

`ccwrap capture` 也会像正常会话一样对齐时区（`--timezone` / `CCWRAP_TZ` / 记住的选择），但因为是非交互的，永远不会弹首次提示。

</details>

## Activity 列表

<details>
<summary>请求分类与筛选</summary>

Activity 区是单一的实时列表 + 类别筛选器：

| 类别 | 内容 |
|------|------|
| `forwarded-api` | 转发到上游网关的请求（model API 调用）。可展开 |
| `synthetic` | ccwrap 在本地直接回的请求（比如 `/v1/models`、MCP registry shim、OAuth bootstrap）|
| `tunnel` | 盲 CONNECT 隧道：host 通过 ccwrap CONNECT 但没做 MITM |
| `telemetry` | Claude Code 自己的 datadog / statsig 遥测流量。会显示但被标记 |
| `errors` | 上游 / 路由解析 / 启动错误 |
| `trace` | profile 切换、posture 变化、refused 转换 |

筛选感知截断：切到 "Forwarded API" 后，可见窗口是从全量 forwarded-api 中的最新一批重建的（不是从当前显示里切片），所以高频的 synthetic/tunnel 流量不会把网关请求淹没。

实时更新走 SSE（`/events`）。

</details>

## 子进程环境

<details>
<summary>传给 Claude 的 env 如何被擦净 / 注入</summary>

Claude 和它继承的子进程启动时会带上：

- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`
- `HTTPS_PROXY` / `https_proxy` / `HTTP_PROXY` / `http_proxy` → 会话代理
- `NODE_EXTRA_CA_CERTS` → ccwrap 的根证书
- 复合 CA bundle env（`系统根 + ccwrap 根`）给 Python / curl / Git
- 仅 loopback 的 `NO_PROXY`
- 一份 ccwrap 生成的 `--settings`，把同样的 proxy/CA 值镜像到 Claude flag 设置里

ccwrap 从子进程和生成的 settings 里**剥离**这些 env key（它们会创建绕过 ccwrap 的第二条 provider 控制路径）：

- `ANTHROPIC_BASE_URL`、`ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`CLAUDE_CODE_OAUTH_TOKEN`
- Bedrock / Vertex / Foundry 路由 key、`VERTEX_REGION_CLAUDE_*`
- `OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`、`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`、`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`（OTEL header 类 env 留着，除非有被阻止的 endpoint）

ccwrap **保留但不预执行**：

- `apiKeyHelper` — 任意 shell 代码，preflight 不跑
- `awsAuthRefresh`、`awsCredentialExport`、`gcpAuthRefresh`、`otelHeadersHelper` — 请求时跑的 helper。在 first-party 路径上 `ccwrap doctor` 会对它们告警；路由到第三方上游时启动直接阻塞（请改用 ccwrap-owned 上游 auth helper）

ccwrap **解析并重新注入**模型偏好 env（`ANTHROPIC_MODEL`、`ANTHROPIC_DEFAULT_*_MODEL*`、`ANTHROPIC_SMALL_FAST_MODEL`、`CLAUDE_CODE_SUBAGENT_MODEL`）：从父进程 env + 可信 Claude 设置中解析后注入子进程，作为经宿主中介的用户意图保留，而不是直接丢弃。

`ANTHROPIC_CUSTOM_HEADERS` 是 Claude 可见的。路由到第三方上游时，auth 风格的 header 名（`Authorization`、`X-API-Key`、`Api-Key`、`X-Gateway-Key`、`X-LitellM-Key`、`X-Provider-Key`、任何 token/secret/credential 风格的）启动前就阻塞；值不会出现在诊断里。网关专属 header 应该用 `CCWRAP_UPSTREAM_HEADERS_JSON` / `CCWRAP_UPSTREAM_HEADERS_FILE` / 可重复的 `--upstream-header` 让它们保持 ccwrap-owned。

</details>

## 环境变量

<details>
<summary>全部 CCWRAP_* 与兼容变量清单</summary>

用户可见：

| 变量 | 说明 |
|------|------|
| `CCWRAP_UPSTREAM` | 上游 base URL（兼容别名：`ANTHROPIC_BASE_URL`）|
| `CCWRAP_UPSTREAM_API_KEY` | `X-API-Key` 注入用的 API key（兼容别名：`ANTHROPIC_API_KEY`）|
| `CCWRAP_UPSTREAM_AUTH_TOKEN` | `Authorization: Bearer ...` 注入用 token（兼容别名：`ANTHROPIC_AUTH_TOKEN`）|
| `CCWRAP_UPSTREAM_HEADERS` / `_JSON` / `_FILE` | ccwrap 接管的上游 header 表，三种格式 |
| `CCWRAP_MODEL_ALIASES_FILE` / `_JSON` | Model alias 表，文件路径或内联 JSON |
| `CCWRAP_PROFILE` | Profile 名（同 `--profile`）|
| `CCWRAP_CAPTURE_BODIES=1` | 开启请求与响应 body 抓取（同 `--capture-bodies`）。默认关 |
| `CCWRAP_CAPTURE_TELEMETRY=1` | opt-in 遥测 MITM + body 抓取（同 `--capture-telemetry`）。默认关 |
| `CCWRAP_NATIVE_TLS=0` | ⚠ 原生 TLS 指纹镜像的 kill switch。stderr 会打危险提示 |
| `CCWRAP_NATIVE_TLS_HELLO` | 指向一份 `clienthello.bin`，用它钉死镜像指纹（取代实时捕获）。启动时 fail-fast 校验 |
| `CCWRAP_WEB_ALLOWED_HOSTS` | 逗号分隔的额外 `Host` 值，放行到仪表盘/info endpoint。默认只允许 loopback（DNS-rebinding 防护）；经隧道/反代访问仪表盘时需要设置 |
| `CCWRAP_QUIET=1` | 把启动横幅收成一行（同 `--quiet`）。默认关 |
| `CCWRAP_KEEP_CLAUDE_METADATA=1` | 关闭非 claude-* 上游的 Claude Code 系统提示词裁剪 |
| `CCWRAP_UNMASK_CREDENTIALS=1` | ⚠ 关闭检查器里 header + body 凭据打码。ccwrap 会往 stderr 打警告 |
| `CCWRAP_TZ` | 注入到 Claude Code 的时区（同 `--timezone`）。`--timezone` 优先 |
| `CCWRAP_NO_TZ_PROMPT=1` | 只关掉首次时区提示（保留 profiles 迁移提示）|
| `CCWRAP_NO_INIT=1` | 跳过首次启动的自动迁移提示（env → profiles.json）与时区提示 |

内部 / 高级：

| 变量 | 说明 |
|------|------|
| `CCWRAP_RUNTIME_DIR` / `CCWRAP_STATE_DIR` | 覆盖默认 runtime / state 目录 |
| `CCWRAP_MANAGED_SETTINGS_DIR` | 覆盖 managed-settings 检查路径 |
| `CCWRAP_SYSTEM_CA_BUNDLE` | 覆盖复合信任 store 用的系统 CA bundle 路径 |

</details>

## 文件系统布局

<details>
<summary>运行时目录 · socket · 缓存 · CA 文件</summary>

Runtime：

```text
<runtime>/sessions/<session-id>/
  manifest.json
  control.sock
  bodies/<spill-id>.json   # 仅在 --capture-bodies 开启时存在（请求 + 响应）
```

State：

```text
<state>/profiles.json        # 命名 profile（首次启动自动 seed `official`）
<state>/certs/
  ca-cert.pem
  ca-key.pem
  ca-bundle.pem
<state>/locks/
  ca.lock
```

`<runtime>` 和 `<state>` 默认位置：

- macOS：state 在 `~/Library/Application Support/ccwrap/`，runtime 在 `$TMPDIR/ccwrap-<UID>/`（通常在 `/var/folders/...` 下）
- Linux：state 在 `~/.config/ccwrap/`（或 `$XDG_STATE_HOME/ccwrap/`），runtime 同上
- 可用 `CCWRAP_STATE_DIR` / `CCWRAP_RUNTIME_DIR` 覆盖

</details>

## 安全

- 网关凭据（API key、bearer token、上游专属 header）不会进入 Claude 子进程 env、生成的 `--settings`、manifest、status 输出或浏览器 UI
- 路由到第三方上游时，Claude 始终只看到 Claude model id；provider 特定的 id 被限制在 ccwrap 内部数据里
- OAuth host body 在保存到磁盘给检查器看之前会把凭据 JSON 字段打码，但原始字节会原样发到上游
- Auth 风格的请求 header（`Authorization`、`X-API-Key` 等）在 store 侧就被掩码：记录进入 inspector wire 之前原文已被抹除，`/recent`、页面 bootstrap、SSE 流、HAR 导出、保存的 body 里都不会出现原文，只有保结构的 `Bearer sk-…‹redacted N chars›` 形式。`CCWRAP_UNMASK_CREDENTIALS=1` 是保留原文的 launch-time opt-in（ribbon 显示持久的 UNMASKED 标记）
- 向 Anthropic host 的上游拨号带的是 Claude Code 自己的 TLS 指纹（原生 TLS 镜像，**fail-closed**：镜像失败会阻断该次拨号，而不是暴露 Go 指纹）
- 仪表盘/info 监听拒绝 `Host` header 非 loopback 形态的请求（DNS-rebinding 防护，HTTP 421）。隧道/反代主机名需用 `CCWRAP_WEB_ALLOWED_HOSTS` 显式放行
- **经隧道访问 = 无鉴权仪表盘。** 一旦用 `CCWRAP_WEB_ALLOWED_HOSTS` 放行隧道/反代访问，**任何拿到该 URL 的人**都能读 `/recent`（请求元数据 + 脱敏 body）、抓取页面里的 CSRF token 并驱动 mutation（切 profile、改 alias、开关抓取）。凭据已脱敏，但 prompt / response body、profile 名、上游 host 都是明文——只在受信网络 / 受信隧道上开放，别把该 URL 贴到公开处
- Linux 上 `XDG_RUNTIME_DIR` 未设时，runtime 目录 fallback 到 `/tmp/ccwrap-<uid>`，其中的请求/响应 body 是**明文落盘**（文件 `0600`、目录 `0700`，同机他人读不到，会话结束即清）；裸 SSH / `su` 等场景建议设 `XDG_RUNTIME_DIR`
- 注入 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` 后，Claude-protected env key（proxy、CA）不会被 Claude 自己的设置源覆盖；例外是"企业 proxy / CA 注意事项"里列的那几类不受保护
- 会话监听只服务 `/`、`/healthz`、`/recent`、`/recent/body`、`/events`、`/native-tls`、`/native-tls/clienthello.bin`，加上 `/profile/*` 下的 profile API endpoint 和抓取开关 `/capture/bodies`、`/capture/telemetry`。其他路径返回 404

## TODO

- 固定设备指纹允许分享使用
- cc-switch 配置的快捷导入和同步

希望这一切对你有所帮助。如果项目有什么疏漏或者更好的想法，欢迎提出 issue 或 PR。
