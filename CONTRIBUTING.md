# Buffarr QA / QC

What "verified" means for a change to Buffarr. A change is not done when the code
is written; it is done when it **builds, its tests pass, and (if deployed) it has
been confirmed live against the specific artifact it changed**. This file is the
checklist for getting there, with the project-specific traps called out.

Bias toward caution over speed. For a one-line doc fix, use judgment; for
anything touching the catalog, the prefill lifecycle, nginx config, auth, or a
deploy, run the full profile.

---

## 1. Local gates (every code change)

Run from the repo root before committing:

```bash
gofmt -l .                 # must print nothing (formatting clean)
go vet ./...               # must be clean
go build ./...             # must be clean
go test ./...              # must be clean
```

- **Templates are compiled at startup**, not at `go build`. A malformed
  `{{...}}` only fails when `parseTemplates()` runs, which the web tests exercise
  via `newTestServer`. So a template change without a passing web test is
  unverified.
- **CSS changes need a cache-bust.** Pages reference `app.css?v=N`. If you edit
  `static/app.css`, bump the `?v=` on the pages that rely on the change, or
  inline the style on the element (the project does both). Forgetting this ships
  CSS that browsers serve stale.

## 2. Testing standards

- **Assert on observable surfaces, not internals**: rendered HTML substrings,
  catalog rows queried back, sidecar lock state (`IsRunning`). Mirror the
  existing table/`httptest` style.
- Reuse the harnesses: `openTestCatalog` (catalog), `newTestServer` +
  `authedRequest` (web, with session + CSRF), `writeLogFile` (ingest).
