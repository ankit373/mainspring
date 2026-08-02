// Package metrics records per-request inference telemetry — the observability
// the incumbents omit (whose documented fix is always "put a gateway in front").
// It aggregates counters/latency for a Prometheus /metrics endpoint and appends
// every request to a JSONL usage ledger. Stdlib-only: no Prometheus client dep.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/util"
)

// Event is one completed inference request.
type Event struct {
	Time         time.Time `json:"time"`
	RequestID    string    `json:"request_id,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`
	Model        string    `json:"model"`
	Tenant       string    `json:"tenant,omitempty"`
	Status       int       `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"` // taxonomy code when the request was refused (503 busy vs circuit-open, …)
	Stream       bool      `json:"stream"`
	Cached       bool      `json:"cached,omitempty"`    // served from the response cache (no backend hit)
	Coalesced    bool      `json:"coalesced,omitempty"` // served by sharing an in-flight leader's result
	Fallback     bool      `json:"fallback,omitempty"`  // served by a fallback model (not the requested one)
	Retries      int       `json:"retries,omitempty"`   // upstream retries this request incurred
	DurationMs   float64   `json:"duration_ms"`
	TTFTMs       float64   `json:"ttft_ms,omitempty"`       // 0 when not applicable
	QueueWaitMs  float64   `json:"queue_wait_ms,omitempty"` // time spent waiting for a concurrency-gate slot (0 = none/disabled)
	Bytes        int64     `json:"bytes"`
	PromptTokens int64     `json:"prompt_tokens,omitempty"` // real, when upstream reports usage
	TokensEst    int64     `json:"tokens_est"`              // output tokens: real when Exact, else estimated
	Exact        bool      `json:"exact_usage,omitempty"`   // true when counts came from an upstream usage object
	CostUSD      float64   `json:"cost_usd,omitempty"`      // computed spend for this request (0 when no rate configured / no exact usage)
}

// quantileRingSize bounds the recent-sample reservoir kept per model for
// percentile estimates (duration and TTFT alike).
const quantileRingSize = 256

// quantileRing is a bounded reservoir of recent float64 samples, used to
// estimate percentiles without storing an unbounded per-model history.
type quantileRing struct {
	buf    []float64
	at     int
	filled bool
}

func (q *quantileRing) add(v float64) {
	if q.buf == nil {
		q.buf = make([]float64, quantileRingSize)
	}
	q.buf[q.at] = v
	q.at = (q.at + 1) % quantileRingSize
	if q.at == 0 {
		q.filled = true
	}
}

// samples returns a copy of the live samples (bounded by how much of the ring
// has been written so far).
func (q *quantileRing) samples() []float64 {
	n := len(q.buf)
	if !q.filled {
		n = q.at
	}
	out := make([]float64, n)
	copy(out, q.buf[:n])
	return out
}

// percentile returns the p-th percentile (0<p<=1) of samples via nearest-rank,
// or 0 for an empty set. Not an exact global quantile — an estimate over the
// bounded reservoir.
func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type modelStat struct {
	requests     map[int]int64 // status -> count
	durSumMs     float64
	durCount     int64
	ttftSumMs    float64
	ttftCount    int64
	tokens       int64
	promptTokens int64
	costUSD      float64
	retries      int64
	coalesced    int64
	fallback     int64

	durRing   quantileRing // recent total-duration samples (ms)
	ttftRing  quantileRing // recent TTFT samples (ms)
	queueRing quantileRing // recent gate queue-wait samples (ms)
}

// tenantStat is a per-principal consumption rollup (actuals, complementing the
// per-tenant token *budget* enforced in the auth layer).
type tenantStat struct {
	requests     int64
	promptTokens int64
	outputTokens int64
	costUSD      float64
}

// Recorder aggregates events and appends them to a ledger file.
type Recorder struct {
	mu      sync.Mutex
	stats   map[string]*modelStat
	tenants map[string]*tenantStat

	ledgerMu  sync.Mutex
	ledger    io.WriteCloser
	writeErrd bool // a ledger write has already failed and been logged
}

// anonymousTenant is the rollup bucket for unauthenticated (open-mode) requests.
const anonymousTenant = "(anonymous)"

