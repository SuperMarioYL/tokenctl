# Changelog

All notable changes to tokenctl are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] - 2026-07-26

Provider-routing correctness pass. One fix found by re-reading the shipped
source: the legacy Claude `/v1/complete` match used a plain prefix check that
also claimed OpenAI's `/v1/completions` endpoint, breaking that traffic under
the default claude-first provider ordering.

### Fixed
- **`POST /v1/completions` now routes to the OpenAI provider instead of being
  claimed by Claude.** `ClaudeProvider.Matches` matched `/v1/complete` with a
  literal `strings.HasPrefix`, but `/v1/complete` is a prefix of OpenAI's legacy
  `/v1/completions` endpoint. Since `(*Server).matchProvider` scans providers in
  config order and the shipped sample/default config lists `claude` before
  `openai`, first-match won and any `POST /v1/completions` request was claimed by
  Claude, reverse-proxied to api.anthropic.com (which does not serve that path),
  and the OpenAI provider never saw it — the request broke. The match is now
  boundary-aware: `/v1/complete` is accepted only when followed by end-of-string,
  `/`, or `?`, so `/v1/completions` falls through to the OpenAI provider.
  `/v1/messages` keeps its plain prefix match (no overlapping sibling) (MEDIUM).

### Added
- Go test: `internal/proxy/match_provider_test.go` builds the real
  `ClaudeProvider` / `OpenAIProvider` in the default claude-first order and
  asserts ordered first-match resolution — `POST /v1/completions` → openai,
  while `POST /v1/complete` and `POST /v1/messages` still → claude.

## [0.6.0] - 2026-07-13

Observability pass. One fix found by re-reading the shipped source: the m3
preemption count — the product's headline signal — was tracked by the proxy but
never shown in the `tokenctl top` live view.

### Fixed
- **`tokenctl top` now shows the preempt count.** The proxy's `/v1/snapshot`
  already serialised `preempts_total` (and the Prometheus collector exposed
  `tokenctl_preempts_total`), but the `tokenctl top` client struct had no
  matching field and the header printed only `throttles` and `denies`. So every
  arbiter preemption — the m3 headline the demo asciinema centres on — was
  silently dropped on decode and invisible in the live view operators watch. The
  header now reads `… throttles=N denies=N preempts=N`, and the snapshot field
  round-trips into the CLI (MEDIUM).

### Added
- Go test: `cmd/tokenctl/render_top_test.go` decodes a `/v1/snapshot` body
  carrying `preempts_total` and asserts the count round-trips into the client
  struct and appears in the rendered header (alongside the existing
  throttles / denies / in-flight signals).

## [0.5.0] - 2026-07-06

Wiring-correctness pass. Two fixes found by re-reading the shipped source: the
live `tokenctl up` proxy never bound its API keys or wallet (so it 401'd all
real traffic), and a mid-stream preempt was surfacing as an undetectable
truncated 200. No new surface area; end-to-end / regression tests for each.

### Fixed
- **`tokenctl up` now admits real traffic and enforces the wallet cap.**
  `runProxy` built the budget tree with `budget.NewTree` but never called
  `tree.BindAll(cfg.APIKeys)` or `tree.SetWallet(cfg.Wallet)`, so the live proxy
  launched via `tokenctl up` had an empty `leafByKey` — every request missed the
  leaf lookup and got `401 unknown_key` — and a nil `walletBudget`, making the
  advertised org wallet cap a silent no-op. Only the unit tests worked, because
  they call `tr.Bind` directly. `runProxy` now binds all configured keys and the
  wallet immediately after `NewTree` (the wallet call is a no-op when no
  `wallet` block is configured, so wallet-less configs still start) (HIGH).
- **A mid-stream preempt is no longer an undetectable truncated 200.** Pre-header
  preemption already emits `499` + `X-TokenCtl-Reason: preempted_by_sibling`, but
  once response headers were flushed the 200 status line could not be rewritten,
  so an arbiter preempt mid-stream just closed the connection — a truncated body
  indistinguishable from a normal short completion. The SSE metered reader now
  detects a preempt after streaming has begun, injects a terminating
  `event: error` / `data: {"reason":"preempted_by_sibling"}` SSE frame into the
  client stream, and fails the copy with a non-EOF error so the stream ends
  non-gracefully (not a clean EOF). Which path fired — pre-header `499` vs
  mid-stream error frame — is recorded on the new `tokenctl_preempt_signals_total`
  counter (`path="pre_header"` / `path="mid_stream"`) so both branches are
  observable (MEDIUM).

