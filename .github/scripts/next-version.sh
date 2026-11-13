#!/usr/bin/env sh
# Computes the next semver version from the latest semver tag (N.N.N, no "v"
# prefix) reachable from HEAD, applying every later commit in chronological order:
#   - "type!:" or "type(scope)!:"  -> major (minor and patch reset to 0)
#   - "feat:" / "feat(scope):"      -> minor (patch reset to 0)
#   - anything else                 -> patch
# Merge commits are skipped. Prints the version (e.g. 0.92.3). If HEAD is already
# tagged it prints that version unchanged (the caller decides whether to skip).
# With no semver tag at all, it walks the whole history starting from 0.0.0.
set -eu

if tag=$(git describe --tags --abbrev=0 --match '[0-9]*.[0-9]*.[0-9]*' 2>/dev/null); then
  base="$tag"
  range="${tag}..HEAD"
else
  tag=""
  base="0.0.0"
  range="HEAD"
fi

# With --previous it prints the starting tag (empty if there is none) instead of
# the next version: the workflow uses it to know which release to diff against
# and to retag from when nothing that ends up in the images has changed.
if [ "${1:-}" = "--previous" ]; then
  echo "$tag"
  exit 0
fi

major=${base%%.*}; rest=${base#*.}
minor=${rest%%.*};  patch=${rest#*.}

if [ -n "$tag" ] && [ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ]; then
  echo "$base"
  exit 0
fi

# One subject per line, oldest first.
subjects="${TMPDIR:-/tmp}/subjects.$$"
git log --reverse --no-merges --format='%s' "$range" > "$subjects"

while IFS= read -r subject; do
  # type, optional scope, optional "!", then ":"
  if printf '%s' "$subject" | grep -Eq '^[A-Za-z]+(\([^)]*\))?!:'; then
    major=$((major + 1)); minor=0; patch=0
  elif printf '%s' "$subject" | grep -Eiq '^feat(\([^)]*\))?:'; then
    minor=$((minor + 1)); patch=0
  else
    patch=$((patch + 1))
  fi
done < "$subjects"
rm -f "$subjects"

echo "${major}.${minor}.${patch}"
