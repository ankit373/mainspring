package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
)

func mockDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"version":"0.5.0"}`)
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[{"name":"llama3.2:latest"},{"name":"qwen2.5-coder:7b"}]}`)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"model_info":{"llama.context_length":131072,"llama.block_count":32}}`)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[{"name":"llama3.2:latest","size":5000000000,"size_vram":5000000000}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDetect(t *testing.T) {
	srv := mockDaemon(t)
	av := New(srv.URL).Detect(context.Background())
	if !av.Present {
		t.Fatalf("expected present, got %+v", av)
	}
	if av.Version != "0.5.0" {
		t.Fatalf("version=%q", av.Version)
	}
}

func TestDetectAbsent(t *testing.T) {
	// Unroutable port; Detect must report absent with adopt-only guidance.
	av := New("http://127.0.0.1:1").Detect(context.Background())
	if av.Present {
		t.Fatal("expected absent")
	}
	if !strings.Contains(av.Reason, "adopt-only") {
		t.Fatalf("reason should note adopt-only, got %q", av.Reason)
	}
}

func TestStartKnownAndUnknown(t *testing.T) {
	srv := mockDaemon(t)
	b := New(srv.URL)

	// ":latest" tag matches the bare id.
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "llama3.2"}); err != nil {
		t.Fatalf("known model should start: %v", err)
	}
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "does-not-exist"}); err == nil {
		t.Fatal("unknown model must error (adopt-only, no auto-pull)")
	}
}

func TestCapabilitiesSurfacesCtxTrap(t *testing.T) {
	srv := mockDaemon(t)
	r, err := New(srv.URL).Start(context.Background(), backend.ModelSpec{ID: "llama3.2"})
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := r.Capabilities(context.Background())
	if caps.Device != "gpu" || !caps.GPUOffload {
		t.Fatalf("expected gpu offload from /api/ps, got device=%q offload=%v", caps.Device, caps.GPUOffload)
	}
	joined := strings.Join(caps.Warnings, " ")
	if !strings.Contains(joined, "per-request") || !strings.Contains(joined, "131072") {
		t.Fatalf("expected num_ctx trap warning with max_ctx, got %v", caps.Warnings)
	}
}
