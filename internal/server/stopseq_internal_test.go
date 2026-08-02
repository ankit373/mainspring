package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// feedAll drives a watch with a sequence of fragments the way a stream would,
// returning everything emitted (plus the flushed tail when nothing matched).
func feedAll(sw *stopWatch, frags ...string) (string, bool) {
	var out strings.Builder
	for _, f := range frags {
		emit, matched := sw.feed(f)
		out.WriteString(emit)
		if matched {
			return out.String(), true
		}
	}
	out.WriteString(sw.flush())
	return out.String(), false
}

func TestStopWatch(t *testing.T) {
	tests := []struct {
		name     string
		seqs     []string
		frags    []string
		wantText string
		wantHit  string
	}{
		{
			name:     "whole sequence inside one fragment",
			seqs:     []string{"STOP"},
			frags:    []string{"hello STOP world"},
			wantText: "hello ",
			wantHit:  "STOP",
		},
		{
			name: "sequence straddles two fragments",
			seqs: []string{"STOP"},
			// The reason feed is incremental: neither fragment contains the sequence.
			frags:    []string{"hello ST", "OP world"},
			wantText: "hello ",
			wantHit:  "STOP",
		},
		{
			name:     "sequence spread over many fragments",
			seqs:     []string{"<<END>>"},
			frags:    []string{"a", "<", "<", "E", "N", "D", ">", ">", "b"},
			wantText: "a",
			wantHit:  "<<END>>",
		},
		{
			name:     "no match emits everything",
			seqs:     []string{"STOP"},
			frags:    []string{"hello ", "world"},
			wantText: "hello world",
		},
		{
			name: "partial match at end of stream is flushed, not dropped",
			seqs: []string{"STOP"},
			// "ST" is held back as a possible start; the stream then ends. Losing it
			// would silently truncate the answer.
			frags:    []string{"hello ", "ST"},
			wantText: "hello ST",
		},
		{
			name:     "false start resolves and is emitted",
			seqs:     []string{"STOP"},
			frags:    []string{"STAND"},
			wantText: "STAND",
		},
		{
			name:     "earliest match wins over listed order",
			seqs:     []string{"ZZ", "AA"},
			frags:    []string{"1 AA 2 ZZ 3"},
			wantText: "1 ",
			wantHit:  "AA",
		},
		{
			name:     "ties go to the caller's order",
			seqs:     []string{"ab", "abc"},
			frags:    []string{"xxabc"},
			wantText: "xx",
			wantHit:  "ab",
		},
		{
			name:     "empty sequences are ignored, not matched everywhere",
			seqs:     []string{"", "STOP"},
			frags:    []string{"hello world"},
			wantText: "hello world",
		},
		{
			name:     "match at the very start yields empty text",
			seqs:     []string{"STOP"},
			frags:    []string{"STOP now"},
			wantText: "",
			wantHit:  "STOP",
		},
		{
			name:     "multibyte sequence split mid-rune boundary",
			seqs:     []string{"→END"},
			frags:    []string{"go \xe2\x86", "\x92END rest"},
			wantText: "go ",
			wantHit:  "→END",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sw := newStopWatch(tc.seqs)
			text, matched := feedAll(sw, tc.frags...)
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if got := sw.hitSeq(); got != tc.wantHit {
				t.Errorf("hit = %q, want %q", got, tc.wantHit)
			}
			if matched != (tc.wantHit != "") {
				t.Errorf("matched = %v, want %v", matched, tc.wantHit != "")
			}
		})
	}
}

// A nil watch is the no-stop-sequences case and must be transparent, so the
// callers on that path need no branch of their own.
func TestStopWatchNilIsTransparent(t *testing.T) {
	var sw *stopWatch
	if got := newStopWatch(nil); got != nil {
		t.Fatalf("newStopWatch(nil) = %v, want nil", got)
	}
	if got := newStopWatch([]string{"", ""}); got != nil {
		t.Fatalf("newStopWatch(empties) = %v, want nil", got)
	}
	emit, matched := sw.feed("anything")
	if emit != "anything" || matched {
		t.Fatalf("feed = (%q, %v), want (%q, false)", emit, matched, "anything")
	}
	if sw.flush() != "" || sw.hitSeq() != "" {
		t.Fatal("nil watch should flush nothing and report no hit")
	}
}

// Once a sequence matches, nothing further escapes — the generation is over as
// far as the caller is concerned, even if more fragments are still in flight.
func TestStopWatchIsTerminal(t *testing.T) {
	sw := newStopWatch([]string{"STOP"})
	if emit, matched := sw.feed("a STOP b"); emit != "a " || !matched {
		t.Fatalf("first feed = (%q, %v)", emit, matched)
	}
	if emit, matched := sw.feed("more text"); emit != "" || !matched {
		t.Fatalf("post-match feed = (%q, %v), want (\"\", true)", emit, matched)
	}
	if sw.flush() != "" {
		t.Fatal("flush after a match should yield nothing")
	}
}

func TestStopReasonFor(t *testing.T) {
	tests := []struct{ finish, hit, want string }{
		{"stop", "", "end_turn"},
		{"", "", "end_turn"},
		{"length", "", "max_tokens"},
		{"tool_calls", "", "tool_use"},
		// A hit wins over whatever the upstream said: finish_reason has no way to
		// express it, which is the whole reason stopWatch exists.
		{"stop", "END", "stop_sequence"},
		{"length", "END", "stop_sequence"},
	}
	for _, tc := range tests {
		if got := stopReasonFor(tc.finish, tc.hit); got != tc.want {
			t.Errorf("stopReasonFor(%q, %q) = %q, want %q", tc.finish, tc.hit, got, tc.want)
		}
	}
	if stopSequenceField("") != nil {
		t.Error("stop_sequence must be null when nothing matched")
	}
	if stopSequenceField("END") != "END" {
		t.Error("stop_sequence must name the sequence that matched")
	}
}

// ownedStopBody is what keeps the engine from stopping-and-erasing the match
// before Mainspring can see it.
func TestOwnedStopBody(t *testing.T) {
	in := []byte(`{"model":"m1","stop":["END"],"stream":false,"max_tokens":64}`)

	for _, promote := range []bool{false, true} {
		var m map[string]any
		if err := json.Unmarshal(ownedStopBody(in, promote), &m); err != nil {
			t.Fatalf("promote=%v: %v", promote, err)
		}
		if _, ok := m["stop"]; ok {
			t.Errorf("promote=%v: stop must never reach the engine", promote)
		}
		if m["model"] != "m1" || m["max_tokens"] != float64(64) {
			t.Errorf("promote=%v: unrelated fields altered: %v", promote, m)
		}
		if m["stream"] != promote {
			t.Errorf("promote=%v: stream = %v", promote, m["stream"])
		}
		_, hasOpts := m["stream_options"]
		if hasOpts != promote {
			t.Errorf("promote=%v: stream_options present = %v", promote, hasOpts)
		}
	}

	// A body that will not parse is the upstream's to reject, not ours to mangle.
	bad := []byte(`{not json`)
	if got := string(ownedStopBody(bad, true)); got != string(bad) {
		t.Errorf("unparseable body was rewritten to %q", got)
	}
}
