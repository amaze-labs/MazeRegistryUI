# Configuration reference

MazeRegistryUI is configured by a single YAML file. There is no environment
variable for individual settings and no way to add a registry from the running
UI: the file is the complete, auditable list of what an operator has chosen to
expose.

The file is located, in order:

1. the `-config` flag,
2. the `MRUI_CONFIG` environment variable,
3. `/etc/mazeregistryui/config.yaml` (the container image sets `MRUI_CONFIG` to
   this path already).

Validate a file without starting the server:

```bash
mazeregistryui -config config.yaml -check
```

`-check` reports **every** problem it finds in one pass — a bad theme, three
registries missing a URL, a duplicate id — rather than one per run.

Unknown keys are a hard error. The decoder runs with `KnownFields(true)`, so a
typo like `registries[].delete_enable` fails at startup instead of silently
doing nothing.

---

## Command line and environment

| Flag | Default | Description |
| --- | --- | --- |
| `-config <path>` | `$MRUI_CONFIG`, else `/etc/mazeregistryui/config.yaml` | Path to the configuration file. |
| `-check` | `false` | Load and validate the configuration, print a summary, exit 0. |
| `-version` | `false` | Print version, commit and build date, then exit. |
| `-log-format <text\|json>` | `$MRUI_LOG_FORMAT`, else `text` | Log encoding. The *level* comes from `server.log_level`. |

