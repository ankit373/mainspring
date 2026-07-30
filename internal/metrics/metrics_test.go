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

func TestLatencyPercentiles(t *testing.T) {
	r, _ := New("")
	if p := r.LatencyPercentiles("none"); p != (Percentiles{}) {
		t.Fatalf("no samples => zero value, got %+v", p)
	}
	now := time.Unix(1700000000, 0)
	// Duration samples 10..100 (step 10); TTFT samples 1..10 (step 1).
	for i := 1; i <= 10; i++ {
		r.Record(Event{Time: now, Model: "m1", Status: 200, DurationMs: float64(i * 10), TTFTMs: float64(i)})
	}
	p := r.LatencyPercentiles("m1")
	// Sorted duration = [10..100]; p50 index int(0.5*10)=5 -> 60; p90 index 9 -> 100; p99 index 9 (clamped) -> 100.
	if p.DurationP50 != 60 {
		t.Fatalf("DurationP50 = %v, want 60", p.DurationP50)
	}
	if p.DurationP90 != 100 {
		t.Fatalf("DurationP90 = %v, want 100", p.DurationP90)
	}
	if p.DurationP99 != 100 {
		t.Fatalf("DurationP99 = %v, want 100", p.DurationP99)
	}
	if p.TTFTP50 != 6 {
		t.Fatalf("TTFTP50 = %v, want 6", p.TTFTP50)
	}
	// TTFTp50 back-compat wrapper must agree.
	if got := r.TTFTp50("m1"); got != p.TTFTP50 {
		t.Fatalf("TTFTp50() = %v, want %v (LatencyPercentiles.TTFTP50)", got, p.TTFTP50)
	}
}

func TestPercentileHelper(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty samples => 0, got %v", got)
	}
	samples := []float64{50, 10, 30, 20, 40} // sorted: 10 20 30 40 50
	if got := percentile(samples, 0.5); got != 30 {
		t.Fatalf("p50 = %v, want 30", got)
	}
	if got := percentile(samples, 0.99); got != 50 {
		t.Fatalf("p99 = %v, want 50 (clamped to max)", got)
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

func TestTenantUsageRollup(t *testing.T) {
	r, _ := New("")
	now := time.Unix(1700000000, 0)
	r.Record(Event{Time: now, Model: "m1", Tenant: "alice", Status: 200, PromptTokens: 10, TokensEst: 5, CostUSD: 0.01})
	r.Record(Event{Time: now, Model: "m1", Tenant: "alice", Status: 200, PromptTokens: 20, TokensEst: 8, CostUSD: 0.02})
	r.Record(Event{Time: now, Model: "m1", Status: 200, PromptTokens: 3, TokensEst: 2}) // no tenant → anonymous

	usage := r.TenantUsage()
	if len(usage) != 2 {
		t.Fatalf("got %d tenants, want 2: %+v", len(usage), usage)
	}
	// Sorted by name: "(anonymous)" sorts before "alice".
	if usage[0].Tenant != anonymousTenant || usage[0].Requests != 1 {
		t.Fatalf("anonymous rollup wrong: %+v", usage[0])
	}
	a := usage[1]
	if a.Tenant != "alice" || a.Requests != 2 || a.PromptTokens != 30 || a.OutputTokens != 13 {
		t.Fatalf("alice rollup wrong: %+v", a)
	}
	if a.CostUSD < 0.0299 || a.CostUSD > 0.0301 {
		t.Fatalf("alice cost = %v, want ~0.03", a.CostUSD)
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
