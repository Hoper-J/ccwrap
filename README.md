<h1 align="center">ccwrap</h1>

<p align="center">简体中文 · <a href="README.en.md">English</a></p>

<p align="center">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
  <a href="https://www.npmjs.com/package/ccwrap-cli"><img alt="npm" src="https://img.shields.io/npm/v/ccwrap-cli"></a>
  <a href="https://github.com/Hoper-J/ccwrap/releases"><img alt="Release" src="https://img.shields.io/github/v/release/Hoper-J/ccwrap"></a>
  <a href="CONTRIBUTING.zh-CN.md"><img alt="PRs Welcome" src="https://img.shields.io/badge/PRs-welcome-brightgreen.svg"></a>
</p>

ccwrap 接管 Claude Code 与上游 API 之间的网络边界，路由到任何 Anthropic 兼容的网关并让 Claude Code 认为是第一方路径、允许按 provider 替换模型并检查每一次请求与响应。

## 背景

得益于官方“开源”的 2.1.88 版本源码，一些针对第三方 API 和非官方模型的限制被发现和分享，故也有了当前项目的诞生。当时 Claude Code 把一些新的特性限制在了第一方 API 路径上才能打开。比如：Auto mode 需要同时满足非第三方（BEDROCK / VERTEX / FOUNDRY）和 `/^claude-(opus|sonnet)-4-6/` 才能使用，这同样使得当时其他厂商的模型无法直接开启 auto mode，会直接报错。最初 ccwrap 只是为了简单地做个路径和模型别名映射绕过 auto mode 的限制 :)

## 安装

```bash
npm install -g ccwrap-cli

# 或：安装脚本（下载预编译二进制）
curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh

# 或：用 Go 直接装
go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest
```

或从源码构建：

```bash
git clone https://github.com/Hoper-J/ccwrap && cd ccwrap && go build -o ccwrap ./cmd/ccwrap
```

### 更新

```bash
ccwrap upgrade          # 渠道感知：npm 装的走 npm，二进制装的原地自替换（sha256 校验），go install 装的重跑 go install
ccwrap version --check  # 只查不装
```

新版本也会在启动横幅、会话退出时提示（每日最多检查一次，`CCWRAP_NO_UPDATE_CHECK=1` 关闭）。手动升级等价命令：`npm i -g ccwrap-cli@latest`、重跑 install.sh、`go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest`。

`ccwrap update` 是 `ccwrap upgrade` 的别名，升级的是 ccwrap 自己；要升级 Claude Code 本体请直接运行 `claude update`（想让它走 ccwrap 的会话环境则用 `ccwrap -- update`）。

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

运行 `ccwrap open` 打开 ccwrap ui（启动横幅里 `inspect` 后面的链接也可以直接 Ctrl + 点击），在 Profiles 页点 `New profile`：

![image-20260620180955100](assets/image-20260620180955100.png)

## 特性

1. **让 Claude Code 认为自己在和  `api.anthropic.com`  进行交互**

   出于成本、地域、自托管模型、合规等原因，有时候需要把 Claude Code 路由到一个 Anthropic 兼容网关，ccwrap 负责让 Claude Code 的行为继续匹配这个选择，而不会因为第一方判定没过就静默降级请求体。

   举个例子，第三方路径下的 Claude Code 会在请求头去掉一些特性，比如：`advanced-tool-use`，同时 mcp 会变成全部内联占用大量 token 而非使用 `ToolSearch` 工具延迟加载，这可能会导致首请求多出接近一倍的 token：

   | 官方                                                         | ccwrap 伪装                                                  | 正常途径使用第三方                                           |
   | ------------------------------------------------------------ | ------------------------------------------------------------ | ------------------------------------------------------------ |
   | ![image-20260701145024608](assets/20260701160500187.png) | ![image-20260701144101020](assets/20260701160452215.png) | ![image-20260701143808100](assets/20260701160456859.png) |

   下面的动画基于 2.1.193 版本，在 `claude -p hi` 下分别捕捉官方订阅账户和直接使用第三方的请求制作，可以看到第三方的首请求甚至会多出 82% 的 token：

   ![Third-Party Downgrade](assets/third-party-downgrade.webp)

