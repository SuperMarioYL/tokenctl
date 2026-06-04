# Changelog

All notable changes to tokenctl are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/supermario-leo/tokenctl/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/supermario-leo/tokenctl/releases/tag/v0.1.0
