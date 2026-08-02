# Mainspring — Engineering Instructions
# A backend-agnostic local inference server. Build the control plane, not the kernels.

## What Mainspring Is
A **standalone, backend-agnostic inference server** shipped as a Go binary (`mainspring`) and a
container image. It presents **one stable, versioned, conformance-tested OpenAI/Anthropic-compatible
API** and runs models on **pluggable engine backends** (llama.cpp, MLX, Ollama, …) behind a single
`Backend`/`Runner` interface. It installs and runs **only what a requested model needs** — no heavy
monolith.

The durable value is the layer *around* inference — **fail-loud correctness, VRAM-residency-aware
scheduling, and standalone governance** — not the compute kernels (those are MIT and already win).
**Never write an inference kernel.** Wrap `llama-server` / `mlx_lm.server` as subprocesses.

> **Relationship to Hydra:** these are **two control planes at different scopes** — do not describe
> Hydra as "a router". Hydra is the **Trust Control Plane**: it routes *across* heads to a target
> **confidence of correctness** (calibration, optimal-stopping ensembles, accountability ledger).
> Mainspring is the **inference control plane** for *one* head: admission control, keys and quotas,
> VRAM residency, and fail-loud truth about what actually ran. Hydra asks *which head should answer
> and how confident are we it's right*; Mainspring answers *what is true about this head right now*.
> Plain proxies (LiteLLM, OpenRouter) sit at Hydra's tier but only forward traffic — that is the tier
> comparison to draw, not one that flattens Hydra into them. They meet over the standard
> OpenAI-compatible HTTP boundary. Keep it clean so either evolves independently.
> **Mainspring must be useful deployed alone.**
>
> **Hydra stays provider-neutral and works with everything** (Ollama, OpenAI, Anthropic/Claude, any
> OpenAI-compatible endpoint) — Hydra never depends on Mainspring. Mainspring is the head Hydra
> integrates with *best* (fail-loud signals, VRAM hints, verified compat), never its only head.
> Nothing in Mainspring may assume it is the exclusive backend.

---

## Architecture

```
                         mainspring (Go)
   ┌───────────────────────────────────────────────────────┐
   │  OpenAI/Anthropic-compat API  (SSE streaming)          │  versioned, conformance-tested
   │  /v1/chat/completions  /v1/completions                 │
   │  /v1/embeddings  /v1/models  /capabilities  /healthz   │  ← fail-loud capabilities endpoint
   ├───────────────────────────────────────────────────────┤
   │  Control plane: API-key auth, per-tenant quotas,       │  ← the differentiator; standalone
   │  observability (TTFT, tok/s, spend), request logging   │
   ├───────────────────────────────────────────────────────┤
   │  Scheduler: VRAM-aware load / unload / swap + KeepAlive │  ← treats residency + swap cost first-class
   ├───────────────────────────────────────────────────────┤
   │  Backend interface (Detect / Start → Runner)           │  ← pattern copied from Ollama (MIT)
   │  Runner (BaseURL / Health / Capabilities / Stop / Mem) │
   ├───────────────┬───────────────┬───────────────────────┤
   │ llamacpp      │  mlx          │  ollama (adopt daemon) │  manageable backends
   │ (llama-server │  (mlx_lm      │  ── lmstudio/llamafile/│  detect-and-adopt-only
   │  subprocess)  │   subprocess) │     gpt4all: detect ───│  (never auto-install)
   └───────────────┴───────────────┴───────────────────────┘
```

### Phased plan
- **Phase 0 — MVP (current):** subprocess-supervise `llama-server`; health-poll; proxy OpenAI
  endpoints with SSE; versioned API; **fail-loud** (`/capabilities`, no silent CPU fallback / ctx
  truncation); API-key auth. Ships a real local head immediately. Single binary + container image.
- **Phase 1 — Multi-backend + governance:** MLX subprocess (Apple Silicon); flesh out the `Backend`
  interface; KeepAlive/slots; **VRAM-aware load/unload + multi-model swap**; per-tenant quotas;
  observability (spend ledger, TTFT, GPU util); OpenAI-compat conformance suite in CI.
- **Phase 1.5 — Deploy anywhere:** k8s manifests + Helm chart on the same image; GPU-node scheduling.
- **Phase 2 — (optional) in-process cgo backend:** only if subprocess overhead is a *measured* bottleneck.
- **Phase 3 — own kernels:** explicitly out of scope, forever.

---

## Package Map