- **Every new route must be accounted for in the auth model.** Protected routes
  go in the enumeration in `TestAuthGate_ProtectedRoutesRedirectWhenUnauthenticated`.
  A public route is an explicit, reviewed addition to `publicPaths` (and
  `setupGate`'s allowlist) with a test proving it is reachable unauthenticated.
  Auth is gated by construction (the whole mux is wrapped), so an un-enumerated
  route is the thing that slips through.
- **Async / worker code must be testable without sleeping.** Inject the poll
  interval and expose a deterministic step the test can drive. Simulate sidecar
  completion by writing the `status.json` / `last-run.log` files into the test's
  prefill dir, as the existing prefill tests do. Never `time.Sleep` for a result.
- A passing test suite proves your tests pass, not that the change is correct.
  For anything in section 4, add the test that would fail if the change were
  wrong, then make it pass.

## 3. Deploy verification

Deploys are manual, image-based, and have real blast radius. Order:

1. **Warn about the logout.** A backend redeploy recreates the container, and
   sessions are in-memory. **Every backend deploy logs the operator out.** Say
   so before doing it.
2. **Pin a rollback.** Before pushing `:latest`, tag the currently-deployed prod
   image as `rollback-YYYYMMDD` on the prod host, so a bad deploy is one
   `docker tag` away from recovery.
3. **Build → push → pull → recreate** only the services that changed: backend
   changes recreate the backend (`up -d --no-deps backend`). nginx/dnsmasq
   changes recreate those (and briefly interrupt caching/DNS, not sessions).
4. **Verify the actual artifact, not a generic 200/303.**
   - **Smoke-test caveat:** an unauthenticated `curl /` returns **303 from the
     auth gate before the handler ever runs**, so a clean 303 proves nothing
     about your handler. Verify the thing you changed:
   - New route → it returns the expected status/body (authed if needed).
   - nginx config value → exec into nginx and `grep` the rendered
     `/etc/nginx/http.d/*.conf` for the new value.
   - New catalog table/column → confirm it exists on the **existing prod DB**
     (see section 4; tests can't catch a migration that doesn't apply).
   - DNS/cache change → `nslookup` a CDN host still resolves; cache `/healthz`
     is 200.

## 4. High-care subsystems (extra QA required)

These have failure modes the default gates do **not** catch.

- **Catalog migrations.** `Open()` runs the whole `schema` const unconditionally
  on every boot, so a new `CREATE TABLE IF NOT EXISTS` lands on existing DBs
  automatically. A new **column** needs `ensureColumn` (the `IF NOT EXISTS`
  create never alters an existing table). Tests always use a fresh DB, so they
  **cannot** tell you a migration applied to a populated prod DB. Confirm it
  post-deploy by querying the table/column directly. A failed migration is often
  silent (a query error swallowed into an empty/zero UI state), not a crash.
- **The prefill lifecycle** (sidecar single-flight, `pending_prestage`
  reconciliation, the batch-queue worker). The sidecar runs one job at a time;
  the in-process waiter dies on restart and `reconcilePrestage` /
  worker-recovery picks the result back up. Any change here must be tested for
  **restart recovery** (an in-flight job resolving after the process that
  started it is gone), and deployed deliberately.
- **nginx config.** `inactive`/`max_size`/`min_free`/`keys_zone` are
  envsubst-templated. Any new `${VAR}` **must** be added to the `ENVSUBST_VARS`
  allowlist in `docker/nginx/entrypoint.sh` and given an `export` default, or it
  renders blank and `nginx -t` fails. Validate the render before deploying:
  `envsubst '<allowlist>' < conf` and eyeball the output. `nginx -t` runs at
  container start and fails loudly, so a bad template crash-loops nginx.
- **Eviction / PURGE.** The backend is the primary, **Lock-aware** evictor;
  nginx's `max_size`/`min_free`/`inactive` are Lock-unaware backstops. Never
  auto-evict a locked game; never lower `inactive` into expiring monitored
  content. Changes here need a test that locked content survives.

## 5. Security checklist

- **No secrets in tracked files or history.** Before committing, and especially
  before any public exposure:
  ```bash
  git ls-files | grep -E '(^|/)\.env$'                         # expect empty
  git grep -nIE 'STEAM_WEB_API_KEY=.+|SESSION_SECRET=.+|NGINX_PURGE_SECRET=.+' -- ':!*.example'
  git grep -nIE '\b[0-9a-f]{32,}\b' -- ':!*_test.go' ':!go.sum'
  ```
  `.env` is gitignored. `.env.example` holds blank placeholders only. Never paste
  secret values into docs, logs, issues, or chat.
- **New routes are private by default** (the mux is wrapped in `authGate`).
  Making one public is a deliberate choice: add it to `publicPaths` + the
  `setupGate` allowlist and justify it.
- **Escape user-influenced data in HTML.** Path-derived values concatenated into
  markup go through `html.EscapeString` (`html/template` already escapes for
  normal interpolation).
- **Registry/token hygiene.** Read tokens from a file via `--password-stdin`,
  never `-p <token>` on the command line.

## 6. Docs & hygiene

- Adding an env var or operator-facing behavior → update `.env.example`,
  `docs/INSTALL.md`, and `README.md` together.
- Mark roadmap items shipped in `docs/ROADMAP.md`.
- **No em dashes or semicolons in prose** (project style): use a period and a
  new sentence. Em dashes and semicolons are fine in code comments.
- **Surgical diffs**: every changed line should trace to the request. Don't
  reformat or "improve" adjacent code. Remove only the orphans your own change
  creates.

---

## Definition of Done

- [ ] `gofmt`, `go vet`, `go build`, `go test` all clean.
- [ ] New behavior has a test that asserts the observable result; new routes are
      in the auth enumeration or an explicit, tested public addition.
- [ ] High-care subsystem changes (section 4) have the restart/migration/render
      test they need.
- [ ] Docs updated (`.env.example` / `INSTALL` / `README` / `ROADMAP`).
- [ ] No secrets added; section 5 grep is clean.
- [ ] If deployed: rollback tag pinned, operator-logout warned, and the **specific
      artifact** verified live (not just a 200/303).