// New returns a Recorder. If ledgerPath is non-empty, events are appended there
// as JSONL (parent dirs created). A failure to open the ledger is returned; the
// recorder still works for in-memory metrics.
func New(ledgerPath string) (*Recorder, error) {
	r := &Recorder{stats: make(map[string]*modelStat), tenants: make(map[string]*tenantStat)}
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

// LedgerActive reports whether the JSONL usage ledger is really open. A
// configured path that failed to open leaves the recorder in-memory only, and an
// operator must be able to see that instead of being told the ledger is on
// because a path was set.
func (r *Recorder) LedgerActive() bool {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	return r.ledger != nil
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
	st.durRing.add(ev.DurationMs)
	st.queueRing.add(ev.QueueWaitMs)
	if ev.TTFTMs > 0 {
		st.ttftSumMs += ev.TTFTMs
		st.ttftCount++
		st.ttftRing.add(ev.TTFTMs)
	}
	st.tokens += ev.TokensEst
	st.promptTokens += ev.PromptTokens
	st.costUSD += ev.CostUSD
	st.retries += int64(ev.Retries)
	if ev.Coalesced {
		st.coalesced++
	}
	if ev.Fallback {
		st.fallback++
	}

	tname := ev.Tenant
	if tname == "" {
		tname = anonymousTenant
	}
	ts := r.tenants[tname]
	if ts == nil {
		ts = &tenantStat{}
		r.tenants[tname] = ts
	}
	ts.requests++
	ts.promptTokens += ev.PromptTokens
	ts.outputTokens += ev.TokensEst
	ts.costUSD += ev.CostUSD
	r.mu.Unlock()

	r.appendLedger(ev)
}

// appendLedger writes one event to the JSONL ledger. A write failure — a full
// disk, a rotated-away file — silently stops the accounting record, so the first
// one is logged; the rest are suppressed so a failing disk cannot flood the log
// at request rate.
func (r *Recorder) appendLedger(ev Event) {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.ledger == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := r.ledger.Write(append(b, '\n')); err != nil && !r.writeErrd {
		r.writeErrd = true
		log.Printf("usage ledger write failed, accounting records are being lost (further errors suppressed): %v", err)
	}
}

// Percentiles is a model's latency percentile snapshot (ms), estimated over a
// bounded recent-sample reservoir — not an exact global quantile.
type Percentiles struct {
	DurationP50, DurationP90, DurationP99    float64
	TTFTP50, TTFTP90, TTFTP99                float64
	QueueWaitP50, QueueWaitP90, QueueWaitP99 float64
}

// LatencyPercentiles returns duration, TTFT, and gate queue-wait p50/p90/p99
// (ms) for a model, or all zeros when there are no samples yet.
func (r *Recorder) LatencyPercentiles(model string) Percentiles {
	r.mu.Lock()
	st := r.stats[model]
	if st == nil {
		r.mu.Unlock()
		return Percentiles{}
	}
	dur, ttft, queue := st.durRing.samples(), st.ttftRing.samples(), st.queueRing.samples()
	r.mu.Unlock()

	return Percentiles{
		DurationP50:  percentile(dur, 0.5),
		DurationP90:  percentile(dur, 0.9),
		DurationP99:  percentile(dur, 0.99),
		TTFTP50:      percentile(ttft, 0.5),
		TTFTP90:      percentile(ttft, 0.9),
		TTFTP99:      percentile(ttft, 0.99),
		QueueWaitP50: percentile(queue, 0.5),
		QueueWaitP90: percentile(queue, 0.9),
		QueueWaitP99: percentile(queue, 0.99),
	}
}

// Costs returns accumulated USD spend per model and the grand total. Models with
// no configured rate contribute 0.
func (r *Recorder) Costs() (perModel map[string]float64, total float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	perModel = make(map[string]float64, len(r.stats))
	for m, st := range r.stats {
		perModel[m] = st.costUSD
		total += st.costUSD
	}
	return perModel, total
}

// TenantUsage is one principal's consumption rollup.
type TenantUsage struct {
	Tenant       string  `json:"tenant"`
	Requests     int64   `json:"requests"`
	PromptTokens int64   `json:"prompt_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// TenantUsage returns per-tenant consumption, sorted by tenant name.
func (r *Recorder) TenantUsage() []TenantUsage {
	r.mu.Lock()
	names := make([]string, 0, len(r.tenants))
	for n := range r.tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]TenantUsage, 0, len(names))
	for _, n := range names {
		ts := r.tenants[n]
		out = append(out, TenantUsage{
			Tenant:       n,
			Requests:     ts.requests,
			PromptTokens: ts.promptTokens,
			OutputTokens: ts.outputTokens,
			CostUSD:      ts.costUSD,
		})
	}
	r.mu.Unlock()
	return out
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
		dur, ttft, queue := st.durRing.samples(), st.ttftRing.samples(), st.queueRing.samples()
		rows = append(rows, rowT{
			model: m, statuses: cp,
			durSum: st.durSumMs, ttftSum: st.ttftSumMs,
			durCount: st.durCount, ttftCount: st.ttftCount,
			tokens: st.tokens, promptTokens: st.promptTokens,
			costUSD:   st.costUSD,
			retries:   st.retries,
			coalesced: st.coalesced,
			fallback:  st.fallback,
			durP50:    percentile(dur, 0.5), durP90: percentile(dur, 0.9), durP99: percentile(dur, 0.99),
			ttftP50: percentile(ttft, 0.5), ttftP90: percentile(ttft, 0.9), ttftP99: percentile(ttft, 0.99),
			queueP50: percentile(queue, 0.5), queueP90: percentile(queue, 0.9), queueP99: percentile(queue, 0.99),
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
			fmt.Fprintf(w, "mainspring_requests_total{model=\"%s\",status=\"%d\"} %d\n", util.PromLabelValue(rw.model), s, rw.statuses[s])
		}
	}

	writeCounter(w, "mainspring_request_duration_ms_sum", "Total request duration in ms by model.", rows, func(rw rowT) float64 { return rw.durSum })
	writeCounterI(w, "mainspring_request_duration_ms_count", "Request count by model (duration observations).", rows, func(rw rowT) int64 { return rw.durCount })
	writeCounter(w, "mainspring_ttft_ms_sum", "Total time-to-first-token in ms by model (streaming).", rows, func(rw rowT) float64 { return rw.ttftSum })
	writeCounterI(w, "mainspring_ttft_ms_count", "TTFT observations by model.", rows, func(rw rowT) int64 { return rw.ttftCount })
	writeCounterI(w, "mainspring_tokens_estimated_total", "Output tokens by model (real when upstream reports usage, else estimated).", rows, func(rw rowT) int64 { return rw.tokens })
	writeCounterI(w, "mainspring_prompt_tokens_total", "Prompt (input) tokens by model (real; 0 when upstream reports no usage).", rows, func(rw rowT) int64 { return rw.promptTokens })
	writeCounter(w, "mainspring_cost_usd_total", "Computed spend in USD by model (0 when no rate configured).", rows, func(rw rowT) float64 { return rw.costUSD })
	writeCounterI(w, "mainspring_retries_total", "Upstream retries by model (transient-failure retries).", rows, func(rw rowT) int64 { return rw.retries })
	writeCounterI(w, "mainspring_coalesced_total", "Requests served by coalescing onto an in-flight leader, by model.", rows, func(rw rowT) int64 { return rw.coalesced })
	writeCounterI(w, "mainspring_fallback_total", "Requests served by a fallback model, by (serving) model.", rows, func(rw rowT) int64 { return rw.fallback })
	writeGauge(w, "mainspring_duration_ms_p50", "Request duration p50 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.durP50 })
	writeGauge(w, "mainspring_duration_ms_p90", "Request duration p90 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.durP90 })
	writeGauge(w, "mainspring_duration_ms_p99", "Request duration p99 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.durP99 })
	writeGauge(w, "mainspring_ttft_ms_p50", "Time-to-first-token p50 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.ttftP50 })
	writeGauge(w, "mainspring_ttft_ms_p90", "Time-to-first-token p90 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.ttftP90 })
	writeGauge(w, "mainspring_ttft_ms_p99", "Time-to-first-token p99 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.ttftP99 })
	writeGauge(w, "mainspring_queue_wait_ms_p50", "Concurrency-gate queue wait p50 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.queueP50 })
	writeGauge(w, "mainspring_queue_wait_ms_p90", "Concurrency-gate queue wait p90 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.queueP90 })
	writeGauge(w, "mainspring_queue_wait_ms_p99", "Concurrency-gate queue wait p99 (ms) by model, recent-sample estimate.", rows, func(rw rowT) float64 { return rw.queueP99 })

	fmt.Fprint(w, "# HELP mainspring_loaded_models Currently resident models.\n# TYPE mainspring_loaded_models gauge\n")
	fmt.Fprintf(w, "mainspring_loaded_models %d\n", g.LoadedModels)
	fmt.Fprint(w, "# HELP mainspring_resident_bytes Estimated resident memory across models.\n# TYPE mainspring_resident_bytes gauge\n")
	fmt.Fprintf(w, "mainspring_resident_bytes %d\n", g.ResidentBytes)
	fmt.Fprint(w, "# HELP mainspring_budget_bytes Configured resident byte budget (0 = unbounded).\n# TYPE mainspring_budget_bytes gauge\n")
	fmt.Fprintf(w, "mainspring_budget_bytes %d\n", g.BudgetBytes)
}

type rowT = struct {
	model                                     string
	statuses                                  map[int]int64
	durSum, ttftSum                           float64
	durCount, ttftCount, tokens, promptTokens int64
	costUSD                                   float64
	retries, coalesced, fallback              int64
	durP50, durP90, durP99                    float64
	ttftP50, ttftP90, ttftP99                 float64
	queueP50, queueP90, queueP99              float64
}

func writeCounter(w io.Writer, name, help string, rows []rowT, val func(rowT) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, rw := range rows {
		fmt.Fprintf(w, "%s{model=\"%s\"} %g\n", name, util.PromLabelValue(rw.model), val(rw))
	}
}

// writeGauge emits a per-model gauge (a point-in-time estimate, unlike a
// monotonic counter) — used for the latency percentiles.
func writeGauge(w io.Writer, name, help string, rows []rowT, val func(rowT) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, rw := range rows {
		fmt.Fprintf(w, "%s{model=\"%s\"} %g\n", name, util.PromLabelValue(rw.model), val(rw))
	}
}

func writeCounterI(w io.Writer, name, help string, rows []rowT, val func(rowT) int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, rw := range rows {
		fmt.Fprintf(w, "%s{model=\"%s\"} %d\n", name, util.PromLabelValue(rw.model), val(rw))
	}
}
