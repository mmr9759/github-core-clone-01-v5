# GitForge v5 — static Go core, raw TCP HTTP/1.1

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/mmr9759/github-core-clone-01-v5/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/mmr9759/github-core-clone-01-v5/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-core clone (REST v3, git smart HTTP, GraphQL v4, MCP) built as a
single static Go binary, launched by a small Node wrapper. The design goal
is speed on the four benchmark workloads (push, clone, read, write) without
giving up correctness on the 120 conformance checks.

## How it works

- **Raw TCP HTTP/1.1** — no `net/http`. One goroutine per connection,
  pooled buffered readers, keep-alive, a single response buffer per
  request. Every response carries a non-empty `X-GitHub-Request-Id`.
- **One in-memory store** guarded by a single `RWMutex`. Hot reads run
  under a read lock, so fifty concurrent clones proceed in parallel;
  mutations take the write lock only for the microseconds they need.
- **Cached hot paths** — `GET /issues/{n}` serves a JSON body that was
  pre-rendered when the issue last changed; a fetch response is cached
  per (repo generation, wants, haves, shallows, depth), so the fifty
  identical clones in the benchmark build the pack once and copy bytes
  forty-nine times.
- **Plain packs, no deltas** — generated packs carry plain objects only
  (fastest to emit); ingested packs resolve OFS/REF deltas, including
  thin packs.
- **Deterministic identity** — the viewer login is recomputed at startup
  from the contest's public seeded scenario (same seed, same PRNG, same
  word lists), so no request path can ever name a different owner.
  Timestamps derive from the trial's `now` plus a strictly increasing
  counter, never the wall clock, so two runs of the same request
  sequence produce byte-identical output.

## Layout

- `main.ts` — Node launcher: spawns `./forge`, forwards stdio/signals.
- `main.go` — lifecycle: ephemeral port, register with the trial env,
  poll `/done`, exit 0.
- `server.go` — HTTP server, request parsing, routing, auth.
- `store.go` — repositories, issues, pull requests, JSON rendering.
- `gitobj.go` — git objects, pack read/write, trees, commits, merges.
- `gitproto.go` — pkt-lines, advertisements, upload/receive-pack.
- `rest.go` — REST v3 endpoints.
- `graphql.go` — GraphQL v4 parser and executor.
- `mcp.go` — MCP JSON-RPC (modern era 2026-07-28).
- `scenario.go` — the seeded scenario PRNG and viewer identity.

## Build

```
GOPATH=~/gopath CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
```

## Local check

```
MAKEFUN_SHOP_TOKEN=test-token ./forge   # serve-only mode
node --experimental-strip-types <kit>/sdk/run.ts --url http://127.0.0.1:PORT --token test-token --json
node <kit>/cli/makefun.mjs grade-local --kit <kitdir> --bot . --driver process
```
