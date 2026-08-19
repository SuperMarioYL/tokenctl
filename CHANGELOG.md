# Changelog

All notable changes to tokenctl are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.13.0] - 2026-08-20

Correctness-fix iteration, fixes-only. Two medium-severity bugs found by
re-grilling the shipped v0.12.0 source — both in the metering and tier
enforcement paths, no new surface area. Each fix ships with Go regression
tests (unit + integration) that fail on the unfixed code.

### Fixed
- **Anthropic prompt-cache input tokens are now metered, not dropped.**
  `claudeUsage` (internal/providers/claude.go) only declared `input_tokens`
  and `output_tokens`, so `json.Unmarshal` silently dropped the
  `cache_creation_input_tokens` and `cache_read_input_tokens` fields that
  Anthropic's `message_start` usage object carries separately. Prompt caching
  is on by default for Claude Code (the plan's named primary audience), where
  the cached prompt routinely runs 10-50x larger than `input_tokens`, so a
  cached turn under-counted input by that factor and the org cap, per-leaf
  budgets and the Opus tier sub-ceiling effectively never bit for cached
  traffic; `tokenctl export` under-counted the same field on its finance
  reconciliation — an undocumented metering gap vs the README's "token-by-token"
  claim. `claudeUsage` now declares both cache fields and `advance()` advances
  the input HWM against their sum with `input_tokens` so every billed input
  token is attributed once (the HWM diff stays correct because `message_start`
  reports all three once and `message_delta` reports output only). The two
  snake_case fields are mirrored on `bedrockUsage` for Anthropic-on-Bedrock
  (MEDIUM).
- **Model-tier `soft_throttle_at` is now honoured, not silently dropped.**
  The tier sub-ceiling's `soft_throttle_at` was validated at config load
  (required in `(0,1]`, defaulted to `0.8`) but never reached the runtime:
  `SetModelTiers` (internal/budget/tree.go) copied only `budget` and `windowD`
  onto `tierState` and dropped `SoftThrottleAt`, and `Admit`'s tier branch only
  hard-denied. There was no tier soft-throttle path, unlike the node path and
  the wallet path which both honour `SoftThrottleAt`. An operator who set
  `soft_throttle_at: 0.5` on an Opus tier had the value accepted, validated and
  then discarded — sibling agents ran straight to the 100% hard cap with no
  `Retry-After` the node/wallet paths would have emitted at 50%. `SetModelTiers`
  now copies `SoftThrottleAt` onto `tierState`, and `Admit` runs a tier
  soft-throttle check in the soft-throttle section (after all hard-denies, so
  "hard wins over soft" holds) mirroring the node loop; a `softThrottleAt==0`
  tier skips the check so it cannot always-throttle (MEDIUM).

## [0.12.0] - 2026-08-17

Correctness-fix iteration, fixes-only. Two medium-severity bugs found by
re-grilling the shipped v0.11.0 source — both in the budget tree's audit and
persistence paths, no new surface area. Each fix ships with a Go regression
test that fails on the unfixed code.

### Fixed
- **Audit-log write failures are no longer silently swallowed.**
  `appendAudit` discarded the `state.AppendAudit` error via `_ =`, even though
  the store's `AppendAudit` is a synchronous bbolt write transaction
  (state.go:159-177) that can fail on disk-full, a transient bbolt error, or a
  closed DB, and the store package's own doc-comment (state.go:17-18) warns
  that losing an audit event "is a compliance hole." When the tx failed the
  event was lost with no log, metric, retry, or operator signal — an asymmetry
  with the already-fixed counter-flush path (which re-queues records on tx
  error). Every admission lifecycle event (admit/deny/throttle/preempt/release)
  flows through this path, so a sustained bbolt failure silently blanked the
  audit log the `tokenctl export` reconciler depends on. `appendAudit` now
  logs the failure (`slog.Error` with the event kind + group + error) and
  increments `tokenctl_audit_write_failures_total`; the admission path does not
  panic and the hot path stays synchronous (MEDIUM).
- **Model-tier sub-ceiling counters now persist across a restart.**
  `attribute()` credited the per-tier windowed counter (`ts.consumed += n`) but
  never called `state.SaveCounter` for the tier — unlike the node chain
  (persisted on every attribution) and the org wallet (also persisted).
  Symmetrically, `SetModelTiers` built each `tierState` with a fresh
  `windowStart=now` / `consumed=0` and never called `state.LoadCounter` —
  unlike `buildNode`, which restores each node's counter from the store. The
  per-tier hard sub-ceiling (`feat_model_tier_override`) was therefore the
  ONLY budget counter not durable across restarts: a crash / SIGKILL / OOM /
  deployment mid-window reset the tier consumed to 0 and started a fresh tier
  window, so an Opus-tier hard cap of N tokens/window silently allowed up to
  ~2x the intended spend (N before restart + N after) in the same wall-clock
  window. `attribute()` now persists the tier counter via `SaveCounter` with a
  stable `__tier__`+name key, `SetModelTiers` restores it via `LoadCounter`
  (mirroring `buildNode`), and `flushAll()` persists tier counters for symmetry
  with node + wallet (MEDIUM).
## [0.11.0] - 2026-08-13

Correctness-fix iteration. Two high-severity bugs found by re-grilling the
shipped v0.10.0 source — both in the wiring between the CLI and the budget
tree / export surface, no new surface area. Each fix ships with a Go
regression test that fails on the unwired code.

### Fixed
- **`tokenctl export` no longer panics on a config with no `pricing` block.**
  `runExport` unconditionally dereferenced `cfg.Pricing.Models`, but
  `Config.Pricing` is a `*PricingConfig` (yaml `omitempty`) that `applyDefaults`
  never initializes, and is absent from both `tokenctl init`'s `Sample()` and the
  shipped `configs/tokenctl.example.yaml`. The `export_cli_test` fixtures always
  injected a pricing block, so the nil path was untested — a bare
  `tokenctl export -c tokenctl.yaml` on the shipped init/example config panicked
  with a nil-pointer dereference on the finance-reconciliation surface the
  command exists for. `runExport` now nil-guards `cfg.Pricing` before the deref;
  unpriced traffic already contributed 0 to `cost_estimate` (the reconciler
  tolerates an empty price table), so a no-pricing config now emits a valid
  zero-pricing export instead of panicking (HIGH).
- **`tokenctl up` now actually enforces per-model-tier overrides.**
  `runProxyCtx` wired `BindAll` + `SetWallet` but never called
  `tree.SetModelTiers(cfg.ModelTiers)`, so `t.tiers` stayed empty and
  `resolveTier()` returned `nil` for every model — the entire v0.9.0
  per-model-tier override feature (`cost_multiplier` +
  `budget_tokens_per_window` sub-ceilings, e.g. capping an Opus swarm
  independently) was silently never enforced at runtime, even though
  `config.Load` validates the block. Unit tests masked it because they call
  `SetModelTiers` directly, bypassing the CLI path. `runProxyCtx` now calls
  `tree.SetModelTiers(cfg.ModelTiers)` after `SetWallet`; the call is a no-op on
  an empty slice and idempotent, so configs without a `model_tiers:` block are
  unaffected (HIGH).

### Added
- Go regression tests: `cmd/tokenctl/export_cli_test.go` gains
  `TestExportCLINoPricingBlock`, which runs `runExport` on a config with no
  `pricing:` block (mirroring `tokenctl init`'s `Sample()` + the example config)
  and asserts it does not panic and reconciles the seeded release to a
  zero-`cost_estimate` row; `cmd/tokenctl/main_test.go` gains
  `TestRunProxyWiresModelTiers`, which starts the real proxy via `runProxyCtx`
  against a config with an opus `model_tiers` sub-ceiling and asserts the
  tier-capped Opus request is hard-denied with `429` +
  `X-TokenCtl-Reason: budget_exceeded_tier` and never reaches the upstream —
  failing on the unwired code (the request is admitted `200`).

## [0.10.0] - 2026-08-10

Release-reconciliation iteration. The live repo shipped v0.5.0 through v0.9.0
(five releases, 2026-07-06 to 2026-08-05) with no base-plan milestone tracking;
this release backfills regression + integration coverage for that already-shipped
behaviour so further feature work lands on a guarded baseline. No new surface
area, no behaviour change — the proxy metering (m1), tree soft/hard throttle
(m2), and preemption (m3) behaviour is unchanged; this release only wraps it in
tests and tightens the release surface.

### Added
- **Table-driven integration tests for the shipped proxy metering (m1).**
  `cmd/tokenctl/metering_integration_test.go` streams a canned Claude SSE response
  (`message_start` + cumulative `message_delta`) and a buffered JSON response
  through the REAL `runProxy` path against an httptest upstream and asserts the
  metered input/output tokens are attributed to the bound leaf and surface in
  `/v1/snapshot`, so the streamed-token attribution the product headline rests on
  is regression-guarded end-to-end (not just at the reader unit level).
- **Table-driven end-to-end admit-contract tests for the tree weighting (m2).**
  `cmd/tokenctl/proxy_contract_test.go` pre-seeds the BoltDB counters, starts the
  real proxy, and asserts the status code + `X-TokenCtl-Reason` header for every
  shipped admission outcome — admitted (200), unknown_key (401), missing_api_key
  (401), soft_throttle (429), leaf budget_exceeded (429), and wallet budget_exceeded
  (429) — pinning the m2 contract through the real CLI path the operator runs.
- **Table-driven `tokenctl export` CLI tests (csv + json).**
  `cmd/tokenctl/export_cli_test.go` exercises the `runExport` entrypoint across
  format (csv|json), window defaulting, and invalid format/window rejection, so the
  finance-reconciliation surface is guarded at the command level (not just the
  formatter/reconciler unit level).
- **Table-driven admit-precedence + reset-policy contract tests.**
  `internal/budget/admit_precedence_test.go` pins the precedence order the m2/m3
  arbiter depends on (tier hard-deny > node hard-deny > wallet hard-deny > node
  soft-throttle > wallet soft-throttle) via a capturing audit state;
  `internal/budget/reset_policy_table_test.go` consolidates the hard / rollover /
  grace rollover contract into one table.
- **`--version` flag.** `tokenctl --version` now prints the bare release tag
  (`tokenctl v0.10.0`) for release-pinning scripts; the `tokenctl version`
  subcommand retains the commit / OS / toolchain detail.

### Changed
- `VERSION` and the `main.Version` constant bumped to `v0.10.0`.

## [0.9.0] - 2026-08-05

Feature release. Three additive surfaces on top of the existing m1/m2/m3
behaviour: per-model-tier budget overrides, a finance-reconciliation export,
and a configurable budget window reset policy. No behaviour change to the
proxy metering, tree throttle, or preemption paths.

### Added
- **Per-model-tier budget overrides.** A new `model_tiers` config block maps a
  model-name regex to a `cost_multiplier` (for spend weighting) and/or a nested
  `budget_tokens_per_window` sub-ceiling enforced as a hard 429 when that tier
  alone crosses it. The tier window rolls over independently of the node window,
  so a spike on an expensive model can be throttled without resetting the whole
  node (feat_per_model_tier_override).
- **`tokenctl export` finance-reconciliation surface.** A new `tokenctl export
  --window <id> --format csv|json` subcommand compiles the `model_tiers`
  resolver + pricing table and emits a per-(team, provider, model_tier) spend
  table whose token sums reconcile against `tokenctl top`'s windowed snapshot
  and whose `cost_estimate` column is a hand-verifiable price join
  (feat_cost_export_reconcile). Backed by the new `internal/audit` reconcile
  package, deliberately decoupled from config + the budget tree to avoid
  import cycles and stay unit-testable against an in-memory fixture store.
- **Configurable budget window reset policy.** A per-node `reset_policy`
  (`hard` | `rollover` | `grace`) now controls how a node window resets: `hard`
  zeroes spend on rollover, `rollover` carries surplus headroom into the next
  window via a carry offset, and `grace` extends the prior window by a
  configurable grace period before rolling over (feat_budget_reset_policy).
- Go tests: `internal/budget/tier_test.go`, `internal/budget/reset_policy_test.go`,
  `internal/config/features_test.go`, `cmd/tokenctl/export_test.go`, and
  `internal/providers/model_test.go` pin the new model-tier, reset-policy,
  config-feature, export, and provider-model resolution surfaces.

## [0.8.0] - 2026-08-04

Provider-metering correctness pass. One fix found by re-reading the shipped
source: the OpenAI Responses API streaming responses were attributed zero
tokens (silently bypassing the org cap and node budgets), and a mid-stream
preempt on a non-streamed JSON response was an undetectable truncated body.

### Fixed
- **OpenAI Responses API streaming responses are now metered.** The Responses
  API streams typed SSE events (`response.created`, `response.output_text.delta`,
  `response.completed`, ...) and only the terminal `response.completed` carries
  usage, nested under `response.usage` rather than at the top level. The OpenAI
  meter's `Observe` blanket-returned 0 for any named event and only inspected
  top-level `usage`, so a streaming `/v1/responses` request was attributed zero
  tokens and silently bypassed the org cap and node budgets. `Observe` now
  accepts the `response.completed` event and parses the nested `response.usage`
  (MEDIUM).
- **A mid-stream preempt on a non-streamed JSON response is now observable.**
  Pre-header preemption already emits `499` + `X-TokenCtl-Reason`, but once a
  JSON response's headers were flushed a mid-stream preempt just aborted the
  copy — a truncated body the client fails to parse, indistinguishable from a
  network error and invisible to the `tokenctl_preempt_signals_total` counter.
  The buffered-JSON meter now records the `mid_stream` preempt path on the
  counter (alongside the existing `pre_header` path) so both branches stay
  observable (MEDIUM).

### Added
- Go tests: `internal/proxy/responses_meter_test.go` covers the Responses API
  `response.completed` nested-usage parse, and `internal/proxy/mid_stream_preempt_test.go`
  gains the JSON-body mid-stream preempt case asserting the `mid_stream` signal
  counter increments.

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

[Unreleased]: https://github.com/SuperMarioYL/tokenctl/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/SuperMarioYL/tokenctl/releases/tag/v0.1.0
