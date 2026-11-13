#!/usr/bin/env sh
# Checks next-version.sh against throwaway repositories. Run from anywhere:
#   sh .github/scripts/next-version.test.sh
set -eu

script="$(cd "$(dirname "$0")" && pwd)/next-version.sh"
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
cd "$repo"
git init -q
git config user.email ci@example.com
git config user.name ci

commit() { git commit -q --allow-empty -m "$1"; }
expect() {
  want=$1; shift
  got=$(sh "$script" "$@")
  if [ "$got" != "$want" ]; then
    echo "FAIL: next-version.sh $* -> '$got', want '$want'" >&2
    exit 1
  fi
}

# No semver tag: the whole history counts, from 0.0.0.
commit "fix: a"
commit "feat: b"
commit "fix(ui): c"
expect 0.1.1
expect "" --previous

# From a tag: feat resets patch, "!" resets minor and patch, the rest bumps patch.
git tag 1.2.3
expect 1.2.3                  # HEAD is the tag itself
commit "feat(api): d"
expect 1.3.0
commit "docs: e"
expect 1.3.1
commit "refactor(core)!: f"
commit "fix: g"
expect 2.0.1
expect 1.2.3 --previous

echo "next-version.sh: all checks passed"
