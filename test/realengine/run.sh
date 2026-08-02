#!/usr/bin/env bash
# Run the real-engine suite: Mainspring, in front of an actual local engine,
# driven by the real OpenAI and Anthropic SDKs.
#
# Unlike the conformance suites this needs a live daemon and real weights, so it
# is not on the pull-request path. It is the only thing here that checks the
# assumptions the fakeserver encodes rather than Mainspring's side of them.
#
#   ./test/realengine/run.sh
#   REAL_MODEL=llama3.2:1b ./test/realengine/run.sh
#
# Everything it starts, it stops. Every reason it cannot run, it says out loud —
# a suite that skips quietly is worse than one that is absent, because the skip
# reads as a pass.
set -euo pipefail

REAL_MODEL="${REAL_MODEL:-qwen2.5:0.5b}"
ENGINE_URL="${ENGINE_URL:-http://127.0.0.1:11434}"
PORT="${PORT:-11501}"
# Overridable so a virtualenv can be used without activating it.
PYTHON="${PYTHON:-python3}"
MS_URL="http://127.0.0.1:${PORT}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
ms_pid=""

cleanup() {
  [ -n "$ms_pid" ] && kill "$ms_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

die() { echo "real-engine suite CANNOT RUN: $*" >&2; exit 2; }

# ── the engine has to be there, and it has to have the model ─────────────────
if ! curl -sf --max-time 5 "${ENGINE_URL}/api/tags" >/dev/null 2>&1; then
  die "no Ollama daemon at ${ENGINE_URL}. Start one with \`ollama serve\`.
     (Set ENGINE_URL to point somewhere else.)"
fi
if ! curl -sf --max-time 5 "${ENGINE_URL}/api/tags" | grep -q "\"${REAL_MODEL}\""; then
  die "model ${REAL_MODEL} is not pulled. Run \`ollama pull ${REAL_MODEL}\`.
     (Set REAL_MODEL to use one you already have — every assertion here is about
     mechanism, not output quality, so any small chat model will do.)"
fi

"$PYTHON" -c 'import anthropic, openai' 2>/dev/null || die "the SDKs are missing from ${PYTHON}. Run:
     ${PYTHON} -m pip install 'openai>=1.40,<2' anthropic
     (Or set PYTHON to an interpreter that has them.)"

# ── config: for an adopted daemon the model id IS the engine's model name ────
# There is no `path`; adopt backends never load anything from disk. Getting this
# wrong is what #197 was found through.
cat > "$work/config.yaml" <<YAML
addr: ":${PORT}"
backend: ollama
ollama_host: "${ENGINE_URL}"
usage_ledger: "off"
keep_alive_seconds: 0
models:
  - id: "${REAL_MODEL}"
YAML

echo "building mainspring…"
go build -o "$work/mainspring" "$root/cmd/mainspring"

echo "starting mainspring on :${PORT} in front of ${ENGINE_URL} (${REAL_MODEL})…"
"$work/mainspring" serve --config "$work/config.yaml" > "$work/mainspring.log" 2>&1 &
ms_pid=$!

for _ in $(seq 1 60); do
  curl -sf --max-time 2 "${MS_URL}/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
if ! curl -sf --max-time 2 "${MS_URL}/healthz" >/dev/null 2>&1; then
  echo "--- mainspring log ---" >&2
  cat "$work/mainspring.log" >&2
  die "mainspring did not come up"
fi

set +e
MAINSPRING_URL="$MS_URL" ENGINE_URL="$ENGINE_URL" REAL_MODEL="$REAL_MODEL" \
  "$PYTHON" "$root/test/realengine/realengine.py"
status=$?
set -e

if [ $status -ne 0 ]; then
  echo "--- mainspring log ---" >&2
  tail -40 "$work/mainspring.log" >&2
fi
exit $status
