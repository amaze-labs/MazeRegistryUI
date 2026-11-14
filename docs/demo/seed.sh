#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Starts the two INVENTED registries the README screenshots are taken from
# ("Platform" and "Release") and fills them with real public images under
# made-up repository names. Nothing here touches a real registry of yours.
#
#   sh docs/demo/seed.sh            # start + seed (idempotent)
#   sh docs/demo/seed.sh down       # remove the registries and the network
#
# Then run the UI against them and take the screenshots at 1440x900, device
# scale factor 2, dark theme:
#
#   make build && bin/mazeregistryui -config docs/demo/config.yaml
#   open http://127.0.0.1:18080
#
# Needs Docker only: crane runs in a container on the demo network.
set -eu

NET=mrui-demo
CRANE=gcr.io/go-containerregistry/crane:debug

if [ "${1:-}" = "down" ]; then
  docker rm -f mrui-platform mrui-release >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  echo "demo removed"
  exit 0
fi

docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
for r in platform:5001 release:5002; do
  name=${r%%:*}
  port=${r##*:}
  docker rm -f "mrui-$name" >/dev/null 2>&1 || true
  docker run -d --name "mrui-$name" --network "$NET" --network-alias "$name" \
    -p "127.0.0.1:$port:5000" -e REGISTRY_STORAGE_DELETE_ENABLED=true \
    registry:3 >/dev/null
done

# The repository and tag names are the contract with the screenshots: rename
# or drop one and a screenshot has to be retaken. Older api-gateway releases
# come from older upstreams on purpose, so the tag list shows distinct digests,
# a single-platform index (1.7.3) and latest sharing a digest with 1.9.0.
docker run --rm -i --network "$NET" --entrypoint sh "$CRANE" -s <<'EOF'
set -eu
P=platform:5000
R=release:5000
amd() { crane copy --insecure --platform linux/amd64 "$1" "$2"; }
two() { crane index filter --insecure --platform linux/amd64 --platform linux/arm64 "$1" -t "$2"; }
one() { crane index filter --insecure --platform linux/amd64 "$1" -t "$2"; }

amd python:3.13-alpine  $P/data/etl-runner:2.4.1
amd python:3.12-alpine  $P/data/etl-runner:2.4.0
amd busybox:1.37        $P/data/warehouse-sync:0.9.2
amd nginx:1.29-alpine   $P/infra/nginx:1.29-alpine
amd postgres:16-alpine  $P/infra/postgres:16-alpine
amd redis:8-alpine      $P/infra/redis:8-alpine
one caddy:2.8-alpine    $P/platform/api-gateway:1.7.3
two caddy:2.9-alpine    $P/platform/api-gateway:1.8.0
two caddy:2.10-alpine   $P/platform/api-gateway:1.9.0
two caddy:2.10-alpine   $P/platform/api-gateway:latest
amd alpine:3.21         $P/platform/auth-service:0.14.1
amd node:22-alpine      $P/platform/web-frontend:3.2.0
amd alpine:3.22         $P/sandbox/alpine:3.22
amd golang:1.25-alpine  $P/tools/ci-builder:1.25

two nginx:1.29-alpine   $R/platform/api-gateway:1.9.0
two node:22-alpine      $R/platform/web-frontend:3.2.0
amd alpine:3.21         $R/platform/auth-service:0.14.1
EOF

echo "demo ready: Platform on 127.0.0.1:5001, Release on 127.0.0.1:5002"
