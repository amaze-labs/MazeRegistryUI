# Contributing

Thanks for taking the time. This is a small Go project with no code generation,
no Node toolchain and no external services, so the loop is short.

## Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.25 or newer | building and testing |
| `make` | any | the task targets |
| Docker (with Buildx) | recent | `make docker-build`, `make compose-up` |
| `git` | any | version metadata comes from `git describe` |

There are no runtime dependencies beyond the Go standard library and
`gopkg.in/yaml.v3`. Templates, CSS, fonts and HTMX are committed under `web/`
and embedded at build time — there is nothing to install or compile for the
frontend.

## Getting started

```bash
git clone https://github.com/amaze-labs/MazeRegistryUI.git
cd MazeRegistryUI
make help          # list every target, with the resolved VERSION/COMMIT
make build         # -> bin/mazeregistryui
make run           # runs against deploy/config.yaml on :8080
```

`make run` expects a registry at the URL in `deploy/config.yaml`. If you do not
have one, `make compose-up` starts a throwaway Distribution 3 registry next to
the UI (`make compose-down` removes it and its volume).

## The loop

```bash
make fmt           # gofmt -w .
make lint          # go vet ./... plus a gofmt check that fails on diffs
make test          # go test ./...
make test-race     # go test -race ./...
make cover         # coverage.out plus the total on stdout
```

`make lint` and `make test` are the gate. CI (`.github/workflows/ci.yml`) runs
the same three things — gofmt, `go vet`, `go test -race` — plus a multi-arch
image build, so anything that passes locally passes there.

Run `make tidy` if you touched `go.mod`.

### Conventions

- Everything is formatted with `gofmt`; no other formatter is configured.
- Exported identifiers carry doc comments. Comments in this codebase explain
  *why* a decision was made, not what the line does — please match that.
- No new third-party dependencies without a good reason. The single-binary,
  no-CDN property is the point of the project.

## Commit messages

Conventional Commits, lower-case subject, imperative mood, scope in
parentheses where it helps. The existing history:

```
feat(cmd): add the mazeregistryui binary
feat(server): add the HTTP layer
feat(ui): add the interface, its design system and client-side behaviour
feat(registry): add an OCI Distribution v1.1 client
build: add Docker image, compose stacks and GitHub Actions pipelines
feat(config): add file-based multi-registry configuration
chore: initialize Go module and version metadata
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `build`, `chore`.
Scopes in use: `cmd`, `server`, `registry`, `config`, `ui`.

## Pull requests

1. Branch off `main`.
2. Make `make lint` and `make test` pass.
3. Add tests for behaviour you change — the registry client and the config
   loader are pure enough to test directly.
4. Update `docs/CONFIGURATION.md` when you add or rename a configuration field.
   The loader runs with `KnownFields(true)`, so an undocumented key is a hard
   startup failure for whoever guesses it wrong.
5. Describe what changed and why in the PR body.

## Releases

Maintainers push a tag; `.github/workflows/release.yml` does the rest. See
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md#releases-and-tagging) for what each
kind of tag produces.

## Licensing

The project is under the [GNU General Public License v3.0 or later](LICENSE).
By opening a pull request you agree that your contribution is licensed under
those same terms — there is no CLA and no copyright assignment.

New source files carry the identifier at the top, above the package comment
and separated from it by a blank line so it does not become documentation:

```go
// SPDX-License-Identifier: GPL-3.0-or-later

// Package foo does ...
package foo
```

Vendored third-party files keep their own headers and are listed in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md). Add an entry there before
bundling anything new, and check the licence is GPLv3-compatible first.
