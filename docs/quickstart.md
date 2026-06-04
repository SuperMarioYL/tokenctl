# tokenctl quickstart — 10 minutes from clone to attributed traffic

This walkthrough takes you from a clean checkout to seeing your Claude Code
seat's tokens flowing through `tokenctl top`, attributed to the right leaf of
your org → team → dev tree.

Target time: **under 10 minutes**, no Docker, no cluster, no message queue.

## 1. Prerequisites

- Go 1.24+
- A Claude or OpenAI API key you can point at a proxy (read-only is fine — the
  proxy just forwards)
- Linux or macOS (Windows binaries are out of scope for v0.1)

## 2. Build the binary

```bash
git clone https://github.com/supermario-leo/tokenctl
cd tokenctl
go build -o tokenctl ./cmd/tokenctl
./tokenctl version
```

You should see something like:

```
tokenctl 0.1.0-dev (commit unknown, linux/amd64, go1.24.0)
```

## 3. Generate a sample config

```bash
./tokenctl init --org acme -c ./tokenctl.yaml
```

This writes a 3-team, 6-dev tree with placeholder API keys. Inspect it:

```bash
less ./tokenctl.yaml
```

You will see the wallet block, two upstream providers (claude + openai), the
recursive `tree:` block, and `api_keys:` mapping placeholder keys to leaves.

## 4. Edit the placeholder keys

Open `tokenctl.yaml`, find the `api_keys:` block, and replace
`replace-me-alice` with the real Bearer token you want to govern — typically
the `ANTHROPIC_API_KEY` your Claude Code seat is configured with, or a
synthetic per-dev token you mint upstream and then bind here.

> Static keys + mTLS is the only auth model in v0.1. SSO / SCIM / per-user
> identity is on the hosted-tier roadmap.

## 5. Start the proxy

```bash
./tokenctl up -c ./tokenctl.yaml
```

You should see:

```
tokenctl 0.1.0-dev — proxy on :8080, metrics on :9090/metrics, store ./tokenctl.db
export the following so your clients route through the proxy:
  export ANTHROPIC_BASE_URL=http://localhost:8080
  export OPENAI_BASE_URL=http://localhost:8080/v1
ctrl-c to stop.
```

Leave this terminal running.

## 6. Point your client at the proxy

In a second terminal:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=<the-key-you-bound-in-tokenctl.yaml>

# Claude Code, or any client that respects ANTHROPIC_BASE_URL:
claude "write a short poem about cgroups"
```

## 7. Watch tokens flow in `tokenctl top`

In a third terminal:

```bash
./tokenctl top -c ./tokenctl.yaml
```

You will see a live table:

```
tokenctl top  2026-06-05T03:42:11Z  in-flight=1  throttles=0  denies=0
wallet: [██······························]  (4.2k / 20.00M = 0%)
────────────────────────────────────────────────────────────────────────────────
GROUP                             WEIGHT  USAGE       BUDGET      STATE
acme                              100     4.2k        20.00M      ok (0%)
acme.team-platform                50      4.2k        10.00M      ok (0%)
acme.team-platform.alice          50      4.2k        5.00M       ok (0%)
acme.team-platform.bob            50      0           5.00M       ok (0%)
...
```

The usage column ticks up in real time as the streamed SSE response from
Claude reaches the proxy. Each token is attributed to the leaf bound to the
Bearer token, and every ancestor's `consumed` counter is updated too.

## 8. Trigger a soft-throttle (optional)

Lower a leaf's budget to something you'll burn in a single call:

```yaml
- name: alice
  weight: 50
  budget: { tokens: 5000, window: 1h, soft_throttle_at: 0.8 }
```

Restart `tokenctl up`, send a long request, and watch the leaf flip from
`ok` → `throttle (8x%)` → `DENY (10x%)`. Requests above 100% return
`HTTP 429` with two response headers:

```
X-TokenCtl-Reason: budget_exceeded
X-TokenCtl-Group: acme.team-platform.alice
```

The proxy logs the deny to BoltDB's audit bucket and to Prometheus
(`tokenctl_denies_total`).

## 9. Prometheus scrape

`http://localhost:9090/metrics` exposes per-group counters:

- `tokenctl_consumed_tokens{group="acme.team-platform.alice"}`
- `tokenctl_budget_tokens{group="..."}`
- `tokenctl_in_flight{group="..."}`
- `tokenctl_denies_total{reason="budget_exceeded",group="..."}`
- `tokenctl_throttles_total{reason="soft_throttle",group="..."}`

Wire it into your existing Grafana — there's nothing new to learn.

## 10. Shut down cleanly

`Ctrl-C` on the `tokenctl up` terminal. The arbiter goroutine drains, the
counter flusher commits one last time, BoltDB is closed. The audit log in
`tokenctl.db` persists across restarts — feed it into your SIEM if needed.

## Next steps

- Read [README.md](../README.md) for the cgroups analogy and the architecture
  diagram.
- Review [`configs/tokenctl.example.yaml`](../configs/tokenctl.example.yaml)
  for every knob, including the `wallet:` shared cap across Claude + OpenAI +
  Bedrock on a single bill.
- For commercial use (multi-region HA, SSO, 90-day audit retention, hosted
  control plane), see the **付费 / Pricing** section of the README.
