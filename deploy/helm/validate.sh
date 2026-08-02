#!/usr/bin/env bash
# Render the Helm chart every way it can be installed, and check the result is
# both valid Kubernetes and a config this server can actually load.
#
#   ./deploy/helm/validate.sh
#
# `helm lint` alone is close to worthless here: it checks chart structure, not
# whether the manifests are valid Kubernetes and not whether the config.yaml the
# chart writes into its Secret is one Mainspring will parse. Every defect in #207
# survived `helm lint` cleanly.
#
# Like the real-engine suite, this refuses loudly rather than skipping — a silent
# skip reads as a pass.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
chart="$root/deploy/helm/mainspring"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

die() { echo "helm validation CANNOT RUN: $*" >&2; exit 2; }
fail() { echo "FAIL: $*" >&2; failed=$((failed + 1)); }
pass() { echo "PASS: $*"; }
failed=0

command -v helm >/dev/null || die "helm is not installed (brew install helm)"
command -v kubeconform >/dev/null || die "kubeconform is not installed.
     go install github.com/yannh/kubeconform/cmd/kubeconform@latest
     (It is what actually validates the manifests against Kubernetes schemas;
     helm lint does not.)"

# ── the chart must not drift from the released app version ───────────────────
# values.yaml resolves image.tag from .Chart.appVersion, so a stale appVersion
# means a default `helm install` silently pulls an old image. This is exactly
# how the chart ended up two releases behind in #207.
released="$(python3 -c 'import json;print(json.load(open("'"$root"'/.release-please-manifest.json"))["."])')"
appversion="$(python3 -c 'import yaml;print(yaml.safe_load(open("'"$chart"'/Chart.yaml"))["appVersion"])')"
chartversion="$(python3 -c 'import yaml;print(yaml.safe_load(open("'"$chart"'/Chart.yaml"))["version"])')"
if [ "$appversion" != "$released" ]; then
  fail "Chart.yaml appVersion is $appversion but the released version is $released.
      release-please bumps this via extra-files; if they disagree, a default
      install pulls ghcr.io/ankit373/mainspring:$appversion."
else
  pass "Chart appVersion tracks the release ($appversion)"
fi
if [ "$chartversion" != "$released" ]; then
  fail "Chart.yaml version is $chartversion but the released version is $released"
else
  pass "Chart version tracks the release ($chartversion)"
fi

# ── lint, then render every combination an operator can install ──────────────
helm lint "$chart" >/dev/null || fail "helm lint failed on default values"
helm lint "$chart" -f "$chart/values-gpu.yaml" >/dev/null || fail "helm lint failed on values-gpu.yaml"
pass "helm lint clean on both shipped values files"

# A config file for the --set-file path, which is how deploy/README.md documents
# injecting a real config and which used to render an unparseable Secret.
cat > "$work/set-file-config.yaml" <<'YAML'
addr: ":11500"
backend: ollama
ollama_host: "http://127.0.0.1:11434"
discover_models: true
models: []
YAML

render() { # name, then helm args
  local name="$1"; shift
  helm template ms "$chart" "$@" > "$work/$name.yaml" 2>"$work/$name.err" \
    || { fail "render '$name' failed: $(tail -3 "$work/$name.err")"; return 1; }
  pass "renders: $name"
}

render default
render gpu -f "$chart/values-gpu.yaml"
# HPA is disabled in both shipped values files, so nothing renders it unless we
# ask — which is why it went unexercised until #207.
render hpa --set autoscaling.enabled=true
render monitor --set serviceMonitor.enabled=true
render persistence --set models.persistence.enabled=true
render everything --set autoscaling.enabled=true --set serviceMonitor.enabled=true \
  --set models.persistence.enabled=true --set probes.enabled=true
render set-file --set-file config="$work/set-file-config.yaml"

# ── are those manifests actually valid Kubernetes? ───────────────────────────
# ServiceMonitor is a Prometheus-Operator CRD, so it has no upstream schema and
# is skipped; everything else must validate strictly.
for f in "$work"/*.yaml; do
  [ "$f" = "$work/set-file-config.yaml" ] && continue
  n="$(basename "$f" .yaml)"
  if kubeconform -strict -ignore-missing-schemas -kubernetes-version 1.31.0 "$f" >"$work/$n.kc" 2>&1; then
    pass "kubeconform: $n"
  else
    fail "kubeconform rejected $n: $(head -5 "$work/$n.kc")"
  fi
done

# ── the part helm can never tell you: does the server accept this config? ────
# The chart writes a config.yaml into a Secret. Since #197 that file is decoded
# with KnownFields(true), so a key the chart ships but the server dropped is a
# hard startup failure — a crash-loop that no amount of chart linting would catch.
echo "building mainspring…"
go build -o "$work/mainspring" "$root/cmd/mainspring" || die "could not build mainspring"

for f in "$work"/default.yaml "$work"/gpu.yaml "$work"/everything.yaml "$work"/set-file.yaml; do
  n="$(basename "$f" .yaml)"
  python3 - "$f" "$work/cfg-$n.yaml" <<'PY'
import sys, yaml
src, dst = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(src)):
    if d and d.get("kind") == "Secret":
        open(dst, "w").write(d["stringData"]["config.yaml"])
        break
else:
    sys.exit("no Secret in " + src)
PY
  if out="$("$work/mainspring" doctor --config "$work/cfg-$n.yaml" 2>&1)" \
     && grep -q "config loaded" <<<"$out"; then
    pass "mainspring loads the config the chart writes: $n"
  else
    fail "mainspring rejected the chart's config ($n):
      $(grep -E '✗|error|cannot' <<<"$out" | head -3)"
  fi
done

echo
if [ "$failed" -gt 0 ]; then
  echo "$failed check(s) failed"
  exit 1
fi
echo "helm chart validated: renders, schema-valid, and the config it ships loads"
