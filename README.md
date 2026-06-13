[English](./README.en.md) | **简体中文**

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:8b5cf6,100:14b8a6&height=180&section=header&text=tokenctl&fontSize=64&fontColor=ffffff&desc=cgroups%20for%20LLM%20tokens&descSize=18&descAlignY=68" alt="tokenctl banner" />
</p>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue?style=flat-square" alt="license"/></a>
  <img src="https://img.shields.io/badge/release-v0.1.0--dev-orange?style=flat-square" alt="release"/>
  <img src="https://img.shields.io/github/actions/workflow/status/SuperMarioYL/tokenctl/ci.yml?style=flat-square" alt="ci"/>
  <img src="https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go&logoColor=white&style=flat-square" alt="go"/>
  <img src="https://img.shields.io/badge/Claude%20Code-ready-8b5cf6?style=flat-square" alt="Claude Code"/>
  <img src="https://img.shields.io/badge/Agent-governable-14b8a6?style=flat-square" alt="Agent"/>
</p>

> **tokenctl 是面向平台工程团队的 cgroups 式 Agent 预算控制器，负责为 Claude Code 流量分配权重并执行抢占。**
>
> 一份 YAML 描述 `org → team → dev` 的预算树；一个二进制反代 Claude / OpenAI / Bedrock；流式 token 实时归账，越线软节流，超额硬拒绝（429 + `X-TokenCtl-Reason`），抢占式让权给高权重兄弟节点。

## 目录

