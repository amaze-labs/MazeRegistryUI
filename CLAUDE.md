# CLAUDE.md

Project context for Claude Code working on **MazeRegistryUI** — a single-binary, server-rendered web UI for browsing OCI / Docker Distribution registries.

The deep reference is `docs/ARCHITECTURE.md` (request path, caches, templates, security posture, package map — read it before changing anything structural). User documentation is in `docs/` (`CONFIGURATION.md`, `DEPLOYMENT.md`), the contribution rules for humans in `CONTRIBUTING.md`. This file holds the rules that apply everywhere and the decisions that must not be undone by accident.

## Stack

- **Go 1.25, standard library first.** The only third-party module is `gopkg.in/yaml.v3`. Routing is `net/http`'s `ServeMux` with Go 1.22+ patterns — no router, no framework, no ORM, no DI. A new dependency needs a good reason and a GPLv3-compatible licence (`THIRD-PARTY-NOTICES.md`).
- **Server-rendered HTML** with `html/template`, **HTMX** for the few interactive parts (lazy tag rows, catalog search, picker health dots). The page must be useful before any script runs: HTMX is progressive enhancement, never load-bearing.
- **No Node toolchain.** CSS, fonts (IBM Plex Mono), `htmx.min.js`, `theme.js`, `app.js` and icons are committed under `web/static/` and embedded with `embed.FS`. Nothing is fetched from a CDN — that is what makes the strict CSP and air-gapped installs possible. Don't add a bundler, a CSS framework or a webfont from a third-party origin.
- **Deployment**: one static binary (`CGO_ENABLED=0`) in `gcr.io/distroless/static-debian12:nonroot` (uid 65532). No shell in the image, so no `HEALTHCHECK` — probes come from outside on `/healthz`. One YAML config file, credentials via `${VAR}` / `*_env`.

## Repository layout

