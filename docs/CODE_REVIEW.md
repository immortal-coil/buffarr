# Code Review: What to Do

A self-review of the backend (`cmd/`, `internal/`), done because the `advisor`
tool wasn't available. Everything below was checked directly: `go build`,
`go vet`, `go test ./... -cover` all pass clean, and there are no `TODO`/
`FIXME` markers left in the code. The codebase overall is in good shape —
small dependency footprint, consistent "why" comments, optimistic-delete +
background-purge patterns that are actually load-tested against a real
3.7 TB cache. What follows is what's missing or worth tidying, ranked by
what actually matters.

## Do first: add a CI gate

There is no `.github/workflows/` (or any CI config) in this repo. Right now
`go build`, `go vet`, and `go test` only run when a human remembers to run
them locally before pushing. A broken build or a regressed test can land on
`master` and isn't caught until the next `./deploy.sh`, on a production box.

This is the highest-value, lowest-effort item here: a ~15-line GitHub Actions
workflow (`go build ./...`, `go vet ./...`, `go test ./...`) on push/PR would
have caught nothing retroactively (the repo is clean) but closes the gap for
every commit going forward.

## Do next: split `internal/web/server.go`

It's 2,469 lines — roughly 35% of the entire non-test Go source in the
project — holding ~70 handler methods on one `Server` struct: auth/session,
library CRUD, bulk operations, CDN toggles, prefill queue, guest purge,
settings, poster fetch, and the cache-max enforcer, all in one file.

None of this is a correctness problem (tests pass, `go vet` is clean, the
logic is sound), and Go doesn't care about file boundaries within a package —
so this is a pure, low-risk mechanical split, not a redesign. Recommended
split, same package (`internal/web`), just new files:

- `routes.go` — `Handler()`, `setupGate`, `authGate`, `csrf`, session helpers
- `handlers_auth.go` — login/callback/logout/setup
- `handlers_library.go` — library, game detail, monitor, delete, bulk ops
- `handlers_guest.go` — guest page/rows/purge
- `handlers_prefill.go` — prefill page/trigger/status/queue
- `handlers_settings.go` — settings page + all `/settings/*` handlers
- `handlers_cdn.go` — CDN page/toggle
- `poster.go` — poster fetch/serve
- `cache_enforcer.go` — `enforceCacheMax`, `evictDownTo`, `RunCacheEnforcer`

Do this with a mechanical move (cut/paste the funcs verbatim, adjust nothing)
so `git blame` stays intact and there's zero behavior risk. Not urgent — the
file works — but it's the thing that will slow down the next feature or the
next reviewer the most.

`internal/catalog/catalog.go` has the same shape at a smaller scale: 1,114
lines mixing library/games, bandwidth tallying, the prefill queue, platform
toggles, generic settings, and tailer state persistence. Same treatment
(`games.go`, `bandwidth.go`, `prefill_queue.go`, `platforms.go`,
`settings.go`, `tail_state.go`) would help, but it's a smaller win — leave it
until `server.go` is done or someone's actively working in that file.

## Worth doing: lint beyond `go vet`

No `golangci-lint` or `staticcheck` config exists, and neither is installed
in this environment. `go vet` catches a narrow class of mistakes (mostly
`printf`-arg and struct-copy issues); `staticcheck`/`golangci-lint`'s default
set catches unused code, simplifiable expressions, and a wider range of real
bugs (e.g. `S1*`/`SA*` checks) for close to zero setup cost. Add it to the CI
workflow from the first item once it exists.

## Worth doing: `govulncheck` in CI

`go.mod` has a small, mostly-indirect dependency set (sqlite driver and its
transitive deps, `google/uuid`, `dustin/go-humanize`) — good, nothing to trim
here. But there's no dependency vulnerability scan anywhere. `govulncheck` is
a single `go install` + one CI step; worth adding alongside the lint step.

## Noted, not urgent

- **`internal/web` has the lowest test coverage in the repo (57.2%)** — also
  the largest file. Splitting the file (above) will make it obvious which
  handler groups are under-tested; fix coverage as a byproduct of the split
  rather than as a separate pass.
- **`nginx.PurgeQueue.isPendingLocked`** (`internal/nginx/purgequeue.go:76`)
  is an O(n) linear scan per `Enqueue` call under the queue's mutex. Fine at
  today's scale (a handful of pending prefixes between drains); if purge
  volume ever grows a lot, swap `pending []string` for a `map[string]struct{}`.
  Not worth doing pre-emptively.
- **`steamauth.Manager`'s session map has no active sweep** — expired
  sessions are only evicted lazily, when someone presents that exact stale
  cookie again. For a single-operator tool with a handful of live sessions at
  most, this is bounded and harmless. Only worth a periodic sweep if the
  trust model ever changes to multi-operator.
- **`docs/FAQ.md` and `internal/web/templates/about.html`'s FAQ section are
  manually kept in sync** (documented in both files' headers). It's worked so
  far and the entries are small in number; not worth building a
  template-generation step for it unless the FAQ list grows a lot or drifts
  in practice.

## Skip

- No dead code, no unused abstractions, no speculative interfaces found —
  the existing `SteamLookup`/`DepotsForApp`/`StateStore` interfaces all have
  a real second caller (tests) or a real nil-vs-real-implementation split
  (Steam resolution being optional). Nothing to cut here.
- Security posture (session cookies, CSRF, hand-rolled OpenID with the
  `check_authentication` re-verification step, first-boot-operator-claim
  window) is already deliberately reasoned through in comments and matches
  what a LAN single-operator tool needs. No changes recommended.
