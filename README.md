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

## What it does (design goals)

- **One stable, versioned OpenAI/Anthropic-compatible API** — validated against the real SDKs in CI.
- **Pluggable backends** behind a `Backend`/`Runner` interface: `llamacpp` (v0), `mlx` (Apple), `ollama`
  (adopt an existing daemon), with `lmstudio` / `llamafile` / `gpt4all` as *detect-and-adopt-only*.
- **Fail loud, never silently degrade** — a `/capabilities` endpoint states the *actual* backend,
  whether it fell back to CPU, and the *effective* context window (never silently smaller than asked).
- **VRAM-residency-aware scheduling** — load / unload / swap with a KeepAlive idle timer.
- **Standalone governance** — its own API keys and per-tenant quotas, because it must be useful when
  deployed alone (k8s / shared GPU box), not only behind a router.
- **Detect-first, opt-in managed install** — use whatever engine is already present; install a missing
  one only when you enable it (signed, version-pinned, per-backend) — never silently.

## Non-goals

- **We do not build inference kernels.** The compute cores (llama.cpp/GGML, MLX) already win; we wrap them.
- No GUI dependency — headless-first daemon.
- We do not vendor Ollama's tree; we reimplement the outer control-plane layer cleanly (its pattern is MIT).

## Status

**Phase 0 (MVP) — in progress.** Subprocess-supervise `llama-server`, proxy the OpenAI endpoints with
SSE streaming, fail-loud `/capabilities`, API-key auth. See `CLAUDE.md` for the phased plan.

## Quick start (once Phase 0 lands)

```bash
# Build
go build -o mainspring ./cmd/mainspring

# See which engines are present on this host (detect-first)
./mainspring backends

# Serve a local GGUF model over the OpenAI API
./mainspring serve --model qwen2.5-coder=/path/to/model.gguf --addr :11500

# Talk to it with any OpenAI client
curl localhost:11500/v1/chat/completions -H 'content-type: application/json' -d '{
  "model": "qwen2.5-coder",
  "messages": [{"role":"user","content":"hello"}]
}'

# The fail-loud truth about what actually ran it
curl localhost:11500/capabilities
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