### Added
- Go tests: `cmd/tokenctl/main_test.go` starts the real `runProxy` from an
  on-disk config against an httptest upstream and asserts a bound key is admitted
  (non-401, reaches the upstream, stamped with its leaf) and that the wallet cap
  hard-denies over-cap traffic with `X-TokenCtl-Reason: budget_exceeded`, plus a
  wallet-less config still starts and admits; `internal/proxy/mid_stream_preempt_test.go`
  covers the injected error frame at the reader level (including a client buffer
  smaller than the frame) and end-to-end through the reverse proxy, asserting the
  client sees a 200 whose body ends in the `event: error` frame and that the
  `mid_stream` preempt-signal counter incremented.

## [0.4.0] - 2026-07-03

Correctness-hardening pass. Two targeted fixes found by re-reading the shipped
source — both in tokenctl's own budget-tree lane, no new surface area — plus Go
regression tests for each.

### Fixed
- **A concurrent agent swarm can no longer overshoot the org wallet cap.** The
  v0.2.0 node-level swarm-overshoot fix (a per-request in-flight reservation
  counted at admission) was never applied at the wallet layer: the wallet
  admission gate read only already-credited spend, so N concurrent agents spread
  across generous leaves all passed the wallet gate at `walletConsumed≈0` and
  overshot the org cap unboundedly — the exact overshoot v0.2.0 closed for nodes,
  reopened at the wallet. The wallet gate now reserves an in-flight estimate per
  admission (mirroring the node reservation) and compares `consumed+reserved`
  against the ceiling, drawing the reservation down on attribution and releasing
  the remainder on `Release` (HIGH).
- **Per-provider spend now resets with the wallet window.** `providerConsumed`
  was a lifetime-monotonic map — only ever incremented, never reset — while
  `walletConsumed` resets to 0 on every wallet-window rollover. After the first
  boundary they diverged permanently, so `walletGuard` preempted whichever
  provider was heaviest in window 1 rather than the current window's actual
  heaviest spender, and the `tokenctl top` per-provider view misreported
  current-window spend. Per-provider counters now reset on every wallet rollover
  (the centralized arbiter path and both lazy rollover branches) so
  `sum(per-provider) == walletConsumed` holds across boundaries (MEDIUM).

### Added
- Go regression tests: a deterministic wallet-reservation test proving the
  reservation is placed at admit, seen by the next admit's effective-load check,
  bounds admits to `ceiling/reserve`, and is released on `Release`; a concurrent
  wallet-overshoot test mirroring the node-level reference; and a per-provider
  reset-on-rollover test covering the arbiter path and both lazy rollover
  branches.

## [0.3.0] - 2026-06-30

Correctness-hardening pass. Three targeted fixes found by re-reading the shipped
source — all in tokenctl's own proxy / store / config lane, no new surface area —
plus Go regression tests for each.

### Fixed
- **Streamed token metering now works on CRLF-framed SSE.** The SSE meter split
  events only on `\n\n`, but real Anthropic/OpenAI gateways and TLS-terminating
  proxies frequently frame events with `\r\n\r\n` (CRLF) — which contains no `\n\n`
  substring — so a CRLF stream was never split incrementally. Every event piled up
  until EOF and collapsed into a single pseudo-event carrying only the last `event:`
  name, dropping `message_start` usage and starving the arbiter of the mid-stream
  signal it preempts on. The reader now frames on both `\n\n` and `\r\n\r\n` (HIGH).
- **A failed counter flush no longer loses the window's spend.** `Store.flush()`
  swapped the dirty set out and cleared it *before* the BoltDB transaction ran, so
  any failed `db.Update` (disk full, transient bbolt error) permanently dropped that
  window's per-node *and* `__wallet__` counters — silently resetting the hard cap
  toward 0 on the next restore, re-opening the v0.2.0 crash-safety guarantee through
  the flush-error path. Unwritten records are now merged back into the dirty set on a
  transaction error so the next flush tick retries them (HIGH).