2. **使用 Model 别名**

   让 Claude Code 认为自己在和官方模型对话，这可能会绕过某些非官模的限制。

   ![image-20260618143526134](assets/image-20260618143526134.png)

   本地的 Claude Code 只会看到 Claude model id（`claude-sonnet-4-6`、`claude-opus-4-8` 等)，实际请求的时候会被替换为映射的模型。来源优先级：

   1. 活动 profile 的 `model_aliases`
   2. `--model-alias-file PATH` 或 `CCWRAP_MODEL_ALIASES_FILE`
   3. `--model-alias LOGICAL=PROVIDER` 或 `CCWRAP_MODEL_ALIASES_JSON`

   具体由 MITM 代理改写：

   - `/v1/messages` 和 `/v1/messages/count_tokens` 顶层 `model` 字段
   - `/v1/messages/batches` 的 `requests[*].params.model`

   响应端（JSON / SSE / batch JSONL）的 model 字段也会反向归一化回 Claude 模型，Claude 始终只看到官方模型 id。

   没配别名的 Claude 模型会原样发给上游。如果上游只认它自己的模型名（比如只提供 GPT 的网关），可以用 `*` 给“其余所有 Claude 模型”指定一个默认目标：

   ```bash
   ccwrap profile edit <name> --model-alias '*=gpt-5.5'
   ```

   这样像 settings 里配的 `fable` 这类没有单独映射的模型也会落到这个目标上，响应里仍然还原成各自的名字。启动时如果 Claude Code 将要用的模型（按 `--model` > `ANTHROPIC_MODEL` > settings 里的 `model`）在当前路由上没有映射，启动横幅和 `ccwrap doctor`（`model_aliases` 一项）会直接给出这条命令。没有用这几种方式指定模型时，ccwrap 事先不知道 Claude Code 自带的默认模型是哪个，所以改看实际流量：会话里只要有 Claude 模型没有映射就发了出去，Claude Code 退出时 ccwrap 会提示，之后用这个 profile 启动也会提醒，直到补上映射为止。这时 ccwrap ui 里会出现一张问题卡片，Models 格子也会变黄，点一下就能选择发给哪个模型，或者保持原名，选完立即生效。

   如果上游本身认 Claude 的模型名，只是另外配了几条别名，可以用 `'*=*'` 表示其余 Claude 模型保持原名发送，ccwrap 也就不再提醒：

   ```bash
   ccwrap profile edit <name> --model-alias '*=*'
   ```

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

   在 ccwrap ui 的会话页里（或命令行 `ccwrap profile switch <name>`）切换上游 profile，或者在 Profiles 页改 model alias，都会在当前会话立即生效：

   ![image-20260619210619928](assets/image-20260619210619928.png)