| Package | Purpose |
|---|---|
| `cmd/mainspring` | CLI entry (Cobra): `serve`, `version`, `backends`, `models`. |
| `internal/backend` | `Backend` (Detect/Start) + `Runner` (BaseURL/Health/Capabilities/Stop/MemoryBytes) interfaces; the fail-loud `Capabilities` type. |
| `internal/backend/llamacpp` | v0 adapter: detect `llama-server`, subprocess-supervise it, health-poll, report real capabilities. |
| `internal/scheduler` | VRAM-aware `EnsureLoaded` + KeepAlive idle-unload + swap. The residency brain. |
| `internal/server` | OpenAI-compat HTTP server: streaming reverse-proxy to the active runner + `/capabilities` + `/healthz`. |
| `internal/auth` | API-key middleware. Fail-loud: warns prominently at startup if running open (no keys). |
| `internal/config` | Config load/save (`~/.config/mainspring/config.yaml`): addr, models, keys, keepalive, backend paths. |
| `internal/build` | Version vars set by ldflags. |
| `internal/util` | Shared utilities: `Accumulator` (bounded io.Writer) for subprocess stdout/stderr. |

### Key invariants
- **Never build an inference kernel.** Backends are subprocess wrappers around MIT/Apache engines.
- **`internal/util.Accumulator` must capture all subprocess stdout/stderr** — never an unbounded `bytes.Buffer`.
- **Fail loud, never silently degrade.** If the effective context < requested, or the run fell back to
  CPU, that MUST surface via `/capabilities` and a warning — never a silent success.
- **The API is a contract.** Breaking the OpenAI-compat shape is a major version bump + a deprecation
  window. Validate against real SDKs in CI (Phase 1).
- **VRAM must be reclaimed on model switch** — `Runner.Stop` terminates the process; verify no leak.
- **Standalone-first.** Never assume Hydra (or any router) sits in front. Auth/quotas live here too.

---

## Code Quality Standard — Non-Negotiable

**Everything here must be industry-best. No "good enough".**
- Be brutally honest in review. Wrong is wrong; mediocre is mediocre.
- **Run the race detector.** `go test -race ./...`. If it fails, it ships nothing.
- No duplicate logic. Dead code is a lie — delete it. Exported symbols must be used.
- User-visible output must be correct. If `go vet` / the linter flags it, fix it *before* review.
- The bar: would a senior engineer at a top systems shop approve this without comment?

## Karpathy Guidelines (always apply)
- Think before coding. State assumptions. Push back when a simpler approach exists.
- Minimum code. No speculative features. No abstractions for single-use code.
- Surgical changes. Match existing style. If it's 200 lines and could be 50, rewrite it.
- No error handling for impossible scenarios. No hallucinated APIs.

---

# Development Workflow — Issue-First, Always

> **Golden rule**: No code without a GitHub issue. No branch without an issue number.

## Branching Strategy (mirrors Hydra)
```
main                ← production only. NEVER pushed directly. Tags live here (release-please).
  ↑ squash PR
release/v1.x        ← UAT gate. RC pre-releases.
  ↑ squash PR
develop             ← integration. All features land here. Edge builds fire here.
  ↑ squash PR
feature/#{n}-slug   ← short-lived. Always branch from develop.
fix/#{n}-slug
chore/#{n}-slug
hotfix/#{n}-slug    ← branches from a main tag.
```

| Branch | Who pushes | Version bump | CI publishes |
|---|---|---|---|
| `main` | release-please PR only | YES (semver tag) | stable release + image |
| `release/v*` | cut from develop | no | RC pre-release |
| `develop` | feature PR merges | no | edge pre-release |
| `feature/*` `fix/*` `chore/*` | you | no | nothing |

## Conventional Commits (required)
`feat:` → minor · `fix:`/`perf:` → patch · `feat!:` / `BREAKING CHANGE:` → major ·
`refactor:`/`chore:`/`docs:`/`test:`/`ci:` → no bump. Never bump versions manually — release-please does.

```bash
git commit -m "feat(backend): add mlx subprocess adapter (#12)"
git commit -m "fix(server): flush SSE frames immediately on stream (#15)"
```

## PRs
- Title is a valid conventional commit. Body contains `Closes #<issue>`.
- Features/fixes target `develop`. Hotfixes target `main`. **Squash merge everywhere.**
- Never open a PR directly to `main` from a feature branch. Never force-push `develop`/`release/*`/`main`.

## GitHub Project Board
A project board for Mainspring must be created and its `--project-id` / Status field-id / option-ids
recorded here before the board workflow can run (same mechanics as Hydra's Project #2). Until then:
create the issue, branch `feature/#{n}-slug`, PR with `Closes #<n>`. **Do not fabricate board IDs.**

---

## STANDING CONSTRAINT — Attribution
**Never add a Claude / Claude Code co-author trailer or "Generated with Claude Code" attribution to any
commit or PR.** This is a hard rule with no exceptions.
