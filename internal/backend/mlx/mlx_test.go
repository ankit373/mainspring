package mlx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
)

func TestBuildArgs(t *testing.T) {
	args := buildArgs(backend.ModelSpec{ID: "m", Path: "/models/m", CtxSize: 8192, ExtraArgs: []string{"--trust-remote-code"}}, "127.0.0.1", 9999)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-m mlx_lm.server", "--model /models/m", "--host 127.0.0.1", "--port 9999", "--max-tokens 8192", "--trust-remote-code"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

func TestDetectMissingPython(t *testing.T) {
	av := New("definitely-not-a-real-python-xyz").Detect(context.Background())
	if av.Present {
		t.Fatal("bogus python must be absent")
	}
	if av.Reason == "" {
		t.Fatal("absent detect must explain why")
	}
}

func TestDirOrFileSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dirOrFileSize(dir); got != 150 {
		t.Fatalf("dir size = %d, want 150", got)
	}
	if got := dirOrFileSize("/no/such/path/xyz"); got != 0 {
		t.Fatalf("missing path size = %d, want 0", got)
	}
}
