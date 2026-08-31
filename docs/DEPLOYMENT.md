# Deployment

MazeRegistryUI is one static binary with one configuration file. It holds no
state, writes nothing to disk and needs no database, so it scales horizontally
by running more copies and is safe to restart at any time. Everything below is
about getting the config file to the process and the process onto the network.

Contents:

- [Container image](#container-image)
- [Docker](#docker)
- [Docker Compose](#docker-compose)
- [Kubernetes](#kubernetes)
- [Reverse proxies](#reverse-proxies)
- [Trusting an internal CA](#trusting-an-internal-ca)
- [Building from source](#building-from-source)
- [Releases and tagging](#releases-and-tagging)

---

## Container image

```
ghcr.io/amaze-labs/mazeregistryui:latest
```

| Property | Value |
| --- | --- |
| Base | `gcr.io/distroless/static-debian12:nonroot` |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | `65532:65532` (`nonroot`), declared numerically |
| Entrypoint | `/mazeregistryui` |
| Port | `8080` |
| Config path | `/etc/mazeregistryui/config.yaml` (`MRUI_CONFIG` is preset) |
| Writable paths | none needed; mount a tmpfs at `/tmp` if you want one |
| Health endpoint | `GET /healthz` → `{"status":"ok","version":…,"registries":N}` |

The image contains a CA bundle (for outbound HTTPS to registries), `/etc/passwd`
entries for the nonroot user, tzdata and the binary. There is no shell, no
package manager and no `curl` — which matters for health checks, see below.

Pin a version in production:

```bash
docker pull ghcr.io/amaze-labs/mazeregistryui:1.2.3
```

---

## Docker

```bash
docker run -d \
  --name mazeregistryui \
  --restart unless-stopped \
  -p 8080:8080 \
  -v /etc/mazeregistryui/config.yaml:/etc/mazeregistryui/config.yaml:ro \
  -e MRUI_LOG_FORMAT=json \
  -e PROD_REGISTRY_PASSWORD \
  --read-only \
  --tmpfs /tmp:size=16m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --user 65532:65532 \
  --memory 256m --cpus 1 \
  ghcr.io/amaze-labs/mazeregistryui:latest
```

Notes on the flags that matter:

- **`-v …:ro`** — the config is the only thing the container needs from the
  host. Mount the file, not its directory, so nothing else is exposed.
- **`--read-only`** — the process never writes to disk. The `--tmpfs /tmp` is
  belt and braces; the binary does not use it today.
- **`--cap-drop ALL` / `--security-opt no-new-privileges:true`** — it binds
  8080, an unprivileged port, and needs nothing else.
- **`--user 65532:65532`** — already the image default; state it explicitly if
  your policy requires it.
- **`-e PROD_REGISTRY_PASSWORD`** (no value) passes the variable through from
  the host environment, keeping the secret out of `docker inspect` history.
  For anything more than a demo, prefer `--env-file` with a mode-0600 file.

Overriding the config location, or passing flags, goes after the image name —
the entrypoint is the binary itself:

```bash
docker run --rm \
  -v "$PWD/config.yaml:/cfg.yaml:ro" \
  ghcr.io/amaze-labs/mazeregistryui:latest -config /cfg.yaml -check
```

That is the quickest way to validate a config before rolling it out.

### The distroless health check constraint

The image has no shell and no HTTP client, so a Docker `HEALTHCHECK` — which
always executes *inside* the container — would resolve to a missing binary and
mark the container permanently unhealthy. The Dockerfile therefore declares no
`HEALTHCHECK` on purpose, and explains why in a comment.

Probe from outside instead:

- **Kubernetes:** `httpGet` probes need nothing in the image. See below.
- **Compose:** a small sidecar with `curl` on the same network, as in
  `compose.yaml`.
- **A load balancer or reverse proxy:** point its own health check at
  `/healthz` (or `<base_path>/healthz` when `base_path` is set).
- **Plain Docker:** `docker run --rm --network container:mazeregistryui
  curlimages/curl -fsS http://127.0.0.1:8080/healthz` from a cron job.

---

## Docker Compose

Two stacks are committed:

| File | Purpose |
| --- | --- |
| `compose.yaml` | Local demo. Builds the image from source and starts a throwaway Distribution 3 registry with deletion enabled, plus a `ui-probe` sidecar that health-checks the UI from outside. |
| `compose.prod.yaml` | Pulls the published image and points at registries that live elsewhere. No local registry. |

Local:

```bash
docker compose up -d          # or: make compose-up
docker compose logs -f mazeregistryui
docker compose down -v        # or: make compose-down
```

Production-ish:

```bash
docker compose -f compose.prod.yaml up -d
```

Both read the UI's configuration from `./deploy/config.yaml`, mounted read-only
at `/etc/mazeregistryui/config.yaml`.

### Credentials

Neither file contains a secret. Put them in a git-ignored `.env` next to the
compose file; Compose loads it automatically and substitutes into the
`environment:` block, and `deploy/config.yaml` resolves the same variable names
through `${VAR}` expansion or `*_env`. The values therefore exist only in
`.env` and in the process environment — never in an image layer, never in git.

```dotenv
# .env
MRUI_HARBOR_USERNAME=robot$$mazeregistryui
MRUI_HARBOR_PASSWORD=…
MRUI_GHCR_USERNAME=amaze-labs
MRUI_GHCR_PASSWORD=ghp_…
```

`compose.prod.yaml` uses the `${VAR:?message}` form, so Compose refuses to
start rather than substituting an empty password.

> A literal `$` in a Compose file must be written `$$`. Harbor robot account
> names usually contain one.

### The `ui-probe` sidecar

`compose.yaml` runs `curlimages/curl` idling forever, with a healthcheck that
curls `http://mazeregistryui:8080/healthz`. Its health mirrors the UI's, so
other services can gate on it:

```yaml
depends_on:
  ui-probe:
    condition: service_healthy
```

This exists solely because the UI image has no shell to run a check in. Behind
Traefik (see the commented labels in `compose.prod.yaml`) the router's own
health check does the same job and the sidecar is unnecessary.

---

## Kubernetes

A complete example: ConfigMap for the configuration, Secret for the
credentials, Deployment, Service, Ingress.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: registry-ui
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: mazeregistryui
  namespace: registry-ui
data:
  config.yaml: |
    server:
      addr: ":8080"
      access_log: true
      log_level: info

    ui:
      title: Registries
      theme: auto
      show_pull_command: true
      default_registry: prod

    registries:
      - id: prod
        name: Production
        url: https://registry.example.com
        description: Release images
        auth:
          type: basic
          username: ci-reader
          password_env: PROD_REGISTRY_PASSWORD

      - id: harbor
        name: Harbor
        url: https://harbor.example.com
        delete_enabled: true
        auth:
          type: basic
          username: ${HARBOR_USERNAME}
          password_env: HARBOR_PASSWORD
---
apiVersion: v1
kind: Secret
metadata:
  name: mazeregistryui-registries
  namespace: registry-ui
type: Opaque
stringData:
  PROD_REGISTRY_PASSWORD: "replace-me"
  HARBOR_USERNAME: "robot$mazeregistryui"
  HARBOR_PASSWORD: "replace-me"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mazeregistryui
  namespace: registry-ui
  labels:
    app.kubernetes.io/name: mazeregistryui
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: mazeregistryui
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: mazeregistryui
      annotations:
        # Roll the pods when the configuration changes; a ConfigMap update is
        # not picked up by a running process, which reads the file once at
        # startup.
        checksum/config: "replace-with-a-hash-of-the-configmap"
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: mazeregistryui
          image: ghcr.io/amaze-labs/mazeregistryui:1.2.3
          imagePullPolicy: IfNotPresent
          args: ["-config", "/etc/mazeregistryui/config.yaml", "-log-format", "json"]
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          envFrom:
            - secretRef:
                name: mazeregistryui-registries
          volumeMounts:
            - name: config
              mountPath: /etc/mazeregistryui
              readOnly: true
            - name: tmp
              mountPath: /tmp
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 2
            periodSeconds: 10
            timeoutSeconds: 2
            failureThreshold: 3
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 10
            periodSeconds: 30
            timeoutSeconds: 3
            failureThreshold: 3
          resources:
            requests:
              cpu: 25m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            privileged: false
            capabilities:
              drop: ["ALL"]
      volumes:
        - name: config
          configMap:
            name: mazeregistryui
        - name: tmp
          emptyDir:
            medium: Memory
            sizeLimit: 16Mi
---
apiVersion: v1
kind: Service
metadata:
  name: mazeregistryui
  namespace: registry-ui
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: mazeregistryui
  ports:
    - name: http
      port: 80
      targetPort: http
      protocol: TCP
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: mazeregistryui
  namespace: registry-ui
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
spec:
  ingressClassName: nginx
  tls:
    - hosts: ["registry-ui.example.com"]
      secretName: mazeregistryui-tls
  rules:
    - host: registry-ui.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: mazeregistryui
                port:
                  name: http
```

Points worth understanding rather than copying blindly:

- **`httpGet /healthz` works because the probe runs from the kubelet**, not
  inside the container — the distroless image's lack of a shell is irrelevant
  here. `/healthz` answers as soon as the listener is up and does not contact
  any registry, so an unreachable registry will not restart your pods. That is
  intentional: a registry being down is a page-level error, not a reason to
  cycle the UI.
- **`readOnlyRootFilesystem: true`** is safe; the `/tmp` `emptyDir` is only
  there so nothing can be surprised by a missing writable directory.
- **`runAsUser: 65532`** matches the image's `nonroot` user. It is declared
  numerically in the Dockerfile precisely so `runAsNonRoot` admission can
  verify it without resolving `/etc/passwd`.
- **Secrets arrive as environment variables** (`envFrom`), which is what
  `${VAR}` expansion and `password_env` consume. Do not put them in the
  ConfigMap.
- **`checksum/config`** — the process reads the config once at startup, so a
  ConfigMap edit has no effect until the pods restart. Set this annotation to a
  hash of the ConfigMap (Helm: `sha256sum` of the rendered template; Kustomize:
  use a `configMapGenerator`, which changes the name and rolls automatically).
- **Two replicas** are fine. The caches are per-process, so each replica warms
  its own; there is no shared state and no session affinity requirement.
- **`base_path` is not needed** for a host-based Ingress like this one. It is
  needed when serving under a path — see below.

### Serving under a sub-path in Kubernetes

Set `server.base_path: /registry` in the ConfigMap and route the prefix
*without* rewriting it:

```yaml
spec:
  rules:
    - host: tools.example.com
      http:
        paths:
          - path: /registry
            pathType: Prefix
            backend:
              service:
                name: mazeregistryui
                port:
                  name: http
```

Do not add `nginx.ingress.kubernetes.io/rewrite-target: /`. The application
already expects the prefix and generates every link with it. Remember that the
probes then need `path: /registry/healthz`.

---

## Reverse proxies

The UI serves plain HTTP; terminate TLS at the proxy. It does not read
`X-Forwarded-*` headers to build URLs — links are built from `base_path` alone
— so the only hard requirements are that the request path arrives unmodified
and that the proxy does not buffer away streaming responses.

### nginx

Root of a host:

```nginx
server {
    listen 443 ssl http2;
    server_name registry-ui.example.com;

    ssl_certificate     /etc/letsencrypt/live/registry-ui.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/registry-ui.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Image pages resolve every child manifest of an index; give a slow
        # registry room before the proxy gives up.
        proxy_read_timeout 120s;
    }

    location = /healthz {
        proxy_pass http://127.0.0.1:8080/healthz;
        access_log off;
    }
}
```

On a sub-path, with `server.base_path: /registry`:

```nginx
location /registry/ {
    # No trailing path on proxy_pass: the prefix must reach the app intact.
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 120s;
}
```

`proxy_pass http://127.0.0.1:8080/;` — note the trailing slash — would strip
`/registry` and break every link. Leave it off.

### Caddy

```caddyfile
registry-ui.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}
```

Sub-path, with `server.base_path: /registry`:

```caddyfile
tools.example.com {
    encode zstd gzip

    # handle, not handle_path: handle_path strips the prefix, which the
    # application needs to keep.
    handle /registry/* {
        reverse_proxy 127.0.0.1:8080
    }
}
```

### Traefik

Labels on the container (the commented block in `compose.prod.yaml` is the same
thing, ready to uncomment):

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.docker.network=mazenet"
  - "traefik.http.routers.mazeregistryui.rule=Host(`registry-ui.example.com`)"
  - "traefik.http.routers.mazeregistryui.entrypoints=websecure"
  - "traefik.http.routers.mazeregistryui.tls=true"
  - "traefik.http.routers.mazeregistryui.tls.certresolver=letsencrypt"
  - "traefik.http.services.mazeregistryui.loadbalancer.server.port=8080"
  - "traefik.http.services.mazeregistryui.loadbalancer.healthcheck.path=/healthz"
  - "traefik.http.services.mazeregistryui.loadbalancer.healthcheck.interval=30s"
```

Sub-path, with `server.base_path: /registry`:

```yaml
labels:
  - "traefik.http.routers.mazeregistryui.rule=Host(`tools.example.com`) && PathPrefix(`/registry`)"
  # Do NOT add a stripprefix middleware here.
  - "traefik.http.services.mazeregistryui.loadbalancer.healthcheck.path=/registry/healthz"
```

When Traefik terminates TLS, drop the `ports:` mapping from the compose service
so the UI is only reachable through the proxy.

### Authentication in front of the UI

The UI has no user accounts: anyone who can reach it can browse every
configured registry, and delete from the ones with `delete_enabled: true`. If
that is not acceptable, put an authenticating proxy in front — nginx
`auth_request`, Caddy `basic_auth`, Traefik's `forwardauth`, oauth2-proxy — and
restrict network access to the UI to the proxy alone (`server.addr:
127.0.0.1:8080`, or a NetworkPolicy).

---

## Trusting an internal CA

Preferred over `insecure: true`, which turns off verification altogether. The
image reads the standard bundle at `/etc/ssl/certs/ca-certificates.crt`, so
mount a bundle that contains both the public roots and your CA over it.

Build the combined file once:

```bash
docker run --rm --entrypoint /bin/sh alpine:3 -c \
  'apk add --no-cache ca-certificates >/dev/null && cat /etc/ssl/certs/ca-certificates.crt' \
  > ca-bundle.crt
cat internal-ca.crt >> ca-bundle.crt
```

Docker:

```bash
docker run -d \
  -v "$PWD/ca-bundle.crt:/etc/ssl/certs/ca-certificates.crt:ro" \
  … ghcr.io/amaze-labs/mazeregistryui:latest
```

Kubernetes — put the bundle in a ConfigMap and mount the single file:

```yaml
volumeMounts:
  - name: ca
    mountPath: /etc/ssl/certs/ca-certificates.crt
    subPath: ca-bundle.crt
    readOnly: true
volumes:
  - name: ca
    configMap:
      name: internal-ca
```

Go also honours `SSL_CERT_FILE`, so `-e SSL_CERT_FILE=/certs/ca-bundle.crt`
with the bundle mounted anywhere is an equally good option.

---

## Building from source

```bash
git clone https://github.com/amaze-labs/MazeRegistryUI.git
cd MazeRegistryUI
make build                       # -> bin/mazeregistryui
./bin/mazeregistryui -config config.yaml
```

`make build` compiles with `CGO_ENABLED=0 -trimpath` and stamps the version,
commit and build date via `-ldflags -X`. `VERSION` defaults to `git describe
--tags --always --dirty`; override any of them:

```bash
make build VERSION=1.2.3 COMMIT=$(git rev-parse --short HEAD)
```

The result is a static binary with no runtime dependencies. Cross-compile the
same way the release workflow does:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags="-s -w -X github.com/amaze-labs/MazeRegistryUI/internal/version.Version=1.2.3" \
  -o mazeregistryui ./cmd/mazeregistryui
```

Container image:

```bash
make docker-build                # ghcr.io/amaze-labs/mazeregistryui:$(VERSION) and :latest
make docker-run                  # build, then run hardened on :8080
```

Multi-arch, the way CI does it:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=1.2.3 \
  -t ghcr.io/amaze-labs/mazeregistryui:1.2.3 .
```

### Running it as a systemd service

```ini
[Unit]
Description=MazeRegistryUI
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/mazeregistryui -config /etc/mazeregistryui/config.yaml -log-format json
EnvironmentFile=/etc/mazeregistryui/secrets.env
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

Keep `secrets.env` mode 0600. The process handles SIGTERM by draining
in-flight requests within `server.shutdown_timeout`, so `systemctl restart` is
graceful.

---

## Releases and tagging

Publishing is driven entirely by pushing a git tag; there is no manual step.

```bash
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

What `.github/workflows/release.yml` then does:

| Tag pushed | GHCR image tags | Binaries + GitHub release |
| --- | --- | --- |
| `v1.2.3` | `1.2.3`, `1.2`, `1`, `latest`, `v1.2.3` | yes |
| `v0.4.0` | `0.4.0`, `0.4`, `latest`, `v0.4.0` | yes |
| `rc-2026-02-01`, `customer-x` | the tag name only | no |

- Every tag produces a multi-arch (`linux/amd64`, `linux/arm64`) image on
  `ghcr.io/amaze-labs/mazeregistryui`.
- The floating `latest`, `X.Y` and `X` tags move only when the tag parses as
  semver, so an internal or throwaway tag can never make `latest` point at
  something unexpected. The bare major tag is skipped for `v0.x` releases,
  where a major version conveys nothing useful.
- Tags starting with `v` additionally build binaries for `linux/amd64`,
  `linux/arm64`, `darwin/amd64` and `darwin/arm64`, tar them up with a
  `checksums.txt`, and attach them to a GitHub release with generated notes.
- `VERSION`, `COMMIT` and `BUILD_DATE` are stamped into the binary, so
  `mazeregistryui -version` and the UI footer identify exactly what is running.

Pushes to `main` and pull requests run `.github/workflows/ci.yml` instead:
gofmt, `go vet`, `go test -race` with coverage, and a multi-arch image build
that is never pushed.

### Upgrading

The configuration format is the contract; check
[docs/CONFIGURATION.md](CONFIGURATION.md) and the release notes for new or
renamed fields, then:

```bash
mazeregistryui -config /etc/mazeregistryui/config.yaml -check
```

Because unknown keys are rejected at startup, a removed field fails loudly
rather than being ignored. There is no state and no migration; rolling back is
redeploying the previous image tag.
