package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// commitProbe records when the status line was written. That is the observable
// which separates buffering from shovelling: a response cannot gain a header once
// it is committed, so anything that reads the upstream body *before* WriteHeader
// is buffering in order to say something about it.
type commitProbe struct {
	*httptest.ResponseRecorder
	committed bool
}

func (p *commitProbe) WriteHeader(code int) {
	p.committed = true
	p.ResponseRecorder.WriteHeader(code)
}

// probeBody counts the reads of the upstream body that happened before commit.
type probeBody struct {
	src   io.Reader
	probe *commitProbe
	early int
}

func (b *probeBody) Read(p []byte) (int, error) {
	if !b.probe.committed {
		b.early++
	}
	return b.src.Read(p)
}

func (b *probeBody) Close() error { return nil }

// TestCommitResponseBuffersOnlyForN is the common-path guarantee: a request that
// did not ask for several candidates must reach the client exactly as it did
// before this check existed — the upstream body untouched until after the status
// line. The n>1 case is the counterexample that proves the probe can see the
// difference.
func TestCommitResponseBuffersOnlyForN(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hi"}}]}`
	for _, tc := range []struct {
		name      string
		cc        *choiceCheck
		wantEarly bool
	}{
		{"no n, or n<=1: nothing reads the body before commit", nil, false},
		{"n>1: the body is buffered so the count precedes the header", &choiceCheck{want: 3}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &commitProbe{ResponseRecorder: httptest.NewRecorder()}
			pb := &probeBody{src: strings.NewReader(body), probe: p}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       pb,
			}
			if err := (&Server{}).commitResponse(p, resp, tc.cc); err != nil {
				t.Fatalf("commitResponse: %v", err)
			}
			if got := pb.early > 0; got != tc.wantEarly {
				t.Fatalf("reads before commit = %d (any=%v), want any=%v", pb.early, got, tc.wantEarly)
			}
			if p.Body.String() != body {
				t.Fatalf("body must be forwarded byte-for-byte:\n got %q\nwant %q", p.Body.String(), body)
			}
		})
	}
}

func TestNewChoiceCheckGating(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		n    *int
		path string
		want bool
	}{
		{"no n at all", nil, "/v1/chat/completions", false},
		{"n=1 asks for what it gets", n(1), "/v1/chat/completions", false},
		{"n=0 is not a request for several", n(0), "/v1/chat/completions", false},
		{"n=2 on chat", n(2), "/v1/chat/completions", true},
		{"n=2 on legacy completions", n(2), "/v1/completions", true},
		{"embeddings has no n and no choices", n(2), "/v1/embeddings", false},
	} {
		if got := newChoiceCheck(tc.n, tc.path) != nil; got != tc.want {
			t.Errorf("%s: newChoiceCheck non-nil = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCountChoices(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"choices":[{"index":0}]}`, 1},
		{`{"choices":[{"index":0},{"index":1},{"index":2}]}`, 3},
		{`{"choices":[]}`, 0},
		{`{"data":[{"embedding":[0.1]}]}`, -1}, // no choices field: unobservable
		{`not json`, -1},
	} {
		if got := countChoices([]byte(tc.body)); got != tc.want {
			t.Errorf("countChoices(%s) = %d, want %d", tc.body, got, tc.want)
		}
	}
}

func TestShortfallWording(t *testing.T) {
	cc := &choiceCheck{want: 3}
	for _, tc := range []struct {
		got  int
		want string
	}{
		{1, "n=3 requested but the engine returned 1 choice"},
		{2, "n=3 requested but the engine returned 2 choices"},
		{3, ""},  // satisfied
		{4, ""},  // more than asked is not a shortfall
		{0, ""},  // an empty answer is a different failure, already reported
		{-1, ""}, // unobservable
	} {
		if got := cc.shortfall(tc.got); got != tc.want {
			t.Errorf("shortfall(%d) = %q, want %q", tc.got, got, tc.want)
		}
	}
	if got := (*choiceCheck)(nil).shortfall(1); got != "" {
		t.Errorf("nil check must never warn, got %q", got)
	}
}

// TestStreamIndexCountSurvivesArbitrarySplits feeds the same SSE bytes in every
// chunk size, because a 32 KiB read boundary falls wherever it likes and a frame
// split across two reads must still be counted exactly once.
func TestStreamIndexCountSurvivesArbitrarySplits(t *testing.T) {
	frames := `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n" +
		`data: {"choices":[{"index":1,"delta":{"content":"b"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		": a comment the engine sent\n\n" +
		"data: [DONE]\n\n"

	for _, chunk := range []int{1, 2, 3, 7, 13, 64, len(frames)} {
		cc := &choiceCheck{want: 3, seen: map[int]bool{}}
		for i := 0; i < len(frames); i += chunk {
			end := min(i+chunk, len(frames))
			if _, err := cc.Write([]byte(frames[i:end])); err != nil {
				t.Fatalf("chunk=%d: %v", chunk, err)
			}
		}
		if len(cc.seen) != 2 {
			t.Fatalf("chunk=%d: distinct indices = %d (%v), want 2", chunk, len(cc.seen), cc.seen)
		}
		if want := "n=3 requested but the engine returned 2 choices"; cc.shortfall(len(cc.seen)) != want {
			t.Fatalf("chunk=%d: want %q", chunk, want)
		}
	}
}

// TestStreamIndexIgnoresToolCallIndex guards the reason frames are decoded rather
// than scanned: `index` also names a tool_call slot, and counting those would
// invent choices the engine never returned.
func TestStreamIndexIgnoresToolCallIndex(t *testing.T) {
	cc := &choiceCheck{want: 2, seen: map[int]bool{}}
	_, _ = cc.Write([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":7,"id":"c1"}]}}]}` + "\n\n"))
	if len(cc.seen) != 1 || !cc.seen[0] {
		t.Fatalf("tool_call index must not count as a choice: %v", cc.seen)
	}
}

// TestStreamCountGoesBlindOnAnOversizedFrame: an engine emitting a frame larger
// than the cap must cost bounded memory, and an abandoned count must warn about
// nothing rather than report a shortfall it never established.
func TestStreamCountGoesBlindOnAnOversizedFrame(t *testing.T) {
	cc := &choiceCheck{want: 3, seen: map[int]bool{}}
	_, _ = cc.Write([]byte("data: " + strings.Repeat("x", choiceFrameCap+1)))
	if !cc.blind || cc.pend != nil {
		t.Fatalf("oversized frame should abandon the count and release the buffer (blind=%v, pend=%d)", cc.blind, len(cc.pend))
	}
	w := httptest.NewRecorder()
	cc.emitShortfall(w)
	if w.Body.Len() != 0 {
		t.Fatalf("a blind count must not warn: %q", w.Body.String())
	}
}

func TestAddWarningComposes(t *testing.T) {
	h := http.Header{}
	addWarning(h, "GPU offload requested but engine loaded on CPU (silent fallback)")
	addWarning(h, "n=3 requested but the engine returned 1 choice")
	want := "GPU offload requested but engine loaded on CPU (silent fallback); " +
		"n=3 requested but the engine returned 1 choice"
	if got := h.Get(warningHeader); got != want {
		t.Fatalf("warnings must compose, not overwrite:\n got %q\nwant %q", got, want)
	}
	if n := len(h.Values(warningHeader)); n != 1 {
		t.Fatalf("composed warnings belong on one header line, got %d", n)
	}
}
