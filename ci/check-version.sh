#!/usr/bin/env bash
# Fail unless the release tag (e.g. v0.1.0) matches Version in internal/hue/version.go.
set -euo pipefail
tag="${1:?usage: check-version.sh <tag>}"
want="${tag#v}"
have="$(sed -n -E 's/^const Version = "([^"]+)"/\1/p' internal/hue/version.go | head -1)"
[ "$want" = "$have" ] || { echo "tag $tag != internal/hue/version.go version $have" >&2; exit 1; }
echo "version OK: $have"
