[English](README.en.md) | **简体中文**

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/hero-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/hero-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/hero-dark.svg">
  <img src="assets/presentation/hero-light.svg" width="1000" alt="为组织、团队和开发者配置 token 预算，通过反向代理归账、控制准入并提供运行指标。">
</picture>

**为组织、团队和开发者配置 token 预算，通过反向代理归账、控制准入并提供运行指标。**

`v0.15.0` · `Go 1.24+` · [Apache-2.0](LICENSE)

[Website](https://tokenctl.lei6393.com) · [Demo record](docs/demo-results.json)

## 为什么使用

多位开发者共享模型额度时，需要知道请求归属以及什么时候应拒绝新请求。tokenctl 把入站 key 绑定到预算树叶子，将用量累计到父级，并支持共享钱包与模型层级限制。额度单位与费用估算分开配置。

## 架构

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/architecture-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/architecture-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/architecture-dark.svg">
  <img src="assets/presentation/architecture-light.svg" width="1000" alt="配置加载器构建树，budget 给请求分配带预留额度的 Admission，proxy 转发并解析支持协议里的用量。AddInput/AddOutput 归账后，BoltDB 保存计数和审计事件；Prometheus、top 与 export 展示运行状态。抢占取消 admission 上下文，由代理处理上游中断。">
</picture>

配置加载器构建树，budget 给请求分配带预留额度的 Admission，proxy 转发并解析支持协议里的用量。AddInput/AddOutput 归账后，BoltDB 保存计数和审计事件；Prometheus、top 与 export 展示运行状态。抢占取消 admission 上下文，由代理处理上游中断。

源码入口：[cmd/tokenctl/main.go](cmd/tokenctl/main.go) · [cmd/tokenctl/export.go](cmd/tokenctl/export.go) · [internal/config/config.go](internal/config/config.go) · [internal/budget/tree.go](internal/budget/tree.go) · [internal/budget/preempt.go](internal/budget/preempt.go) · [internal/proxy/proxy.go](internal/proxy/proxy.go) · [internal/store/state.go](internal/store/state.go) · [configs/tokenctl.example.yaml](configs/tokenctl.example.yaml)

## 安装

需要 Go 1.24+。示例 go run 在内存中建立预算树，不启动 HTTP 服务、不调用模型、不写用户账本。

```bash
git clone https://github.com/SuperMarioYL/tokenctl.git
cd tokenctl
go build -o bin/tokenctl ./cmd/tokenctl
```

## 快速开始

给生产预算树显式注入 3 个输入和 7 个输出 token 计数，在 10-token 上限后尝试下一次准入。数字是示例输入，不是从实际模型流量计量所得。

```bash
go run ./examples/presentation-demo
```

完整输入与执行步骤见上方命令及 [Demo 记录](docs/demo-results.json)。

## 使用

```bash
./bin/tokenctl init --org acme
./bin/tokenctl up -c tokenctl.yaml
# 在另一个终端：
./bin/tokenctl top -c tokenctl.yaml --once
```
启动前编辑 providers、api_keys 和预算值，并使用真实上游接受的鉴权配置。客户端要把请求路由到代理；只启动服务不会产生计数。软节流返回带 Retry-After 的 429，硬拒绝返回 budget_exceeded；进行中的流已发响应头时不能再改 HTTP 状态。

## 实际 Demo

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/process-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/process-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/process-dark.svg">
  <img src="assets/presentation/process-light.svg" width="1000" alt="给生产预算树显式注入 3 个输入和 7 个输出 token 计数，在 10-token 上限后尝试下一次准入。数字是示例输入，不是从实际模型流量计量所得。">
</picture>

### 用完预算后拒绝准入

首次请求完成归账后，下一次返回 budget exceeded。

```text
$ go run ./examples/presentation-demo
{
  "budget_tokens": 10,
  "group": "demo.developer",
  "next_request_denied": true,
  "reason": "tokenctl: budget exceeded",
  "supplied_input_tokens": 3,
  "supplied_output_tokens": 7
}
```

## 能力与接入

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/integrations-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/integrations-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/integrations-dark.svg">
  <img src="assets/presentation/integrations-light.svg" width="1000" alt="CLI 提供 init/up/top/export，服务公开 Prometheus 指标与快照。软件中的 provider 适配路径与实际云账户认证是分开的条件；本示例仅调用预算核心，不测试代理或任何供应商连接。">
</picture>

CLI 提供 init/up/top/export，服务公开 Prometheus 指标与快照。软件中的 provider 适配路径与实际云账户认证是分开的条件；本示例仅调用预算核心，不测试代理或任何供应商连接。



## 配置

完整示例见 [tokenctl.example.yaml](configs/tokenctl.example.yaml)。tree 定义 name/weight/budget/children，api_keys 绑定 leaf；wallet 提供总上限。model_tiers 支持模型名正则、cost_multiplier 和层级预算；reset_policy 支持 hard/rollover/grace。pricing 供 export 估算费用。store.path 相对配置目录解析；TLS、listen 与 metrics 配置服务地址。

## 路线图与范围

当前包含预算树、预留、模型层级、重置策略、代理计量与审计导出。托管控制面、团队 SSO 和供应商账单自动对接属于后续方向。

- 计量依赖上游协议和 usage 字段；token 额度与实际货币费用不同，不能把示例数字当作账单。
- 本示例未验证 SSE 计量、在线抢占或真实供应商连接；这些需要各自环境的接入验证。

![Terminal recording](assets/demo.gif) · [Recording script](docs/demo.tape)

## 许可证

[Apache-2.0](LICENSE)
