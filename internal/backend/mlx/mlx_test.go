package mlx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// buildArgs' full contract lives in runner_test.go
// (TestBuildArgsDoesNotPassCtxAsAGenerationCap). The version that used to live
// here asserted `--max-tokens 8192` from `CtxSize: 8192`, which locked in the
// bug fixed in #213 — a test can make a wrong mapping look deliberate.

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