6. **保持 Claude Code 的原生 TLS 指纹**

   普通 Go 转发时会把 TLS 指纹换成 Go `crypto/tls` 的——header 是 Claude Code 的形状、指纹却是 Go 的，这个错位本身是识别特征。ccwrap 用 [utls](https://github.com/refraction-networking/utls) 把 Claude Code 自己的 ClientHello 原样复刻到上游。Claude Code 内部不止一套 TLS（2.1.296 里 `/v1/messages` 和大部分其他请求用的 ClientHello 就不同），所以 ccwrap 按连接复刻：每个请求都带着它所在连接的那份指纹发往上游。HTTP 层也一样：Go 转发会把请求头按字母重排、统一大小写，ccwrap 照 Claude Code 原本的顺序和大小写发出去。

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

   使用 `--capture-bodies`（或在 ccwrap ui 的会话页打开 Bodies），可以逐字读到 Claude Code 发出的每一条 `/v1/messages`：

   ![image-20260620115343451](assets/image-20260620115343451.png)

   不开 body 抓取也能看出请求为什么失败：每条请求都会显示模型映射（`claude-opus-4-8 → gpt-5.5`，没映射的标 `(no alias)`），失败的请求会记下上游返回的错误内容和 ccwrap 给的修复建议，展开那一行就能看到。Traffic 格子下面那行是失败的模型请求数；Claude Code 自动重试时，同样的错误会在 Errors 里合并成一条，带上重复次数。终端里的 `ccwrap dashboard` 也一样。

注意：使用当前工具有可能会触发一些官方特性（比如发送 `cache_control: { scope: "global" }`，tool schema 里加 `eager_input_streaming: true`，避免大 tool input 卡死），但如果上游服务器并没有处理这方面的请求，可能会出现预期外的情况。

另外，ccwrap 绕不开一些依赖于服务端的额外能力：Fast Mode 需要通过 `orgStatus` 检查、`max` effort 对上游模型能力有要求、Web Search 依赖 Anthropic 后端的搜索服务、Advisor tool 的 beta header。ccwrap 本身做的是让 Claude Code 发出去的请求体和上游是 `api.anthropic.com` 时发的一致，所以能否工作取决于网关本身是否处理了这些请求。

## 命令

```bash
ccwrap [CCWRAP_FLAGS...] [CLAUDE_ARGS...]      # 启动一个 Claude 会话
ccwrap [CCWRAP_FLAGS...] -- [CLAUDE_ARGS...]   # 显式分隔
ccwrap run [CCWRAP_FLAGS...] [--] [CLAUDE_ARGS...]   # 消除歧义
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

不带管理子命令时，`ccwrap` 会直接启动 Claude Code。在启动模式下，ccwrap 只消费它认识的flag，不认识的 flag、位置参数或 `--` 会原封不动传给 Claude。

`ccwrap ui` 是每个用户一个的本地网页，看会话、管理 profile、按项目启动会话都在这里。启动会话时它会在后台自动拉起（`CCWRAP_NO_UI=1` 关闭；stderr 不是终端时也不拉起）；没有页面开着、也没有会话在跑时，10 分钟后自己退出。`ccwrap open` 会在需要时启动它，然后在浏览器里打开当前所在的会话，没有会话时打开 Projects 页。

在 SSH 里或者没有图形界面时，`ccwrap open` 不开浏览器，而是打印链接和一条 `ssh -L <端口>:127.0.0.1:<端口> <这台机器>` 转发命令，在自己的电脑上执行后就能打开；想让这条命令一直有效，用 `CCWRAP_UI_PORT` 固定端口。Projects 页里在终端中启动会话目前只支持 macOS（Terminal、iTerm、Ghostty）。

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
| `--no-update-check`（或 `CCWRAP_NO_UPDATE_CHECK=1`）| 关闭每日一次的新版本检查与全部更新提示（`upgrade` / `version --check` 等显式动作不受影响）。默认开 |
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

`base_url` 填网关的根地址，不要带 `/v1`：Claude Code 会自己拼上 `/v1/messages`，写成 `https://gateway.example/v1` 请求就会发到 `/v1/v1/messages`（不少网关文档给的是 OpenAI 风格、带 `/v1` 的地址）。写错时启动横幅和 `ccwrap doctor`（`upstream_path` 一项）会提示，并给出改好的地址。填成服务商的 OpenAI 兼容地址（比如百炼的 `/compatible-mode`、方舟的 `/api/v3`）时也会提示，并给出这家的 Anthropic 兼容地址；在 ccwrap ui 的 Profiles 页里点「Use it」就能换上。

Auth 模式：

- `ccwrap_bearer` — 注入 `Authorization: Bearer <key>`
- `ccwrap_x_api_key` — 注入 `X-API-Key: <key>`
- 整个 `auth` 块省略 — ccwrap 不为这个 profile 接管 auth（Claude 自己的 OAuth / API key 直达上游）

Auth key 来源：内联 `key` 字段或 `key_env`（环境变量名，启动时解析）。两者互斥。

Egress 模式：`inherit`（用启动时解析的 proxy）、`direct`、`http`（URL scheme 必须是 `http://` 或 `https://`）、`socks5`（URL scheme 必须是 `socks5://`，DNS 在本地解析）、`socks5h`（URL scheme 必须是 `socks5h://`，DNS 在 proxy 端解析）。URL scheme 必须跟 mode 匹配，`mode=socks5` + `url=http://...` 这种组合会被拒绝。

### 默认配置：`official`

`official` profile 在 `profiles.json` 不存在时首次启动自动 seed。它代表官方 Claude Code 路径：无 `base_url`、无 `auth` 块、Claude 自己的凭据直达 `api.anthropic.com`。在 ccwrap ui 里删掉当前 default 时，会优先回到 `official`（若它还在；否则取剩下的第一个 profile，再否则 inherit-env）；而 CLI 的 `ccwrap profile rm` 删 default 时直接重置为 inherit-env，`official` 下次启动自动重新 seed。

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

profile 切换大多数（1P → 网关、网关 → 网关等）是热切换：代理在内部重新绑定路由，Claude 进程会正常运行。唯一一种需要重启的转换是从第三方切回第一方，这时 ccwrap ui / CLI 会显示 `refused_needs_relaunch`。

### Egress 出口自检

`ccwrap profile test-egress [name]` 探测每个 profile 的 egress 出口连通性，返回:

- 状态、延迟
- 出口公网 IP
- 地理位置（国家、地区、城市）
- ASN / 运营商

默认探测目标 `https://ipinfo.io/json`。可用 `CCWRAP_EGRESS_TEST_URL=<自托管端点>` 覆盖 — 任何返回 ipinfo schema (`{ip, country, region, city, org}`) 的 HTTPS endpoint 都可以。

该探测不发送任何 Claude API 流量，也不携带 profile 的任何凭据，只测出口路径。与 `ccwrap profile test`（测上游 auth）互补。

出口为 inherit 的 profile，这两个测试（以及 ccwrap ui 里的测试）都和会话一样，在当前环境之上叠加用户级 Claude settings 里配置的代理（项目级配置不计入），出口测试的结果会写明「Claude settings · <代理地址>」。Claude settings 格式有错时直接报错。

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

同样的情况下，ccwrap 还会给 Claude Code 设上 `CLAUDE_CODE_TOASTY_THIMBLE=0` 和 `CLAUDE_CODE_GENTLE_PARASOL=0`，关掉它按 Claude 模型插在对话中途的提醒。这些提醒是给 Claude 调的，非 Claude 模型会把它当成最新指令：比如别名到 `claude-fable-5-1` 时，工具调用之后的那一轮会插一句"把互不依赖的项一次请求完"，GPT 会照字面去向用户要"材料"，而不是回答。你自己设了这两个变量的话，以你的为准。

设 `CCWRAP_KEEP_CLAUDE_METADATA=1` 可关闭以上行为。

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

实时更新走 SSE（ccwrap ui 的 `/api/v1/events`）。

</details>

## 子进程环境

<details>
<summary>传给 Claude 的 env 如何被擦净 / 注入</summary>

Claude 和它继承的子进程启动时会带上：

- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` —— **仅在 ccwrap 拥有上游凭据时注入**（profile 声明了 auth → 占位符 bootstrap）。当前版本的 Claude Code 会把这个 flag 理解成"凭据由宿主供给"，从而完全不再读取本地凭据（钥匙串里的订阅 OAuth、`/login` 存的 key、`apiKeyHelper`），所以一方透传时 ccwrap 不注入它，你的订阅登录照常工作
- `HTTPS_PROXY` / `https_proxy` / `HTTP_PROXY` / `http_proxy` → 会话代理
- `NODE_EXTRA_CA_CERTS` → ccwrap 的根证书
- 复合 CA bundle env（`系统根 + ccwrap 根`）给 Python / curl / Git
- 仅 loopback 的 `NO_PROXY`
- `CCWRAP_SESSION_ID` → 这次的 ccwrap 会话 ID，Claude Code 里跑的命令和 hook 可以靠它找到对应的会话
- `CLAUDE_CODE_GZIP_REQUEST_BODIES=0` → 请求 body 不压缩。ccwrap 的别名改写、system 块裁剪和请求查看器都要读 body；Claude Code 走代理时本来就不压缩，这一项只是盖过显式设置的 `=1`
- 一份 ccwrap 生成的 `--settings`，把上面这些值镜像到 Claude flag 设置里。默认以 inline JSON 传入：Claude Code 会把 `--settings` 文件所在目录随请求上报，inline 时报的是它自己的临时目录，不会带出 ccwrap 的路径。你自己传了 `--settings` 文件时，合并结果改写进 `/tmp/claude-<uid>/` 下的 0600 文件，免得文件内容出现在进程列表里

ccwrap 从子进程和生成的 settings 里**剥离**这些 env key（它们会创建绕过 ccwrap 的第二条 provider 控制路径）：

- `ANTHROPIC_BASE_URL`、`ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`CLAUDE_CODE_OAUTH_TOKEN`
- Bedrock / Vertex / Foundry 路由 key、`VERTEX_REGION_CLAUDE_*`
- `OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`、`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`、`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`（OTEL header 类 env 留着，除非有被阻止的 endpoint）

这份剥离清单只管子进程环境和 ccwrap 生成的 settings。Claude Code 还会自己读两处配置，可能把请求直接带离 ccwrap：

- 它自己的 settings 文件（`~/.claude/settings.json`、项目里的 `.claude/settings.json` 等）里的 `ANTHROPIC_BASE_URL`，或 `CLAUDE_CODE_USE_*` 这类 provider 开关
- Anthropic SDK 的凭据 profile（`ANTHROPIC_CONFIG_DIR`、`$XDG_CONFIG_HOME/anthropic` 或 `~/.config/anthropic` 下的 `configs/<name>.json`），不需要任何 env 就会生效，里面的 `base_url` 决定请求发往哪里

其中任何一处会把请求带到 api.anthropic.com 以外时，ccwrap **拒绝启动**，并一次列出所有问题和改法。settings 按 Claude Code 自己的优先级合并后再判断，所以在项目的 `.claude/settings.local.json` 里改回官方地址（或把开关设为 `0`）就不会被拦；ccwrap 自己提供凭据的第三方路由不受影响（这时 Claude Code 会忽略这两处）。`ccwrap doctor` 里分别对应 `claude_settings_route` 和 `anthropic_profile`。

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
| `CCWRAP_WEB_ALLOWED_HOSTS` | 逗号分隔的额外 `Host` 值，放行到 ccwrap ui。默认只允许 loopback（DNS-rebinding 防护）；经隧道/反代访问 ccwrap ui 时需要设置。ccwrap ui 启动时读取，改了以后先 `ccwrap ui stop` |
| `CCWRAP_NO_UI=1` | 启动会话时不顺带拉起 ccwrap ui（`ccwrap open` 仍会启动它） |
| `CCWRAP_UI_PORT` | 固定 ccwrap ui 的端口。默认沿用上次的端口，被占用时换一个 |
| `CCWRAP_LOCAL_PEER_CHECK=0` | ⚠ 关掉会话端口和 ccwrap ui 的本机用户检查（默认只接受与 ccwrap 同 uid 的进程和 root）。只有隧道/反代以其他非 root 用户运行时才需要 |
| `CCWRAP_QUIET=1` | 把启动横幅收成一行（同 `--quiet`）。默认关 |
| `CCWRAP_KEEP_CLAUDE_METADATA=1` | 关闭非 claude-* 上游的 Claude Code 系统提示词裁剪，以及对话中途提醒的关闭 |
| `CCWRAP_UNMASK_CREDENTIALS=1` | ⚠ 关闭检查器里 header + body 凭据打码。ccwrap 会往 stderr 打警告 |
| `CCWRAP_TZ` | 注入到 Claude Code 的时区（同 `--timezone`）。`--timezone` 优先 |
| `CCWRAP_NO_TZ_PROMPT=1` | 只关掉首次时区提示（保留 profiles 迁移提示）|
| `CCWRAP_NO_INIT=1` | 跳过首次启动的自动迁移提示（env → profiles.json）与时区提示 |
| `CCWRAP_NO_UPDATE_CHECK=1` | 关闭每日更新检查与提示（同 `--no-update-check`）。默认关（即检查开启）|
| `CCWRAP_UPDATE_CHECK_URL` | 覆盖版本发现端点（默认 `registry.npmjs.org/ccwrap-cli/latest`；任何返回 `{"version":"x.y.z"}` 的 HTTPS 端点均可，供自托管/镜像）|

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
  ccwrap-session-settings.json   # 生成的 settings 副本，仅供排查（不传给 Claude）
  bodies/<spill-id>.json   # 仅在 --capture-bodies 开启时存在（请求 + 响应）
<runtime>/ui/
  ui.json                  # ccwrap ui 的地址和进程信息（不含 token）
  ui.lock
  ui.sock                  # ccwrap 命令和 ccwrap ui 通信用的 unix socket
  ui.log
  launch/                  # Projects 页启动会话用的一次性脚本，运行后自删
```

State：

```text
<state>/profiles.json        # 命名 profile（首次启动自动 seed `official`）
<state>/projects.json        # 每个项目上次用的 profile
<state>/ui-port.json         # ccwrap ui 上次用的端口
<state>/terminal.json        # Projects 页选的终端
<state>/unmapped-models.json # 各 profile 实际发出过、但没有映射的 Claude 模型
<state>/timezone             # 首次运行时对时区提问的回答
<state>/certs/
  ca-cert.pem
  ca-key.pem
  ca-bundle.pem
<state>/locks/
  ca.lock
```

`<runtime>` 和 `<state>` 默认位置：

- macOS：state 在 `~/Library/Application Support/ccwrap/`，runtime 在 `$TMPDIR/ccwrap-<UID>/`（通常在 `/var/folders/...` 下）
- Linux：state 在 `~/.local/state/ccwrap/`（或 `$XDG_STATE_HOME/ccwrap/`），runtime 在 `$XDG_RUNTIME_DIR/ccwrap/`，没有 `XDG_RUNTIME_DIR` 时是 `/tmp/ccwrap-<UID>/`
- 可用 `CCWRAP_STATE_DIR` / `CCWRAP_RUNTIME_DIR` 覆盖

runtime 目录必须是当前用户自己的：不能是符号链接，属主必须是你，其他用户不能写入。不满足时 ccwrap 拒绝使用它，并提示设置 `CCWRAP_RUNTIME_DIR`。

</details>

## 安全

- 网关凭据（API key、bearer token、上游专属 header）不会进入 Claude 子进程 env、生成的 `--settings`、manifest、status 输出或浏览器 UI
- 路由到第三方上游时，Claude 始终只看到 Claude model id；provider 特定的 id 被限制在 ccwrap 内部数据里
- OAuth host body 在保存到磁盘给检查器看之前会把凭据 JSON 字段打码，但原始字节会原样发到上游
- Auth 风格的请求 header（`Authorization`、`X-API-Key` 等）在 store 侧就被掩码：记录进入 inspector wire 之前原文已被抹除，会话 API（活动列表、事件流）、`ccwrap capture`、保存的 body 里都不会出现原文，只有保结构的 `Bearer sk-…‹redacted N chars›` 形式。`CCWRAP_UNMASK_CREDENTIALS=1` 是保留原文的 launch-time opt-in（ribbon 显示持久的 UNMASKED 标记）
- 向 Anthropic host 的上游拨号带的是 Claude Code 自己的 TLS 指纹（原生 TLS 镜像，**fail-closed**：镜像失败会阻断该次拨号，而不是暴露 Go 指纹）；请求头也保持 Claude Code 原本的顺序和大小写
- ccwrap ui 拒绝 `Host` header 非 loopback 形态的请求（DNS-rebinding 防护，HTTP 421），写操作还要求同站请求并带上页面里的 token。隧道/反代主机名需用 `CCWRAP_WEB_ALLOWED_HOSTS` 显式放行
- **经隧道访问 = 无鉴权的 ccwrap ui。** 一旦用 `CCWRAP_WEB_ALLOWED_HOSTS` 放行隧道/反代访问，**任何拿到该 URL 的人**都能看所有会话的请求（元数据 + 脱敏 body），拿到页面里的 token 后还能执行写操作：切换和编辑 profile（包括改上游地址）、开关抓取、在你的终端里为已知项目启动会话。凭据已脱敏，但 prompt / response body、profile 名、上游 host 都是明文——只在受信网络 / 受信隧道上开放，别把该 URL 贴到公开处
- 会话端口和 ccwrap ui 只接受本机同一用户的连接：每个连接进来时，ccwrap 会向内核查对端 socket 的属主 uid（Linux 读 `/proc/net/tcp`，macOS 读 `net.inet.tcp.pcblist_n`），只放行与 ccwrap 同 uid 的进程和 root，其他用户的连接直接断开并记进日志——同机其他账号既借不了会话代理去用你的网关 key，也读不到 ccwrap ui 里抓到的对话。查不到属主时，会话端口放行，ccwrap ui 拒绝（它能启动会话、修改 profile）；`ccwrap doctor` 的 `local_access` 一行会显示是否生效。隧道/反代如果以其他非 root 用户运行也会被拒，这时用 `CCWRAP_LOCAL_PEER_CHECK=0` 关掉检查
- Linux 上 `XDG_RUNTIME_DIR` 未设时，runtime 目录 fallback 到 `/tmp/ccwrap-<uid>`，其中的请求/响应 body 是**明文落盘**（文件 `0600`、目录 `0700`，同机他人读不到，会话结束即清）；这个目录如果不是你自己建的（属主不是你、是符号链接、或其他用户可写），ccwrap 会拒绝使用。裸 SSH / `su` 等场景建议设 `XDG_RUNTIME_DIR`
- ccwrap 拥有上游凭据时会注入 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`，此时 Claude-protected env key（proxy、CA）不会被 Claude 自己的设置源覆盖；例外是"企业 proxy / CA 注意事项"里列的那几类不受保护。一方透传时不注入该 flag（当前版本的 Claude Code 会因它彻底拒读本地登录凭据）；路由/auth 的 env 通路仍由 ccwrap 自己的 spawn 清洗和生成 settings 剥离兜住
- 会话端口只做代理：发给端口本身的请求一律返回 404，只有一句提示，不带任何会话数据。会话的数据和操作走控制 socket（靠文件权限保护），ccwrap ui 和 `ccwrap capture` 都经由它访问
- 更新检查是一个不带任何标识的 plain GET（默认打到 `registry.npmjs.org`，每日最多一次，走 egress 出口，UA 为 versionless 的 `ccwrap-update-check`，响应限 1 MiB）。首次检查前横幅会有一次性声明；`CCWRAP_NO_UPDATE_CHECK=1` 关闭，`CCWRAP_UPDATE_CHECK_URL` 可指向自托管端点。`ccwrap upgrade` 的二进制自替换与 install.sh 同规格：sha256 必须匹配 checksums.txt（cosign 验签见 SECURITY.md）

## TODO

- 固定设备指纹允许分享使用
- cc-switch 配置的快捷导入和同步

希望这一切对你有所帮助。如果项目有什么疏漏或者更好的想法，欢迎提出 issue 或 PR。
