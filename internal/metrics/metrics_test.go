package metrics

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordAndPrometheus(t *testing.T) {
	r, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	r.Record(Event{Time: now, Model: "m1", Status: 200, Stream: true, DurationMs: 100, TTFTMs: 20, Bytes: 50, TokensEst: 8})
	r.Record(Event{Time: now, Model: "m1", Status: 200, Stream: false, DurationMs: 40, Bytes: 30, TokensEst: 5})
	r.Record(Event{Time: now, Model: "m1", Status: 404, Stream: false, DurationMs: 1})

	var buf bytes.Buffer
	r.WritePrometheus(&buf, Gauges{LoadedModels: 1, ResidentBytes: 1234, BudgetBytes: 5000})
	out := buf.String()

	for _, want := range []string{
		`mainspring_requests_total{model="m1",status="200"} 2`,
		`mainspring_requests_total{model="m1",status="404"} 1`,
		`mainspring_request_duration_ms_sum{model="m1"} 141`,
		`mainspring_request_duration_ms_count{model="m1"} 3`,
		`mainspring_ttft_ms_count{model="m1"} 1`,
		`mainspring_tokens_estimated_total{model="m1"} 13`,
		`mainspring_loaded_models 1`,
		`mainspring_resident_bytes 1234`,
		`mainspring_budget_bytes 5000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prometheus output missing %q\n---\n%s", want, out)
		}
	}
}

func TestResilienceCounters(t *testing.T) {
	r, _ := New("")
	now := time.Unix(1700000000, 0)
	r.Record(Event{Time: now, Model: "m1", Status: 200, Retries: 2, Fallback: true})
	r.Record(Event{Time: now, Model: "m1", Status: 200, Retries: 1, Coalesced: true})
	r.Record(Event{Time: now, Model: "m1", Status: 200}) // no resilience events

	var buf bytes.Buffer
	r.WritePrometheus(&buf, Gauges{})
	out := buf.String()
	for _, want := range []string{
		`mainspring_retries_total{model="m1"} 3`,   // 2 + 1
		`mainspring_coalesced_total{model="m1"} 1`, // one coalesced
		`mainspring_fallback_total{model="m1"} 1`,  // one fallback
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prometheus output missing %q\n---\n%s", want, out)
		}
	}
}

func TestTTFTp50(t *testing.T) {
	r, _ := New("")
	if r.TTFTp50("none") != 0 {
		t.Fatal("no samples => 0")
	}
	now := time.Unix(1700000000, 0)
	for _, v := range []float64{10, 30, 20, 50, 40} { // median = 30
		r.Record(Event{Time: now, Model: "m1", Status: 200, TTFTMs: v})
	}
	if got := r.TTFTp50("m1"); got != 30 {
		t.Fatalf("p50 = %v, want 30", got)
	}
}

func TestLedgerAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.jsonl")
	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Record(Event{Model: "m1", Status: 200})
	r.Record(Event{Model: "m2", Status: 200})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("ledger not created: %v", err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("expected 2 ledger lines, got %d", lines)
	}
}