- **Over-subscribed budget trees are rejected at load.** `Validate()` never checked
  the core invariant `sum(child.budget) <= parent.budget`, so a tree where the teams'
  budgets summed above the org ceiling loaded silently and then enforced incoherent
  deny/throttle decisions against the very invariant the hierarchy promises. Such a
  config is now rejected at load with a clear error, on every node, not just the root
  (MEDIUM).

### Added
- Go regression tests: `sse_crlf_test.go` (LF + CRLF + chunked-read framing),
  `flush_retry_test.go` (re-queue on transaction error + retry lands),
  `budget_invariant_test.go` (over-subscription rejected at root and inner nodes;
  Sample config stays valid).

## [0.2.0] - 2026-06-23

Enforcement-correctness pass. v0.1 shipped the three milestones but several of the
guarantees the README advertises only held on paper; this release makes them hold
end-to-end. No new surface area — five targeted fixes plus Go regression tests.

### Fixed
- **Preemption now actually tears down the upstream call.** The arbiter cancelled the
  admission context, but the reverse proxy served the *original* client request and
  never injected `Admission.Context()`, so an m3 preempt was a no-op: the upstream
  kept streaming and the client still got a 200. The proxy now runs the upstream call
  under the admission's context and emits `499` + `X-TokenCtl-Reason: preempted_by_sibling`
  when the arbiter fires (HIGH).
- **The org wallet counter survives a crash.** `walletConsumed` was only flushed in
  `Close()`, so a SIGKILL / OOM between windows lost the whole window's org-level spend
  and reloaded the hard cap as 0 — the exact opposite of what a budget enforcer must
  guarantee. The wallet is now `SaveCounter`'d on every attribution (HIGH).
- **Concurrent admits can no longer overshoot the hard ceiling.** `Admit` checked only
  already-credited `consumed`, but tokens are credited asynchronously as the response
  streams, so an agent swarm all admitted at `consumed≈0` and then each streamed
  millions. Each request now reserves a per-request in-flight estimate that counts
  toward the deny/throttle comparison and is reconciled as real tokens arrive (or
  released on request end), bounding overshoot.
- **Window rollover is coherent across the tree.** Each node lazily reset its own window
  on first touch, so parent and child `windowStart` drifted across a boundary and broke
  the documented `sum(child.consumed) <= parent.consumed` invariant. Rollover is now
  driven from a single `now` (arbiter tick / whole-chain reset) so the tree rolls over
  together.
- **The buffered-JSON meter no longer holds the whole body in memory.** `jsonMeteredReader`
  buffered the entire non-streamed response just to read `usage` on EOF, scaling memory
  with concurrency × body size (an OOM/DoS vector). It now retains a bounded tail and
  reconstructs the trailing `usage` object from it, so per-request memory is capped
  regardless of response size.

### Tests
- `internal/proxy/preempt_wiring_test.go` — preempt cancels the upstream and returns 499.
- `internal/budget/tree_test.go` — wallet counter persisted across a simulated reload;
  concurrent admits respect the hard ceiling via reservation; coherent window rollover
  preserves the parent ≥ sum(children) invariant.
- `internal/proxy/metered_reader_test.go` — bounded buffer with usage still metered
  correctly on multi-megabyte bodies.

## [0.1.0] - 2026-06-05

First public cut. Three milestones land together as the v0.1 control plane.

### Added
- **m1 — proxy + meter.** HTTPS forward proxy in front of Claude / OpenAI traffic,
  streaming per-key input/output token accounting, Prometheus metrics, and a
  `tokenctl top` live view.
- **m2 — budget tree.** YAML-defined `org → team → dev` quota tree with weighted
  allocation, soft-throttle (delay queue) at 80% of a node's quota, and hard-deny
  (HTTP 429 + `X-TokenCtl-Reason`) past 100%.
- **m3 — preemption + arbitration.** In-flight cancellation of low-weight requests when
  a high-weight sibling needs headroom, plus multi-provider arbitration across
  Claude / OpenAI / Bedrock on a single shared wallet.
- BoltDB-backed persistence for counters and an append-only audit log.
- Bilingual README (简体中文 primary, English sibling), Apache-2.0 license, GitHub
  Actions CI.

[Unreleased]: https://github.com/SuperMarioYL/tokenctl/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/SuperMarioYL/tokenctl/releases/tag/v0.1.0
