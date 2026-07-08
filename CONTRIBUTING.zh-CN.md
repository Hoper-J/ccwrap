# ccwrap 贡献指南

[English](CONTRIBUTING.md) · 简体中文

无论是报告 bug、完善文档还是提交代码，所有形式的贡献都欢迎，感谢你做出的一切反馈。这份指南会帮你顺利提交第一个 issue 或 PR。

## 目录

- [开始之前](#开始之前)
- [报告 Bug](#报告-bug)
- [提议新功能](#提议新功能)
- [报告安全漏洞](#报告安全漏洞)
- [搭建开发环境](#搭建开发环境)
- [运行测试](#运行测试)
- [提交规范](#提交规范)
- [关于 AI 辅助](#关于-ai-辅助)
- [提交 Pull Request](#提交-pull-request)
- [有特殊约束的区域](#有特殊约束的区域)
- [许可](#许可)

## 开始之前

- 先搜索 [现有 issue](https://github.com/Hoper-J/ccwrap/issues)，避免重复报告或重复劳动。
- 小修小补（错别字、文档、显而易见的 bug 修复）可以直接开 PR。
- 涉及行为、架构的改动，请**先开 issue** 再动手——ccwrap 处在 Claude Code 和上游之间的信任边界上，一些看似奇怪的设计（比如 fail-closed）是刻意的。
- 交流时保持友善与尊重。

## 报告 Bug

开 issue 时请尽量包含：

- **复现步骤**：从哪条命令开始、期望什么、实际发生了什么
- **版本信息**：`ccwrap version` 的输出，以及操作系统（macOS / Linux）
- **路由形态**：走的是一方直连还是第三方网关（**不要**贴 API key 或任何凭据）
- 相关的终端输出或仪表盘表现

## 提议新功能

直接开 issue 描述你的场景和期望的行为。说明"为什么需要"比"怎么实现"更重要。

## 报告安全漏洞

**不要开公开 issue。** 请通过仓库 Security 标签页的 "Report a vulnerability"（Security Advisories）私密报告，详见 [SECURITY.md](SECURITY.md)。

## 搭建开发环境

前置要求：

| 工具 | 版本 | 说明 |
| --- | --- | --- |
| Go | 1.24+ | `go.mod` 的要求 |
| Node.js | LTS | web 仪表盘的行为测试会把渲染出的 JS 放进真实 `node` 进程执行；没有 node 这些测试会静默 `t.Skip` |
| shellcheck | 任意近期版本 | 可选，只在改 `install.sh` 时用到（CI） |

克隆并构建：

```bash
git clone https://github.com/Hoper-J/ccwrap && cd ccwrap
go build -o ccwrap ./cmd/ccwrap
./ccwrap version
```

## 运行测试

```bash
gofmt -l $(git ls-files '*.go')   # 应无输出
go vet ./...
go test ./...
go test -race ./...
```

## 提交规范

提交信息遵循 [Conventional Commits](https://www.conventionalcommits.org/) 风格：`type(scope): 描述`。`type` 和 `scope` 用英文，冒号后的描述**中英文皆可**。仓库里的真实例子：

```
build(npm): 将 npm 包重命名为无 scope 的 ccwrap-cli
fix(envpolicy): scrub the anthropic_aws/mantle/gateway families
feat(launcher): inject an aligned timezone into the Claude Code child
```

常用 type：`feat` / `fix` / `docs` / `ci` / `build` / `refactor` / `test`。scope 一般取受影响的包名或区域（`envpolicy`、`launcher`、`readme`、`npm`……）。

## 关于 AI 辅助

ccwrap 本身就是为 Claude Code 服务的项目，我们不反对用 AI 辅助编码。但请遵守两条：

- 你需要**理解并亲自验证**提交的每一处改动——"AI 写的我没细看"不是可接受的 PR 状态。
- 如果改动大量由 AI 生成，请在 PR 描述中如实说明。

## 提交 Pull Request

1. Fork 本仓库，从 `main` 拉一个特性分支
2. 完成改动，**为行为变化补测试**
3. 跑一遍[上面的自查命令](#运行测试)
4. Push 到你的 fork，向 `main` 发起 PR

对 PR 的期望：

- **一个 PR 只做一件事**，方便 review 和回滚
- 描述里写清**动机**（解决什么问题）和**验证方式**（怎么确认它工作）；修复 issue 时用 `Fixes #N` 关联
- 除 `govulncheck`（仅提示、不阻塞——它报的基本都是随 Go 补丁版本修复的标准库 CVE）之外，CI 全绿是合并前提

这是个人维护的项目，review 可能需要一段时间，请耐心；review 意见仅针对代码，可能由 fable-5 / gpt-5.6-sol 进行初期的预审（会标识）。

## 有特殊约束的区域

动这些地方之前，先读一下周边注释和对应测试：

- **TLS 指纹路径**（`internal/tlsfp` 及 supervisor 的上游拨号）— ccwrap 的核心是让上游看到 Claude Code 原生的 undici 指纹，镜像失败时 **fail-closed**（阻断该次拨号，而不是退回 Go 指纹）是刻意设计，请不要"顺手修"成 fail-open。undici 基线由 `scripts/gen-undici-baseline.mjs` 生成。
- **双语文档** — `README.md`（简体中文，默认）与 `README.en.md` 是一对，改动请两边同步。

## 许可

项目使用 [MIT 许可](LICENSE)。提交 PR 即表示你同意你的贡献以同样的许可发布。
