package openaiadopt

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
		io.WriteString(w, `{"object":"list","data":[{"id":"mymodel","object":"model"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDetect(t *testing.T) {
	srv := mockServer(t)
	if av := New("llamafile", srv.URL, "start it").Detect(context.Background()); !av.Present {
		t.Fatalf("expected present, got %+v", av)
	}
}

func TestDetectAbsentUsesHint(t *testing.T) {
	av := New("llamafile", "http://127.0.0.1:1", "run the server thing").Detect(context.Background())
	if av.Present {
		t.Fatal("expected absent")
	}
	if !strings.Contains(av.Reason, "run the server thing") || !strings.Contains(av.Reason, "adopt-only") {
		t.Fatalf("reason should include the hint and adopt-only note, got %q", av.Reason)
	}
}

func TestStartKnownAndUnknown(t *testing.T) {
	srv := mockServer(t)
	b := New("llamafile", srv.URL, "start it")
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "mymodel"}); err != nil {
		t.Fatalf("loaded model should start: %v", err)
	}
	if _, err := b.Start(context.Background(), backend.ModelSpec{ID: "nope"}); err == nil {
		t.Fatal("unloaded model must error (adopt-only)")
	}
}

func TestCapabilitiesHonestUnknown(t *testing.T) {
	srv := mockServer(t)
	r, err := New("gpt4all", srv.URL, "enable API").Start(context.Background(), backend.ModelSpec{ID: "mymodel"})
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := r.Capabilities(context.Background())
	if caps.Backend != "gpt4all" || caps.Device != "unknown" || len(caps.Warnings) == 0 {
		t.Fatalf("expected honest unknown caps tagged with backend name, got %+v", caps)
	}
}