- `cmd/mazeregistryui/` — flags (`-config`, `-check`, `-version`, `-log-format`), logger, signal handling. Thin; keep it that way.
- `internal/config/` — schema, defaults (`applyDefaults`), `${VAR}` expansion on the raw text, `KnownFields(true)` decoding, `Validate` (collects every error with `errors.Join`).
- `internal/registry/` — the OCI Distribution v1.1 client: `types.go` (the `Client` interface, sentinel errors, display model), `client.go`, `manifest.go`, `auth.go` (`WWW-Authenticate` scanner + token cache), `cache.go` (bounded TTL/LRU). No dependency on `server`.
- `internal/server/` — routing (`server.go`), one handler per route plus its view model (`handlers.go`), catalog index (`catalog.go`), templates (`render.go`), security headers / CSRF / access log (`middleware.go`), humanisers (`humanize.go`).
- `internal/version/` — build metadata injected with `-ldflags -X`.
- `web/templates/` — `base.html` plus one file per page, each defining a `content` block; `partials/` for everything HTMX asks for directly. `web/static/` — the embedded assets.
- `deploy/config.yaml` — the example config for the compose stack. `compose.yaml` (local demo: registry + UI built from source), `compose.prod.yaml` (published image, external registries).
- `docs/` — `ARCHITECTURE.md`, `CONFIGURATION.md` (every config key — a new one goes there), `DEPLOYMENT.md`. `docs/images/` are the README screenshots; `docs/demo/` (`seed.sh` + `config.yaml`) is the INVENTED pair of registries they are taken from — see working rule 5.
- `.github/workflows/` — **`verify.yml`** publishes NOTHING: gofmt, `go vet`, `go test -race` with coverage, then an amd64 no-push image build and a smoke test (the image runs read-only with `deploy/config.yaml`, whose registry is absent, and `/healthz` must report the baked-in `VERSION`). It runs on pull requests and as the first job of the release (`workflow_call`), so a red suite blocks a release instead of racing it. **`release.yml`** runs on every push to `main`: `.github/scripts/next-version.sh` computes the version from the latest semver tag plus every commit subject after it, applied in order (`type!:` major, `feat:` minor, anything else patch — checked by `next-version.test.sh` on every run); the image is rebuilt only if its `PATHS` changed since the previous tag, otherwise the previous release is retagged; builds run natively per architecture (`ubuntu-latest` amd64, `ubuntu-24.04-arm` arm64, no QEMU), push by digest, then `docker buildx imagetools create` assembles the manifest list as `<version>` + `latest` — with retries, because GHCR intermittently 404s a digest it accepted seconds earlier (don't remove them). The annotated git tag (no `v`) is pushed LAST, only once the image is published — that ordering is what makes retagging `previous` safe — and the same job publishes a GitHub Release from the commit subjects; a failure there only warns, since the tag and the image ARE the release. Release caches use `release-<arch>` scopes, verify uses `verify`: never share a scope between a PR build and a publishing one. **`check_pr.yml`** requires Conventional Commits PR titles. `.github/dependabot.yml` groups minor/patch updates for actions, Go modules and the Dockerfile's base images. Every action is pinned by SHA with the version in a comment.

## Working on this repository

The same rules, for humans, are in `CONTRIBUTING.md`.

1. **Commit messages**: conventional commits, lower-case imperative subject, scope where it helps (`cmd`, `config`, `registry`, `server`, `ui`, `web`), with this co-author trailer:
       Co-authored-by: Claude <noreply@anthropic.com>
   The TYPE must describe THIS change, not echo the commits around it: `release.yml` turns every subject into a version bump (`feat:` = minor, `type!:` = major, anything else = patch), so a wrong type ships a wrong release number. `feat:` only for something a user can now do or see that they couldn't before; a tweak, a correction, a reworded label is `fix:`; `docs:` / `chore:` / `refactor:` / `test:` / `ci:` / `build:` for what changes no behaviour. A follow-up to a feature is not a feature because the feature was. Decide the type from the diff before writing the subject. Every push to `main` is a release.
2. **Tracked issues close themselves**: reference the issue from every commit that implements it, and make the LAST one `Closes #<n>` so the push closes it.
3. **Issue first, and nothing personal in it**: open the issue before writing the code that answers it (with `file:line` evidence), and reference related ones (`Refs #<n>`). The repo is public: issues, commits, PR bodies, tests, fixtures, config examples and screenshots carry nothing from a real installation — no real registry hostnames, domains, repository names, IPs, usernames or tokens. When an example helps, invent a neutral one (`registry.example.com`, `team/api`, `ci-reader`).
4. **Take what you work on**: when work on an issue or a pull request starts, assign it to the person you are working for (`gh issue edit <n> --add-assignee @me`, `gh pr edit <n> --add-assignee @me`) — unless it is already assigned to someone else: then ask before starting.
5. **Screenshots come from the demo, never from a real registry**: `docs/images/` (catalog, repository, image, picker) are taken from `docs/demo/` — `sh docs/demo/seed.sh` starts the two invented registries, `bin/mazeregistryui -config docs/demo/config.yaml` serves them on `127.0.0.1:18080` — at a 1440×900 viewport, device scale factor 2, dark theme. A change that shows on a screenshot retakes it in the same change. The repository and tag names in `seed.sh` are the contract with the screenshots: renaming or dropping one means retaking. The UI caches tag rows, so restart it after re-seeding.
6. **SPDX header on every new first-party source file**: `// SPDX-License-Identifier: GPL-3.0-or-later` on the first line, a blank line, then the package comment (so it doesn't become documentation). Vendored files keep their own headers and get a `THIRD-PARTY-NOTICES.md` entry.

## Local dev quick reference

- `make help` lists every target with the resolved `VERSION` / `COMMIT`.
- `make build` → `bin/mazeregistryui`; `make run` serves `deploy/config.yaml` on :8080 (needs the registry it names — `make compose-up` starts a throwaway Distribution 3 next to the UI, `make compose-down` removes it and its volume).
- The gate: `make lint` (go vet + gofmt check) and `make test`; `make test-race` and `make cover` as CI runs them. `make tidy` after touching `go.mod`.
- `bin/mazeregistryui -config <file> -check` validates a config and exits — use it after any config-schema change.
- Templates and static files are embedded: rebuild (or `go run`) to see a change, there is no live reload.
- No frontend test runner: check UI changes in a browser, light AND dark theme, and at phone width.

## Configuration single source of truth

- A config key exists in exactly three places: the struct in `internal/config`, its default in `applyDefaults` (only when the zero value is wrong) plus its check in `Validate`, and its row in `docs/CONFIGURATION.md`. The decoder runs with `KnownFields(true)`, so an undocumented key is a hard startup failure for whoever guesses its name — keep the doc in step in the same commit.
- `${VAR}` expansion happens on the raw text before parsing, so it works in any string field. An unset variable with no `${VAR:-fallback}` is a startup error — never soften that into an empty string (silently authenticating with an empty password is worse than refusing to start).
- Don't duplicate defaults in the compose files or the Dockerfile: compose only passes credentials through (with `:?` to fail fast in `compose.prod.yaml`).

## Security contract (immutable)

- **The browser never talks to a registry.** Every call is proxied; credentials live only in the server process and are never rendered. `sanitiseURL` strips userinfo before anything reaches a log line or an error.
- **Strict CSP** (`middleware.go`): `default-src 'none'`, `'self'` for script/font/connect, `style-src 'self'` plus a per-request nonce. So: no inline `<script>`, no `style="…"` attributes (a nonce can never authorise an attribute), no external origins. Computed styles (the layer bar widths) go in the one nonced `<style>` block via `Layout.Nonce`. Never add `'unsafe-inline'`.
- **Validate before interpolating**: repository names against the OCI grammar, tags against the tag grammar, digests against `^sha256:[0-9a-f]{64}$` — before they reach a URL. A new registry call goes on the `Client` interface with the same validation and a response-size ceiling on anything it reads.
- **Deletion is the only write path**, behind `delete_enabled`, the per-process CSRF token and `Sec-Fetch-Site: same-origin`. Don't add another state-changing route without the same protections.
- `Authorization` is stripped when a redirect crosses hosts (`stripAuthOnHostChange`); redirects are capped at 10. Keep both.
- There is no user authentication by design — an authenticating proxy goes in front (`docs/DEPLOYMENT.md`). Don't bolt one on.
- Legacy Docker manifest schema 1 is rejected on purpose (`ErrUnsupported`).

## Architecture invariants

- **One template set per page**, because every page defines `content`; a separate `_partials` set serves HTMX. Partials must be self-contained — one that reads `base`'s data breaks the moment HTMX requests it directly. Fragment routes live under `/x/` and render with `renderPartial`.
- **Every URL goes through `joinPath(basePath, …)` or the `url` / `asset` template funcs.** A hard-coded path breaks every `server.base_path` deployment.
- **Template funcs stay few** (`url`, `asset`, `bytes`, `since`, `full`, `short`, `dict`). Anything awkward in a template belongs in the handler, as a named, tested function building a view struct.
- **Caching**: digest-addressed content is immutable (`immutableTTL`, 1 h); tag- or name-addressed content uses `opts.CacheTTL`. Set the entry's repo scope so a deletion invalidates it. Cached values are shared — clone before sorting or filtering.
- **Anything that fans out** over tags or index children goes through a semaphore (`perRegistryConcurrency`, `maxChildConcurrency`). Unbounded parallelism against a registry is how you get rate-limited.
- **Tag ordering**: the registry's own order sets the pagination cursor BEFORE the natural display sort (`latest` first) rearranges the page — don't swap those steps.
- **Index sizes count distinct blobs once**; attestation children (`unknown/unknown`) are listed but excluded from platforms; a missing config blob is not fatal. Product decisions, not accidents.
- **A stale catalog beats an error page**: a failed refresh serves the previous list.

## When in doubt

- For a change that touches multiple areas, prefer ONE commit per logical concern. Easy to revert.
- If the user asks for a generic "audit", deliver a punch list grouped by severity (BLOCKER / HIGH / MEDIUM / LOW) with `file:line` references, not vague critiques.
- If a feature design implies a UX choice the user hasn't explicitly made, ASK first with a short clarifying question, then build it.