- [为什么需要它](#为什么需要它)
- [快速上手（10 分钟）](#快速上手10-分钟)
- [架构概览](#架构概览)
- [配置说明](#配置说明)
- [对比 chrome-devtools-mcp](#对比-chrome-devtools-mcp)
- [付费 / Pricing](#付费--pricing)
- [路线图](#路线图)
- [开源协议与贡献](#开源协议与贡献)
- [分享一下](#分享一下)

## 为什么需要它

平台 / DevEx 团队正在面临同一个问题：CFO 把全公司的 **Claude Code** 账单交给一个人管，而 Helicone、Portkey、LangSmith 这些工具只在事后给你一张账单——它们「看见」却不「管住」。

ChromeDevTools 团队最近开源了 [`chrome-devtools-mcp`](https://github.com/ChromeDevTools/chrome-devtools-mcp)，让 Agent 直接驱动浏览器：调试器更强了，token 烧得也更快了。HKUDS 这类组织也在发布越来越多面向生产的 Agent 框架。当 **Agent** 成为团队里事实上的「无监督新员工」时，再没有 OS 级别的资源仲裁器就是失职——这就是 tokenctl 存在的理由：**一个工程团队真正能装上的 AI 预算控制器**。

> 灵感来源：Simon Willison 的「每月 \$1,500 Claude Code 预算」周记。我们写了那篇文章描述的「执行层」。

## 快速上手（10 分钟）

```bash
git clone https://github.com/SuperMarioYL/tokenctl && cd tokenctl
go build -o tokenctl ./cmd/tokenctl
./tokenctl init --org acme && ./tokenctl up -c tokenctl.yaml
```

接着把 Claude Code 的 `ANTHROPIC_BASE_URL` 指向 `http://localhost:8080`，再开一个终端运行：

```bash
./tokenctl top -c tokenctl.yaml
```

你会立刻看到每个 dev 节点的 token 实时滚动、父级团队的剩余预算逐秒下跌。详细步骤见 [docs/quickstart.md](./docs/quickstart.md)。

<details><summary>tokenctl top 示例输出</summary>

```
tokenctl top  2026-06-05T03:42:11Z  in-flight=1  throttles=0  denies=0
wallet: [██······························]  (4.2k / 20.00M = 0%)
────────────────────────────────────────────────────────────────────────────────
GROUP                             WEIGHT  USAGE       BUDGET      STATE
acme                              100     4.2k        20.00M      ok (0%)
acme.team-platform                50      4.2k        10.00M      ok (0%)
acme.team-platform.alice          50      4.2k        5.00M       ok (0%)
acme.team-product                 30      0           6.00M       ok (0%)
acme.team-research                20      0           4.00M       ok (0%)
```

</details>

> 📼 30 秒 asciinema 演示即将发布（见 [`assets/demo.tape`](./assets/demo.tape) 的录制脚本）。

## 架构概览

整个 v0.1 是一个二进制、三个内部模块：

| 模块 | 职责 |
| --- | --- |
| `internal/proxy` | TLS 反代 Claude / OpenAI / Bedrock，解析 SSE 流增量计 token |
| `internal/budget` | `TokenGroup` 递归树 + 单个仲裁 goroutine，负责准入 / 节流 / 抢占 |
| `internal/store` | 内嵌 BoltDB，持久化窗口内 `consumed` 计数 + append-only 审计日志 |

没有外部 DB、没有消息队列、没有第二个进程。一份 YAML，一个二进制，一个 BoltDB 文件。

## 配置说明

完整示例见 [`configs/tokenctl.example.yaml`](./configs/tokenctl.example.yaml)。核心字段：

| 字段 | 类型 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `listen` | string | `:8080` | 反代监听地址 |
| `tls.cert_file` / `tls.key_file` | string | 空 | 本地终结 TLS；留空为明文 HTTP |
| `store.path` | string | `tokenctl.db` | BoltDB 文件，相对路径基于 yaml 所在目录 |
| `metrics.listen` | string | `:9090` | Prometheus 抓取端 |
| `wallet.budget` | object | 空 | 跨多个 provider 的统一上限（一份钱包） |
| `providers[]` | list | 必填 | `claude` / `openai` / `bedrock`（后者需 `region`） |
| `tree` | object | 必填 | `org → team → dev` 递归预算树根节点 |
| `tree.weight` | int | 必填 | 节点在父节点 slack 内的相对权重 |
| `tree.budget.tokens` | int | 可选 | 该节点窗口内硬上限 |
| `tree.budget.window` | duration | 必填 | Go duration（`1h` / `24h` / `720h`） |
| `tree.budget.soft_throttle_at` | float ∈ (0,1] | `0.8` | 软节流触发比例 |
| `api_keys[]` | list | 必填 | 把入站 Bearer token 绑定到 leaf 路径 |

## 对比 chrome-devtools-mcp

[`ChromeDevTools/chrome-devtools-mcp`](https://github.com/ChromeDevTools/chrome-devtools-mcp) 让 Agent 拥有更强的工具能力，tokenctl 让平台团队拥有控制能力。两者解决的是同一条管线上的不同环节：

| 维度 | chrome-devtools-mcp | tokenctl |
| --- | --- | --- |
| 关注层级 | Agent 调用浏览器调试器 | Agent 消耗 token 的预算与抢占 |
| 部署形态 | npm / MCP server，跟随 Agent 进程 | 独立反代，平台侧统一治理 |
| 调试体验 | ✓ 远比 tokenctl 强 | — 不涉及 |
| 跨 provider 钱包 | — | ✓ Claude + OpenAI + Bedrock 一张账单 |
| 在线抢占（kill mid-stream） | — | ✓ 高权重兄弟节点缺粮时强制让权 |
| 软节流 + 硬拒绝 | — | ✓ 80% FIFO 排队 / 100% 429 |

实事求是地讲：`chrome-devtools-mcp` 在它的本职上比 tokenctl 强得多，二者通常是**配套部署**而不是替代关系——Agent 由它增强，token 由我们仲裁。

## 付费 / Pricing

OSS 二进制（Apache-2.0）永久免费。**托管控制面（Hosted Control Plane）** 面向需要采购流程的平台团队：

| 套餐 | 适合 | 价格 |
| --- | --- | --- |
| **OSS 自部署** | 1–500 个被治理席位 | 免费 |
| **Hosted Pro** | Series B/C，多区域 HA，SSO / SCIM，90 天审计留存，Slack / PagerDuty 告警 | **\$1,500 / 月** 起，含 500 席；额外 \$3 / 席 / 月 |
| **Hosted Enterprise** | 大型平台团队，私有部署支持，SOC2 路径 | 走单 |

锚点参考：Simon Willison 写的 \$1,500/月「单个开发者上限」——我们把这个数字搬到「钱包整体下限」上。

➡ 想 30 分钟内拿到一个跑在 us-east-1 的托管 endpoint？写信到 [leo.stack@outlook.com](mailto:leo.stack@outlook.com)。

## 路线图

- [x] **m1 — `proxy_meter`**：反代 + SSE 增量 token 归账 + `/metrics` + `tokenctl top` 实时视图
- [x] **m2 — `tree_weight`**：YAML 预算树 + 软节流（80%）+ 硬拒绝（429 + `X-TokenCtl-Reason`）
- [x] **m3 — `preempt_arb`**：在线抢占 + 跨 provider 钱包仲裁（Claude + OpenAI + Bedrock 一份钱）
- [ ] **m4** — 托管控制面 GA（Fly.io 多区域 + Stripe Billing + WorkOS SSO）
- [ ] **m5** — Web UI（只读视图先行，写入仍走 YAML + Git）
- [ ] **m6** — Anthropic Workspaces / LangSmith 计费 webhook 集成

## 开源协议与贡献

Apache-2.0。详见 [LICENSE](./LICENSE)。

提 Bug 或想法请在 [GitHub Issues](https://github.com/SuperMarioYL/tokenctl/issues) 开一张；PR 之前烦请先开一个 Issue 对齐方向。中文沟通完全 OK。

推送代码后，记得给仓库加上 topic 方便被搜索到：

```bash
gh repo edit --add-topic mcp --add-topic agent --add-topic claude-code --add-topic llm-budget
```

## 分享一下

```
tokenctl —— 给 Claude Code 装上 cgroups。
一份 YAML 描述 org→team→dev 预算树，反代 Claude/OpenAI/Bedrock，
越线 429，缺粮抢占。一个 Go 二进制，工程团队真能装上。
👉 https://github.com/SuperMarioYL/tokenctl
```
