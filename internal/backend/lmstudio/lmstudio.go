// Package lmstudio is an adopt-if-present backend for LM Studio's local server,
// which speaks the OpenAI API on :1234. LM Studio's core is closed-source and
// ships as an Electron app, so Mainspring never installs or supervises it — we
// adopt a running server and proxy to it, adding fail-loud signals it omits.
package lmstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

const defaultHost = "http://127.0.0.1:1234"

// Backend adopts a running LM Studio server.
type Backend struct {
	Host string
}

// New returns an LM Studio backend; host "" uses the default local server.
func New(host string) *Backend {
	if host == "" {
		host = defaultHost
	}
	return &Backend{Host: strings.TrimRight(host, "/")}
}

func (b *Backend) Name() string { return "lmstudio" }

var client = &http.Client{Timeout: 3 * time.Second}

// Detect reports whether a reachable LM Studio server is present.
func (b *Backend) Detect(ctx context.Context) backend.Availability {
	av := backend.Availability{Name: "lmstudio", Path: b.Host}
	if _, err := b.listModels(ctx); err != nil {
		av.Reason = "no LM Studio server at " + b.Host + " — start it (LM Studio → Developer → Start Server), or `lms server start` (adopt-only; Mainspring will not install it)"
		return av
	}
	av.Present = true
	return av
}

// Start verifies the model is loaded in LM Studio and returns a proxy runner. It
// does not load models (adopt-only).
func (b *Backend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	models, err := b.listModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("lmstudio: %w", err)
	}
	found := false
	for _, m := range models {
		if m == spec.ID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("lmstudio: model %q not loaded — load it in LM Studio first (Mainspring will not load it)", spec.ID)
	}
	return &runner{host: b.Host, spec: spec}, nil
}

func (b *Backend) listModels(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.Host+"/v1/models", nil)
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

// runner adopts the LM Studio server for one model.
type runner struct {
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

// MemoryBytes is unknown for an adopted LM Studio server (it manages its own).
func (r *runner) MemoryBytes() int64 { return 0 }

// Stop is a no-op: we adopt LM Studio, we don't own its process.
func (r *runner) Stop(context.Context) error { return nil }

// Capabilities: LM Studio doesn't expose device/offload or effective context
// over its OpenAI API, so we report unknown and say so rather than guessing.
func (r *runner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{
		Backend:      "lmstudio",
		Model:        r.spec.ID,
		Device:       "unknown",
		RequestedCtx: r.spec.CtxSize,
		Warnings:     []string{"LM Studio does not expose device/offload or effective context over its API; set the context and GPU offload in the LM Studio app"},
	}, nil
}
