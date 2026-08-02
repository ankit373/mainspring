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
  `/embeddings`, `/models`, and Anthropic `/v1/messages` (text **and** tool use, streaming translated)
  plus `/v1/messages/count_tokens` for pre-flighting a context budget —
  **both dialects validated against their real Python and JS SDKs in CI**, failure paths included (an
  upstream 5xx, a stream cut mid-flight). Both dialects also run the **same** pipeline — auth,
  per-tenant budgets, context guardrail, clamp, concurrency gate, timeouts, circuit breaker, cost
  accounting, response cache, coalescing, retry and model fallback. They differ in what they put on
  the wire, not in how a request is governed.
- **Pluggable backends** behind a `Backend`/`Runner` interface: `llamacpp` and `mlx` (managed
  subprocess) · `ollama` / `lmstudio` / `llamafile` / `gpt4all` (detect-and-adopt-only, never installed).
  **Per-model routing, aliases, and ordered fallback chains** — one server can serve several models on
  different engines and fail over when one is down.
- **Fail loud, never silently degrade** — `/capabilities` and `X-Mainspring-{Backend,Device,Warning}`
  headers state the *actual* backend, whether it fell back to CPU, and the *effective* context window.
- **VRAM-residency-aware scheduling** — single-flight load, byte-budget + LRU eviction, KeepAlive idle
  unload, optional preload.
- **Reliability** — per-model concurrency limit + bounded queue (503 backpressure), a **circuit breaker**
  with a background health probe, **retry-with-backoff** on transient upstream failures (safe for streams),
  **model-level fallback chains** (a different model answers when the primary is down), and a structured
  error taxonomy (stable `code` per failure class) that **every** response goes through — an unrouted
  path answers `route_not_found` and a wrong method `method_not_allowed`, not Go's plain text.
- **Opt-in response cache** — identical deterministic (temperature 0) non-streaming requests return from a
  bounded TTL+LRU cache without re-running the model; hits carry `X-Mainspring-Cache: hit`.
- **Request coalescing** — a burst of identical in-flight deterministic requests shares one backend
  computation (single-flight); followers carry `X-Mainspring-Coalesced: true`.
- **Context guardrail** — optional `enforce_context` rejects over-context requests (`context_length_exceeded`)
  instead of letting the engine silently truncate, or downgrades to a warning header per model.
- **Standalone governance** — API-key tenants with roles (admin/inference), per-tenant rate + token
  budgets, and **real** streaming token accounting.
- **Cost accounting** — optional per-model USD pricing turns real usage into spend, surfaced in the
  usage ledger, a `mainspring_cost_usd_total` metric, and `/v1/quality` (a real cost signal for routing).
- **Operability** — Prometheus `/metrics`, JSONL usage ledger, `X-Request-ID` + W3C `traceparent`
  propagation, optional access log, **SIGHUP hot-reload** (carries flag-supplied models and keys
  across, and refuses any reload that would leave a running server unauthenticated), an **admin API**
  (drain / reload / model load-unload), **TLS**, graceful drain, and a `/v1/quality` routing signal a
  trust control plane like Hydra can consume.
- **Detect-first, opt-in managed install** — use whatever engine is present; install a missing one only
  when you explicitly run `mainspring install`, never silently. Every install is SHA-256-verified and
  refuses on a mismatch. Mainspring does **not** bundle a catalogue of engine builds: you point it at
  the artifact you want with `--url` + `--sha256`, or at your own manifest (optionally ed25519-signed
  via `--pubkey`). Shipping a curated, pinned manifest is tracked separately.
- **Deploy anywhere** — a single static binary, a distroless container image, and a Helm chart
  (HPA / ServiceMonitor / GPU node scheduling).

## Non-goals

- **We do not build inference kernels.** The compute cores (llama.cpp/GGML, MLX) already win; we wrap them.
- No GUI dependency — headless-first daemon.
- We do not vendor Ollama's tree; we reimplement the outer control-plane layer cleanly (its pattern is MIT).

## Status

**v0.2.0 — released.** The full control plane is shipped: OpenAI + Anthropic APIs, six backends with
per-model routing / aliases / fallback, VRAM-aware scheduling that never evicts a runner out from under
an in-flight request, governance with quota-headroom headers, circuit breaker (open vs half-open
distinguished in `/metrics`), response cache + request coalescing, cost accounting, latency and
queue-wait percentiles, TLS, admin API, request-ID + trace-context, and `/v1/quality`.
`go test -race` clean across the tree. See
[CHANGELOG.md](CHANGELOG.md) and the [releases page](https://github.com/ankit373/mainspring/releases).

**Since v0.2.0 (on `develop`, unreleased)** a full correctness audit of the tree closed 17 issues.
Anthropic `/v1/messages` reached parity with the OpenAI path — it had been skipping the clamp, the
context guardrail, cost accounting and the circuit breaker while recording a hardcoded `200` for every
request. The response cache stopped replaying one tenant's request id and quota headroom to another,
and stopped treating an *omitted* `temperature` as deterministic. A config reload can no longer wipe
flag-supplied models or silently drop the server into open mode. Truncated responses are no longer
served as successes or cached, and a client's own disconnect is no longer charged to the circuit
breaker at any point — while a real timeout still is. Rejections, admin principals and ledger failures
all now appear in the telemetry that claimed to cover them.

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

These are **two control planes at different scopes**, not a router and a server.
[Hydra](https://github.com/ankit373/hydra) is the **Trust Control Plane**: it routes *across* heads to
a target **confidence of correctness**. Mainspring is the **inference control plane** for *one* head:
admission control, VRAM residency, and fail-loud truth about what actually ran.

Hydra asks *which head should answer this, and how confident are we that it's right*. Mainspring
answers *what is true about this head right now*. They meet over the standard OpenAI-compatible HTTP
boundary, so either can evolve independently.

**Hydra stays provider-neutral — it works with everything** (Ollama, OpenAI, Anthropic/Claude, any
OpenAI-compatible endpoint). Mainspring is not a replacement for that and Hydra never depends on it;
because Mainspring speaks the same standard API, Hydra treats it like any other head. What makes
Mainspring special is that it's the head Hydra works with *best* — it's the only one that emits
fail-loud capability signals, VRAM-residency hints, and verified OpenAI-compat correctness that
Hydra's trust layer can consume directly. Best-integrated, never exclusive.

## Contributing

Issue first, always — see [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow, the commit
convention, and the `go test -race` bar. Security issues go through
[SECURITY.md](SECURITY.md), never a public issue.

## License

MIT — see [LICENSE](LICENSE). Built on MIT/Apache-2.0 engines only (llama.cpp, GGML, MLX, `go-openai`).
