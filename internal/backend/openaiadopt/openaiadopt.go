// Package openaiadopt is a reusable adopt-if-present backend for any local
// server that already speaks the OpenAI API (llamafile, GPT4All, …). Mainspring
// never installs or supervises these — it detects a running server, verifies the
// requested model is loaded, and proxies to it, adding an honest capabilities
// note (these servers don't expose device/offload/effective-ctx over their API).
package openaiadopt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

// Backend adopts a running OpenAI-compatible server.
type Backend struct {
	name string
	host string
	hint string // shown when absent: how to start/enable the server
}

// New builds an adopt backend. host must be fully resolved (with any default
// applied by the caller); hint explains how to make the server available.
func New(name, host, hint string) *Backend {
	return &Backend{name: name, host: strings.TrimRight(host, "/"), hint: hint}
}

func (b *Backend) Name() string { return b.name }

var client = &http.Client{Timeout: 3 * time.Second}

// Detect reports whether the server is reachable.
func (b *Backend) Detect(ctx context.Context) backend.Availability {
	av := backend.Availability{Name: b.name, Path: b.host}
	if _, err := b.listModels(ctx); err != nil {
		av.Reason = fmt.Sprintf("no %s server at %s — %s (adopt-only; Mainspring will not install it)", b.name, b.host, b.hint)
		return av
	}
	av.Present = true
	return av
}

// Start verifies the model is loaded and returns a proxy runner (no auto-load).
func (b *Backend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	models, err := b.listModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.name, err)
	}
	if !slices.Contains(models, spec.ID) {
		return nil, backend.MissingModelError(b.name, b.host, spec, models,
			fmt.Sprintf("Load it in %s first (Mainspring will not load it).", b.name))
	}
	return &runner{name: b.name, host: b.host, spec: spec}, nil
}

// ListModels enumerates the models the adopted OpenAI-compatible server exposes.
func (b *Backend) ListModels(ctx context.Context) ([]string, error) { return b.listModels(ctx) }

func (b *Backend) listModels(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.host+"/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

type runner struct {
	name string
	host string
	spec backend.ModelSpec
}

func (r *runner) BaseURL() string { return r.host }

func (r *runner) Health(ctx context.Context) backend.Status {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.host+"/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		return backend.StatusDown
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return backend.StatusReady
	}
	return backend.StatusDown
}

// MemoryBytes is unknown for an adopted server (it manages its own memory).
func (r *runner) MemoryBytes() int64 { return 0 }

// Stop is a no-op: we adopt the server, we don't own its process.
func (r *runner) Stop(context.Context) error { return nil }

// Capabilities honestly reports unknown device/ctx — these servers don't expose
// them over the OpenAI API, so we say so rather than guessing.
func (r *runner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{
		Backend:      r.name,
		Model:        r.spec.ID,
		Device:       "unknown",
		RequestedCtx: r.spec.CtxSize,
		Warnings:     []string{r.name + " does not expose device/offload or effective context over its API"},
	}, nil
}
