// Package metrics records per-request inference telemetry — the observability
// the incumbents omit (whose documented fix is always "put a gateway in front").
// It aggregates counters/latency for a Prometheus /metrics endpoint and appends
// every request to a JSONL usage ledger. Stdlib-only: no Prometheus client dep.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is one completed inference request.
type Event struct {
	Time       time.Time `json:"time"`
	Model      string    `json:"model"`
	Tenant     string    `json:"tenant,omitempty"`
	Status     int       `json:"status"`
	Stream     bool      `json:"stream"`
	DurationMs float64   `json:"duration_ms"`
	TTFTMs     float64   `json:"ttft_ms,omitempty"` // 0 when not applicable
	Bytes      int64     `json:"bytes"`
	TokensEst  int64     `json:"tokens_est"`
}

type modelStat struct {
	requests  map[int]int64 // status -> count
	durSumMs  float64
	durCount  int64
	ttftSumMs float64
	ttftCount int64
	tokens    int64
}

// Recorder aggregates events and appends them to a ledger file.
type Recorder struct {
	mu    sync.Mutex
	stats map[string]*modelStat

	ledgerMu sync.Mutex
	ledger   io.WriteCloser
}

// New returns a Recorder. If ledgerPath is non-empty, events are appended there
// as JSONL (parent dirs created). A failure to open the ledger is returned; the
// recorder still works for in-memory metrics.
func New(ledgerPath string) (*Recorder, error) {
	r := &Recorder{stats: make(map[string]*modelStat)}
	if ledgerPath == "" {
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		return r, fmt.Errorf("usage ledger dir: %w", err)
	}
	f, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return r, fmt.Errorf("open usage ledger: %w", err)
	}
	r.ledger = f
	return r, nil
}

// Close flushes and closes the ledger.
func (r *Recorder) Close() error {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.ledger != nil {
		return r.ledger.Close()
	}
	return nil
}

// Record aggregates ev and appends it to the ledger.
func (r *Recorder) Record(ev Event) {
	r.mu.Lock()
	st := r.stats[ev.Model]
	if st == nil {
		st = &modelStat{requests: make(map[int]int64)}
		r.stats[ev.Model] = st
	}
	st.requests[ev.Status]++
	st.durSumMs += ev.DurationMs
	st.durCount++
	if ev.TTFTMs > 0 {
		st.ttftSumMs += ev.TTFTMs
		st.ttftCount++
	}
	st.tokens += ev.TokensEst
	r.mu.Unlock()

	r.appendLedger(ev)
}

func (r *Recorder) appendLedger(ev Event) {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.ledger == nil {
		return
	}
	if b, err := json.Marshal(ev); err == nil {
		_, _ = r.ledger.Write(append(b, '\n'))
	}
}

// Gauges are point-in-time values supplied at scrape time (e.g. residency).
type Gauges struct {
	LoadedModels  int
	ResidentBytes int64
	BudgetBytes   int64
}

// WritePrometheus renders the Prometheus text exposition format.
func (r *Recorder) WritePrometheus(w io.Writer, g Gauges) {
	r.mu.Lock()
	models := make([]string, 0, len(r.stats))
	for m := range r.stats {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]rowT, 0, len(models))
	for _, m := range models {
		st := r.stats[m]
		cp := make(map[int]int64, len(st.requests))
		for k, v := range st.requests {
			cp[k] = v
		}
		rows = append(rows, rowT{
			model: m, statuses: cp,
			durSum: st.durSumMs, ttftSum: st.ttftSumMs,
			durCount: st.durCount, ttftCount: st.ttftCount, tokens: st.tokens,
		})
	}
	r.mu.Unlock()

	fmt.Fprint(w, "# HELP mainspring_requests_total Inference requests by model and status.\n")
	fmt.Fprint(w, "# TYPE mainspring_requests_total counter\n")
	for _, rw := range rows {
		statuses := make([]int, 0, len(rw.statuses))
		for s := range rw.statuses {
			statuses = append(statuses, s)
		}
		sort.Ints(statuses)
		for _, s := range statuses {
			fmt.Fprintf(w, "mainspring_requests_total{model=%q,status=\"%d\"} %d\n", esc(rw.model), s, rw.statuses[s])
		}
	}

	writeCounter(w, "mainspring_request_duration_ms_sum", "Total request duration in ms by model.", rows, func(rw rowT) float64 { return rw.durSum })
	writeCounterI(w, "mainspring_request_duration_ms_count", "Request count by model (duration observations).", rows, func(rw rowT) int64 { return rw.durCount })
	writeCounter(w, "mainspring_ttft_ms_sum", "Total time-to-first-token in ms by model (streaming).", rows, func(rw rowT) float64 { return rw.ttftSum })
	writeCounterI(w, "mainspring_ttft_ms_count", "TTFT observations by model.", rows, func(rw rowT) int64 { return rw.ttftCount })
	writeCounterI(w, "mainspring_tokens_estimated_total", "Estimated output tokens by model.", rows, func(rw rowT) int64 { return rw.tokens })

	fmt.Fprint(w, "# HELP mainspring_loaded_models Currently resident models.\n# TYPE mainspring_loaded_models gauge\n")
	fmt.Fprintf(w, "mainspring_loaded_models %d\n", g.LoadedModels)
	fmt.Fprint(w, "# HELP mainspring_resident_bytes Estimated resident memory across models.\n# TYPE mainspring_resident_bytes gauge\n")
	fmt.Fprintf(w, "mainspring_resident_bytes %d\n", g.ResidentBytes)
	fmt.Fprint(w, "# HELP mainspring_budget_bytes Configured resident byte budget (0 = unbounded).\n# TYPE mainspring_budget_bytes gauge\n")
	fmt.Fprintf(w, "mainspring_budget_bytes %d\n", g.BudgetBytes)
}

type rowT = struct {
	model                       string
	statuses                    map[int]int64
	durSum, ttftSum             float64
	durCount, ttftCount, tokens int64
}

func writeCounter(w io.Writer, name, help string, rows []rowT, val func(rowT) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, rw := range rows {
		fmt.Fprintf(w, "%s{model=%q} %g\n", name, esc(rw.model), val(rw))
	}
}

func writeCounterI(w io.Writer, name, help string, rows []rowT, val func(rowT) int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, rw := range rows {
		fmt.Fprintf(w, "%s{model=%q} %d\n", name, esc(rw.model), val(rw))
	}
}

// esc escapes a Prometheus label value.
func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
