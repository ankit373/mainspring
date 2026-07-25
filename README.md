# Mainspring

**A backend-agnostic local inference server. One stable OpenAI/Anthropic-compatible API in
front; pluggable engines (llama.cpp, MLX, Ollama, …) behind it — install and run only what a
model needs.** Ships as a single Go binary and a container image, so the same server runs on
your laptop, a shared GPU box, or a GPU node in Kubernetes.

> The mainspring is the one tightly-wound component that drives the entire movement. This is the
> component that drives your local AI: small, self-contained, and the thing everything else runs on.

---

## Why

Local inference *engines* are commoditized and excellent — llama.cpp and Apple MLX are MIT-licensed
and win on tokens/sec. What's missing is the **control plane around them**. The most-cited problems
with Ollama and friends are not kernel problems, they are control-plane problems:

- **Silent context truncation** — prompts get dropped with no error, and the same prompt truncates
  differently per machine ("great locally, collapses in prod").
- **Silent CPU fallback** — you think you're on the GPU; you aren't, and nothing tells you.
- **Zero authentication** — tens of thousands of instances sit exposed on the public internet.
- **Model thrashing / VRAM mismanagement** — evict-and-reload every call; VRAM not reclaimed on switch.
- **OpenAI-compat gaps** — tool-call `arguments` type flips, streaming-with-tools breaks, `logprobs`/`n` ignored.

Mainspring's thesis: **don't build inference kernels — build the local-inference control plane.**
Wrap the permissively-licensed engines behind one **versioned, conformance-tested** API and make the
layer around them **fail loud, VRAM-aware, and governed**.

## What it does

- **One stable, versioned OpenAI *and* Anthropic API** — `/v1/chat/completions`, `/completions`,
  `/embeddings`, `/models`, and Anthropic `/v1/messages` (text **and** tool use, streaming translated) —
  validated against the real OpenAI Python/JS SDKs in CI.
- **Pluggable backends** behind a `Backend`/`Runner` interface: `llamacpp` and `mlx` (managed
  subprocess) · `ollama` / `lmstudio` / `llamafile` / `gpt4all` (detect-and-adopt-only, never installed).
  **Per-model routing, aliases, and ordered fallback chains** — one server can serve several models on
  different engines and fail over when one is down.
- **Fail loud, never silently degrade** — `/capabilities` and `X-Mainspring-{Backend,Device,Warning}`
  headers state the *actual* backend, whether it fell back to CPU, and the *effective* context window.
- **VRAM-residency-aware scheduling** — single-flight load, byte-budget + LRU eviction, KeepAlive idle
  unload, optional preload.
- **Reliability** — per-model concurrency limit + bounded queue (503 backpressure), a **circuit breaker**
  with a background health probe, and a structured error taxonomy (stable `code` per failure class).
- **Standalone governance** — API-key tenants with roles (admin/inference), per-tenant rate + token
  budgets, and **real** streaming token accounting.
- **Operability** — Prometheus `/metrics`, JSONL usage ledger, `X-Request-ID` + W3C `traceparent`
  propagation, optional access log, **SIGHUP hot-reload**, an **admin API** (drain / reload / model
  load-unload), **TLS**, graceful drain, and a `/v1/quality` routing signal a router (Hydra) can consume.
- **Detect-first, opt-in managed install** — use whatever engine is present; install a missing one only
  when you enable it (SHA-256-pinned, optionally ed25519-signed manifest) — never silently.
- **Deploy anywhere** — a single static binary, a distroless container image, and a Helm chart
  (HPA / ServiceMonitor / GPU node scheduling).

## Non-goals

- **We do not build inference kernels.** The compute cores (llama.cpp/GGML, MLX) already win; we wrap them.
- No GUI dependency — headless-first daemon.
- We do not vendor Ollama's tree; we reimplement the outer control-plane layer cleanly (its pattern is MIT).

## Status

**v0.1.0 — released.** The full control plane is shipped: OpenAI + Anthropic APIs, six backends with
per-model routing / aliases / fallback, VRAM-aware scheduling, governance, circuit breaker, TLS, admin
API, request-ID + trace-context, and `/v1/quality`. `go test -race` clean across the tree. See
[CHANGELOG.md](CHANGELOG.md) and the [releases page](https://github.com/ankit373/mainspring/releases).

## Quick start

```bash
# Build from source…
go build -o mainspring ./cmd/mainspring
# …or grab a release binary / the container image
#   https://github.com/ankit373/mainspring/releases
#   ghcr.io/ankit373/mainspring:latest

# See which engines are present on this host (detect-first)
./mainspring backends

# Serve a local GGUF via llama.cpp…
./mainspring serve --model qwen2.5-coder=/path/to/model.gguf --addr :11500
# …or adopt a model from a running Ollama daemon
./mainspring serve --backend ollama --model 'qwen2.5-coder:7b=' --addr :11500

# Talk to it with any OpenAI client
curl localhost:11500/v1/chat/completions -H 'content-type: application/json' -d '{
  "model": "qwen2.5-coder",
  "messages": [{"role":"user","content":"hello"}]
}'

# The fail-loud truth about what actually ran it
curl localhost:11500/capabilities
# The machine-readable routing signal (per-model resident/device/degraded/queue/breaker)
curl localhost:11500/v1/quality
```

## Relationship to Hydra

Mainspring is an **inference head**; [Hydra](https://github.com/ankit373/hydra) is the **router/trust
control plane above it**. They meet over the standard OpenAI-compatible HTTP boundary, so either can
evolve independently. Mainspring governs *its own* served endpoint (admission control); Hydra governs
*routing across many heads*.

**Hydra stays provider-neutral — it works with everything** (Ollama, OpenAI, Anthropic/Claude, any
OpenAI-compatible endpoint). Mainspring is not a replacement for that and Hydra never depends on it;
because Mainspring speaks the same standard API, Hydra treats it like any other head. What makes
Mainspring special is that it's the head Hydra works with *best* — it's the only one that emits
fail-loud capability signals, VRAM-residency hints, and verified OpenAI-compat correctness that
Hydra's trust layer can consume directly. Best-integrated, never exclusive.

## License

MIT — see [LICENSE](LICENSE). Built on MIT/Apache-2.0 engines only (llama.cpp, GGML, MLX, `go-openai`).
