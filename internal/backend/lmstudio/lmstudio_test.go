package lmstudio

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
)

func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5-7b-instruct","object":"model"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDetect(t *testing.T) {
	srv := mockServer(t)
	if av := New(srv.URL).Detect(context.Background()); !av.Present {
		t.Fatalf("expected present, got %+v", av)
	}
}

func TestDetectAbsent(t *testing.T) {
	av := New("http://127.0.0.1:1").Detect(context.Background())
	if av.Present {
		t.Fatal("expected absent")
	}
	if !strings.Contains(av.Reason, "adopt-only") {
		t.Fatalf("reason should note adopt-only, got %q", av.Reason)
	}
}

func TestStartKnownAndUnknown(t *testing.T) {
	srv := mockServer(t)
	b := New(srv.URL)
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "qwen2.5-7b-instruct"}); err != nil {
		t.Fatalf("loaded model should start: %v", err)
	}
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "not-loaded"}); err == nil {
		t.Fatal("unloaded model must error (adopt-only)")
	}
}

func TestCapabilitiesHonestUnknown(t *testing.T) {
	srv := mockServer(t)
	r, err := New(srv.URL).Start(context.Background(), backend.ModelSpec{ID: "qwen2.5-7b-instruct"})
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := r.Capabilities(context.Background())
	if caps.Device != "unknown" {
		t.Fatalf("LM Studio device should be reported unknown, got %q", caps.Device)
	}
	if len(caps.Warnings) == 0 {
		t.Fatal("should warn that device/ctx are not exposed")
	}
}
