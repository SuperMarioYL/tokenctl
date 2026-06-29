# Changelog

All notable changes to tokenctl are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/SuperMarioYL/tokenctl/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/SuperMarioYL/tokenctl/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/SuperMarioYL/tokenctl/releases/tag/v0.1.0
