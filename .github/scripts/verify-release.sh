#!/usr/bin/env bash
# Verify a published release from the outside.
#
# This deliberately inspects the release users can see rather than the build log.
# A release build can go green and publish nothing, and for four consecutive
# releases the build never ran at all (#227) — a tag that shipped nothing looked
# exactly like a tag that shipped everything, because nothing ever asked.
#
# Runnable locally against any past release:  .github/scripts/verify-release.sh v0.4.0
set -euo pipefail

TAG="${1:?usage: verify-release.sh <tag>}"
REPO="${REPO:-ankit373/mainspring}"
VERSION="${TAG#v}"

# What a complete release carries. Adding a platform means adding it here too, and
# that is the point: a silently dropped platform has to fail rather than shrink the
# release quietly.
EXPECTED=(
  "mainspring_${VERSION}_darwin_amd64.tar.gz"
  "mainspring_${VERSION}_darwin_arm64.tar.gz"
  "mainspring_${VERSION}_linux_amd64.tar.gz"
  "mainspring_${VERSION}_linux_amd64.deb"
  "mainspring_${VERSION}_linux_amd64.rpm"
  "mainspring_${VERSION}_linux_amd64.apk"
  "mainspring_${VERSION}_linux_arm64.tar.gz"
  "mainspring_${VERSION}_linux_arm64.deb"
  "mainspring_${VERSION}_linux_arm64.rpm"
  "mainspring_${VERSION}_linux_arm64.apk"
  "mainspring_${VERSION}_windows_amd64.zip"
  "mainspring_${VERSION}_windows_arm64.zip"
)

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "==> verifying release $TAG in $REPO"

meta=$(gh release view "$TAG" --repo "$REPO" --json isDraft,assets 2>/dev/null) ||
  fail "no release exists for $TAG — the release build did not run (see #227)"

[ "$(jq -r .isDraft <<<"$meta")" = "false" ] ||
  fail "$TAG is still a draft, so nobody can download it"

names=$(jq -r '.assets[].name' <<<"$meta")
[ -n "$names" ] ||
  fail "$TAG has no assets — the release build did not run (see #227)"

grep -qxF -- 'checksums.txt' <<<"$names" ||
  fail "$TAG has no checksums.txt, so its artifacts cannot be verified at all"

missing=()
for want in "${EXPECTED[@]}"; do
  grep -qxF -- "$want" <<<"$names" || missing+=("$want")
done
[ ${#missing[@]} -eq 0 ] ||
  fail "$TAG is missing ${#missing[@]} expected artifact(s): ${missing[*]}"

# Presence is not integrity. Download everything and hash it against the release's
# own manifest, which catches a truncated or half-uploaded asset that still shows up
# in the asset list with a plausible size.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gh release download "$TAG" --repo "$REPO" --dir "$work" --clobber

if command -v sha256sum >/dev/null 2>&1; then
  check=(sha256sum -c checksums.txt)
else
  check=(shasum -a 256 -c checksums.txt) # macOS
fi
(cd "$work" && "${check[@]}") || fail "checksum mismatch in $TAG"

echo "==> OK: $TAG — $(wc -l <"$work/checksums.txt" | tr -d ' ') artifacts present and hash-correct"