| Variable | Effect |
| --- | --- |
| `MRUI_CONFIG` | Default for `-config`. |
| `MRUI_LOG_FORMAT` | Default for `-log-format`. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | Honoured for outbound registry calls (standard Go `http.ProxyFromEnvironment`). |
| *anything referenced by* `${VAR}` | Substituted into the config file — see [Secrets](#secrets). |

---

## `server`

HTTP listener settings.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `addr` | string | `:8080` | Listen address. Use `127.0.0.1:8080` to bind only the loopback interface when a reverse proxy on the same host is the only client. |
| `base_path` | string | *(empty)* | Serve the whole UI under a sub-path, e.g. `/registry`. Normalised to a leading slash and no trailing slash; `/` means the same as empty. See [Behind a reverse proxy on a sub-path](#behind-a-reverse-proxy-on-a-sub-path). |
| `read_timeout` | duration | `15s` | Maximum time to read a whole request. |
| `write_timeout` | duration | `60s` | Maximum time to write a response. Raise it if a registry is slow enough that image pages time out; the per-registry `timeout` should be well under this. |
| `shutdown_timeout` | duration | `10s` | Grace period for in-flight requests after SIGINT/SIGTERM. |
| `access_log` | bool | `false` | One structured log line per request (method, path, status, bytes, duration). Requests for `/static/` are excluded so they do not drown out everything else. |
| `log_level` | string | `info` | One of `debug`, `info`, `warn`, `error`. `debug` logs every outbound registry request with its status and elapsed time — the first thing to turn on when a registry misbehaves. |

Durations are Go duration strings (`15s`, `2m`, `1h30m`). A bare number is
**not** accepted and fails to parse.

Two timeouts are not configurable because they are safety limits rather than
tuning knobs: the header read timeout (10s) and the idle connection timeout
(90s).

---

## `ui`

Presentation settings. Nothing here affects what the server is allowed to do.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `title` | string | `MazeRegistryUI` | Name in the header and in `<title>`. Useful to distinguish staging from production at a glance. |
| `theme` | string | `auto` | Initial theme: `auto`, `dark` or `light`. `auto` follows the visitor's system preference. A visitor's own choice is stored in the `mrui_theme` cookie and always wins. |
| `catalog_page_size` | int | `50` | Repositories rendered per catalog page, and the page size used when walking the registry catalog. |
| `tag_page_size` | int | `50` | Tags requested per page on a repository view. |
| `default_registry` | string | first entry's `id` | The registry selected on first visit and by `/`. Must match a configured `id`. |
| `show_pull_command` | bool | `false` | Render a copyable `docker pull …` line on the image view. |
| `footer_note` | string | *(empty)* | Free text in the footer — a support address, a ticket queue, an ownership note. |

Values of `0` or less for the two page sizes fall back to the defaults. The
registry client independently clamps any page request to 1000 items.

---

## `registries[]`

At least one entry is required; the process refuses to start otherwise. Each
entry gets its own HTTP client, credentials, cache and concurrency budget, so a
slow or broken registry cannot affect the others.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `id` | string | slug of `name` | Stable identifier used in URLs (`/r/<id>/...`). Must be unique. Set it explicitly if you care about bookmarks surviving a rename. |
| `name` | string | host of `url` | Display name in the picker and breadcrumbs. |
| `url` | string | *(required)* | Registry root, scheme included, **without** the `/v2` suffix — e.g. `https://harbor.example.com`, not `https://harbor.example.com/v2`. Including it is rejected at startup. |
| `description` | string | *(empty)* | One line under the name in the registry picker. |
| `pull_host` | string | host of `url` | Hostname shown in `docker pull` commands, for registries the UI reaches at a different address from the one users pull from (in-cluster service name vs. public hostname). |
| `insecure` | bool | `false` | Skip TLS certificate verification for this registry. See the warning in [Self-signed internal registry](#self-signed-internal-registry). |
| `delete_enabled` | bool | `false` | Show the manifest delete action. The registry must *also* allow deletion — see [Deletion](#deletion). |
| `timeout` | duration | `20s` | Per-request timeout for calls to this registry, and the response-header timeout on its transport. |
| `cache_ttl` | duration | `60s` | How long tag-addressed data (tag lists, catalog listings, tag→manifest lookups) is reused. Digest-addressed content is immutable and always cached for one hour regardless of this value. |
| `auth` | object | `{type: none}` | Credentials — see below. |

### Why `id` matters

`id` is derived from `name` by lower-casing and replacing every run of
non-alphanumeric characters with `-`. `Harbor (EU)` becomes `harbor-eu`. That
derivation is stable, but it changes if the display name changes, which breaks
every bookmark. Setting `id` explicitly decouples the two.

### `registries[].auth`

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `type` | string | `none` | `none`, `basic` or `bearer`. |
| `username` | string | | Required when `type: basic`. |
| `password` | string | | Password for `type: basic`. Prefer `password_env`. |
| `password_env` | string | | Name of an environment variable holding the password. Takes precedence over `password`; the process refuses to start if the variable is unset. |
| `token` | string | | Static bearer token. Required (or `token_env`) when `type: bearer`. |
| `token_env` | string | | Name of an environment variable holding the token. Takes precedence over `token`; must be set. |

`type: none` still works against a registry that requires a token from an
anonymous token service — see [How authentication is chosen](#how-authentication-is-chosen).

---

## Secrets

Two mechanisms, both keeping credentials out of the file and out of git.

**`${VAR}` expansion** runs over every scalar of the parsed document, so it
works in any string field. Expanding after parsing rather than before means
references inside comments are left alone, and a password containing a colon,
a quote or a newline cannot corrupt the document it lands in:

```yaml
registries:
  - name: Harbor
    url: https://harbor.example.com
    auth:
      type: basic
      username: ${HARBOR_USERNAME}
      password: ${HARBOR_PASSWORD}
```

`${VAR:-fallback}` supplies a default when the variable is unset:

```yaml
ui:
  title: ${MRUI_TITLE:-MazeRegistryUI}
```

A `${VAR}` with no fallback and no value in the environment is a startup
error, listing every missing variable at once. This is deliberate:
authenticating with a silently empty password is worse than not starting.

One YAML detail: inside a flow mapping the `}` closes the mapping, so a
reference there has to be quoted. Block style, which the examples above use,
needs no quoting.

```yaml
auth: { type: basic, username: u, password: "${HARBOR_PASSWORD}" }   # quoted
```

**`password_env` / `token_env`** name the variable instead of interpolating it.
The effect is the same; the difference is that the config file then contains
nothing that looks like a credential, which is easier to review and to commit.

```yaml
auth:
  type: basic
  username: ci-reader
  password_env: PROD_REGISTRY_PASSWORD
```

If both a literal and its `*_env` counterpart are present, the environment
variable wins.

---

## Worked examples

Each of these is a complete file.

### A single anonymous registry

```yaml
server:
  addr: ":8080"
  access_log: true

ui:
  title: Registry
  show_pull_command: true

registries:
  - id: local
    name: Local registry
    url: http://registry:5000
    description: Distribution 3 on the internal network
    pull_host: registry.internal:5000
    auth:
      type: none
```

`pull_host` is set because the UI reaches the registry by its container name
while people pull from `registry.internal:5000`.

### Several registries with mixed authentication

```yaml
server:
  addr: ":8080"
  access_log: true
  log_level: info

ui:
  title: Amaze Labs registries
  theme: auto
  catalog_page_size: 50
  tag_page_size: 50
  default_registry: prod
  show_pull_command: true
  footer_note: Questions? #platform on Slack.

registries:
  - id: prod
    name: Production
    url: https://registry.example.com
    description: Release images, read-only
    auth:
      type: basic
      username: ci-reader
      password_env: PROD_REGISTRY_PASSWORD

  - id: staging
    name: Staging
    url: https://registry.staging.example.com
    description: Rebuilt on every merge to main
    delete_enabled: true
    cache_ttl: 15s
    auth:
      type: basic
      username: ${STAGING_USERNAME}
      password: ${STAGING_PASSWORD}

  - id: sandbox
    name: Sandbox
    url: http://sandbox-registry.internal:5000
    description: Anonymous, wiped nightly
    delete_enabled: true
    timeout: 5s
    auth:
      type: none
```

Staging gets a shorter `cache_ttl` because tags move on every merge and a
minute of staleness is confusing there; production keeps the default.

### Harbor

Harbor issues bearer tokens from its own token service. Configure basic
credentials — ideally a robot account — and the handshake is automatic.

```yaml
server:
  addr: ":8080"

ui:
  title: Harbor
  show_pull_command: true

registries:
  - id: harbor
    name: Harbor
    url: https://harbor.example.com
    description: Corporate registry
    auth:
      type: basic
      username: ${HARBOR_ROBOT_NAME}      # e.g. robot$library+mazeregistryui
      password_env: HARBOR_ROBOT_SECRET
```

The robot account needs *pull* (repository list and read) permission, plus
*delete* if you enable `delete_enabled`. Harbor's catalog endpoint only returns
projects the account can see, so a robot scoped to one project produces a
catalog containing only that project.

> Harbor robot names frequently contain `$`. Inside a Compose file `$` starts a
> variable reference and has to be written `$$`; in a plain `.env` file or a
> Kubernetes Secret it does not.

### GHCR with a personal access token

```yaml
server:
  addr: ":8080"

ui:
  title: GitHub Container Registry
  show_pull_command: true

registries:
  - id: ghcr
    name: GHCR
    url: https://ghcr.io
    description: github.com/amaze-labs packages
    pull_host: ghcr.io
    auth:
      type: bearer
      token_env: GHCR_TOKEN            # a classic PAT with read:packages
```

The token is presented directly *and* forwarded to GHCR's token service during
the handshake, so private packages the PAT can see remain visible.

GHCR does **not** implement `/v2/_catalog`. The catalog page will therefore be
empty or return an error; repository and image URLs still work, so GHCR is
useful here as a second entry you navigate into by URL rather than by browsing.
`type: basic` with your GitHub username and the PAT as the password works too.

### Self-signed internal registry

```yaml
server:
  addr: ":8080"

ui:
  title: Lab registry

registries:
  - id: lab
    name: Lab
    url: https://registry.lab.internal:5000
    description: Self-signed certificate
    insecure: true
    auth:
      type: basic
      username: lab
      password_env: LAB_REGISTRY_PASSWORD
```

> **Warning.** `insecure: true` disables certificate verification for this
> registry entirely: no chain validation, no hostname check, no expiry check.
> Anyone able to intercept the connection can present any certificate and will
> receive the credentials configured above. It is acceptable on a trusted
> internal segment while a proper certificate is pending; it is not acceptable
> across the internet. The better fix is to mount your internal CA into the
> container's trust store (see
> [docs/DEPLOYMENT.md](DEPLOYMENT.md#trusting-an-internal-ca)) and leave
> `insecure` at `false`.

### Behind a reverse proxy on a sub-path

To serve the UI at `https://tools.example.com/registry/`:

```yaml
server:
  addr: "127.0.0.1:8080"
  base_path: /registry
  access_log: true

ui:
  title: Registry

registries:
  - id: prod
    name: Production
    url: https://registry.example.com
    auth:
      type: basic
      username: ci-reader
      password_env: PROD_REGISTRY_PASSWORD
```

Every link, form action, static asset and HTMX endpoint is generated with the
prefix, and `/registry` redirects to `/registry/`, so the proxy must pass the
path through **unmodified** — do not strip the prefix. Note that `/healthz` is
also moved under the prefix (`/registry/healthz`) because the base path wraps
the entire handler. Proxy snippets are in
[docs/DEPLOYMENT.md](DEPLOYMENT.md#reverse-proxies).

### Everything from the environment

Useful when the config file itself is a ConfigMap and all credentials come from
a Secret:

```yaml
server:
  addr: ":8080"
  base_path: ${MRUI_BASE_PATH:-}
  access_log: true
  log_level: ${MRUI_LOG_LEVEL:-info}

ui:
  title: ${MRUI_TITLE:-MazeRegistryUI}
  theme: ${MRUI_THEME:-auto}
  show_pull_command: true

registries:
  - id: primary
    name: ${MRUI_REGISTRY_NAME:-Registry}
    url: ${MRUI_REGISTRY_URL}
    auth:
      type: basic
      username: ${MRUI_REGISTRY_USERNAME}
      password_env: MRUI_REGISTRY_PASSWORD
```

`MRUI_REGISTRY_URL` and `MRUI_REGISTRY_USERNAME` have no fallback, so the
process refuses to start if the Secret was not mounted.

---

## How authentication is chosen

Per registry, on every outbound request:

1. **Static credentials are attached first**, according to `auth.type`:
   `basic` sends an `Authorization: Basic` header built from `username` and
   `password`; `bearer` sends `Authorization: Bearer <token>`; `none` sends
   nothing.
2. **If the registry answers `401`**, its `WWW-Authenticate` header is parsed.
   - No `Bearer` challenge (a `Basic` challenge, or no header at all) → the
     `401` is final and surfaces as *"The registry rejected the configured
     credentials."*
   - A `Bearer` challenge → the token handshake below runs.
3. **The token handshake** takes `realm`, `service` and `scope` from the
   challenge and issues a `GET` to the realm URL with `service` and one `scope`
   parameter per scope. Credentials are presented to the token service too:
   basic auth when a username or password is configured, otherwise the static
   bearer token when one is (which is what makes GHCR and Gitea PATs work).
   Without either, the request is anonymous, which is exactly right for a
   public registry that still requires a token.
4. **The original request is retried exactly once** with the returned token. A
   second failure is reported as-is; retrying further would only add load to a
   registry that has already said no.

Tokens are cached per `(realm, service, scope)` for their advertised
`expires_in`, minus a ten-second safety margin, defaulting to 60 seconds when
the registry omits it. Token fetches are serialised: when a page of tags fans
out into dozens of parallel manifest requests that all hit the same `401`, one
token request is made, not dozens.

Two details worth knowing:

- `Authorization` is stripped when a redirect crosses to a different host.
  Registries routinely redirect blob downloads to object storage, and a
  registry credential does not belong in a request to S3.
- Credentials are never sent to the browser and never appear in logs or error
  pages; URLs are stripped of any userinfo before being logged.

---

## Deletion

Deleting a manifest requires **both** sides to agree:

1. `delete_enabled: true` on the registry entry here, and
2. deletion enabled on the registry itself — for Distribution, start it with
   `REGISTRY_STORAGE_DELETE_ENABLED=true` (or `storage.delete.enabled: true` in
   its config) and restart it.

The UI deletes by digest, so removing a manifest removes **every tag pointing
at it**, not just the one you were looking at. The form asks for confirmation
and carries a CSRF token; a POST that did not originate from the current page
is rejected.

Deletion frees no disk space by itself. Distribution only unlinks the manifest;
the blobs remain until garbage collection runs:

```bash
registry garbage-collect /etc/distribution/config.yml
```

Run it with the registry in read-only mode or stopped, per the Distribution
documentation, and expect space to be reclaimed only afterwards.

---

## Troubleshooting

### "The registry rejected the configured credentials" (401)

Reproduce the call the UI makes, from the same host:

```bash
curl -i -u "$USER:$PASS" https://registry.example.com/v2/_catalog
```

Then work through:

- **`auth.type: none` against a registry that needs credentials.** The message
  is the same either way; check the type is set.
- **A `*_env` variable that is set but empty.** The loader only rejects *unset*
  variables. An empty password is accepted and then refused by the registry.
- **A Harbor robot account name mangled by `$` expansion** in a Compose file.
  Write `$$` there, or use a `.env` file.
- **Insufficient scope rather than bad credentials.** Harbor and GHCR return
  `401` for "you may not list this" as well as for "who are you". Set
  `log_level: debug` to see the token request and the scope being asked for.
- **A GHCR PAT without `read:packages`**, or an expired one.

### The catalog is empty, but images are definitely there

- **The registry does not implement `/v2/_catalog`.** GHCR and Docker Hub do
  not. The repository and image views still work if you navigate to them
  directly; the catalog will not populate.
- **The credentials can read repositories but not enumerate them.** Harbor
  returns only projects the account has access to; a token scoped to a single
  repository yields an empty catalog.
- **A cached failure.** The catalog index is refreshed on `cache_ttl` and a
  stale list is preferred over an error page, so a registry that recovered may
  take up to `cache_ttl` to look right. Restarting the process clears it.
- **A very large registry.** The in-memory index stops at 50,000 repositories;
  beyond that the listing is truncated rather than growing without bound.

### `url … must not include the /v2 API path`

Configure `url: https://registry.example.com`, not
`https://registry.example.com/v2`. The client appends `/v2/...` itself.

`/v2` is the name the OCI Distribution Specification gives its API path — it
has nothing to do with the version of the registry or of this UI. A
Distribution 3.x server, and every OCI v1.1 registry, still serves its API
under `/v2/`. Seeing `/v2` in a URL is not a sign that something is out of date.

### Deleting returns 405, or "deletion is disabled on the registry itself"

`delete_enabled: true` here only makes the button appear. The registry returned
`405 Method Not Allowed`, which means deletion is off on its side. For
Distribution, set `REGISTRY_STORAGE_DELETE_ENABLED=true` and restart it. For
Harbor, delete permission belongs to the robot account or role. Afterwards,
remember that space is reclaimed only by garbage collection.

### Self-signed certificate errors

`x509: certificate signed by unknown authority` in the logs. Either mount your
internal CA bundle into the container so the certificate validates properly
(preferred, see
[docs/DEPLOYMENT.md](DEPLOYMENT.md#trusting-an-internal-ca)), or set
`insecure: true` on that registry, accepting that the connection is then
unauthenticated in both directions.

### A registry that ignores pagination

The client asks for `?n=<page size>` and follows the `Link: rel="next"` header
when the registry sends one. Registries that do not implement pagination
respond with everything at once, and that is handled: getting back more items
than were requested is taken as proof that `n` was ignored, and no next page is
requested. Getting back exactly a full page with no `Link` header is treated as
"there is probably more", using the last item as the cursor — a registry that
returns exactly `n` items and nothing further will cost one extra, empty
request per listing. If a registry paginates in a way the client cannot follow,
lower `catalog_page_size` and `tag_page_size` so a page fits comfortably inside
one response.

### Everything is slow

Turn on `log_level: debug` and `access_log: true`: each outbound registry call
is logged with its status and elapsed time, which separates "the registry is
slow" from "the UI is slow". Then consider raising `cache_ttl`, lowering
`tag_page_size` so fewer rows resolve per screen, or raising the registry
`timeout` if calls are being cut off. Tag rows resolve at most six at a time
per registry by design; that limit is not configurable.
