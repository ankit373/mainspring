#!/usr/bin/env bash
# Prove the published image can be pulled by someone holding no credentials.
#
# The push step's own success proves nothing, because the push is authenticated. A GHCR
# package is private by default even when its repository is public, so an image can
# publish green and still be unpullable by every user on earth — which is exactly what
# happened while the README advertised it (#228).
#
# Runnable locally:  .github/scripts/verify-image.sh 0.4.0
set -euo pipefail

VERSION="${1:?usage: verify-image.sh <version>   e.g. 0.4.0}"
IMAGE="${IMAGE:-ankit373/mainspring}"
WANT_PLATFORMS="${WANT_PLATFORMS:-linux/amd64 linux/arm64}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "==> anonymous pull check: ghcr.io/${IMAGE}:${VERSION}"

# Deliberately no credentials in this request: this has to fail the way a stranger's
# `docker pull` fails, not the way our own authenticated push succeeds.
if ! tokenresp=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE}:pull&service=ghcr.io"); then
  fail "GHCR refused an anonymous pull token for ${IMAGE}, so the package is private.
  Making the repository public does not make its packages public.
  Fix, in the web UI (there is no REST endpoint for container visibility):
    github.com/${IMAGE} -> Packages -> mainspring -> Package settings
      -> Danger Zone -> Change visibility -> Public"
fi

token=$(jq -r '.token // empty' <<<"$tokenresp")
[ -n "$token" ] || fail "GHCR returned no anonymous pull token for ${IMAGE}: ${tokenresp}"

if ! index=$(curl -fsS \
  -H "Authorization: Bearer ${token}" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://ghcr.io/v2/${IMAGE}/manifests/${VERSION}"); then
  fail "anonymous manifest fetch failed for ${IMAGE}:${VERSION} — the tag was not published"
fi

# A single-platform manifest has no .manifests array at all, so this also catches a
# multi-arch build that silently collapsed to one architecture.
for platform in $WANT_PLATFORMS; do
  os="${platform%%/*}"
  arch="${platform##*/}"
  if ! jq -e --arg os "$os" --arg arch "$arch" \
    '[.manifests[]? | select(.platform.os == $os and .platform.architecture == $arch)] | length > 0' \
    >/dev/null <<<"$index"; then
    fail "${IMAGE}:${VERSION} has no ${platform} manifest — the multi-arch build did not land"
  fi
  echo "    ${platform} present"
done

echo "==> OK: ghcr.io/${IMAGE}:${VERSION} is anonymously pullable for: ${WANT_PLATFORMS}"
