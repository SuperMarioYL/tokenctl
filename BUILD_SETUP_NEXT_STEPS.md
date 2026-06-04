# Next steps — manual one-time setup

No external publish services (PyPI / npm / crates.io / Gitee mirror) are wired into this
build, so there is no third-party onboarding to complete. The CI workflow uses only the
stock GitHub-hosted Actions, which need no setup.

A few human-only steps remain before this repo is live:

## 1. Verify the build locally (Go toolchain was absent on the build machine)

This repo was scaffolded on a host without the Go toolchain, so `go build` / `go test`
were **not** run during the build. Verify before pushing:

```bash
cd workspace/builds/tokenctl--t8j0k1l2
go mod download
go vet ./...
go build ./...
go test -race -count=1 ./...
```

Fix any signature drift the offline build couldn't catch, then continue.

## 2. Create the GitHub repo and push

The git history is authored by your global git identity
(`supermario-leo <leo.stack@outlook.com>`). Create the remote and push:

```bash
gh repo create tokenctl --public --source=. --remote=origin \
  --description "cgroups for LLM tokens — hierarchical, preemptive token-budget controller for agent quota"
git push -u origin main
gh release create v0.1.0 --title "tokenctl v0.1.0" --notes-from-tag
```

## 3. (Optional) Record the demo asset

`assets/demo.tape` is a [VHS](https://github.com/charmbracelet/vhs) script. Once the
binary builds, render the GIF the README links:

```bash
go build -o ./tokenctl ./cmd/tokenctl
vhs assets/demo.tape          # -> assets/demo.gif
# or, for an asciinema cast:
# asciinema rec assets/demo.cast
```

## 4. (Commercial tier) Stand up the hosted control plane

The README's `付费 / Pricing` section points at a hosted multi-tenant tier (deferred from
v0.1's single-binary scope). That's a separate build — see `go_to_market.md` §8 in the
plan directory for the first-paid-customer path.
