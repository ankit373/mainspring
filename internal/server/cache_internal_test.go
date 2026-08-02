package server

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCacheableRequiresExplicitTemperatureZero(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicit zero", `{"model":"m","temperature":0}`, true},
		{"explicit zero as float", `{"model":"m","temperature":0.0}`, true},
		{"omitted means sample normally (default 1)", `{"model":"m"}`, false},
		{"null is omitted", `{"model":"m","temperature":null}`, false},
		{"positive", `{"model":"m","temperature":0.7}`, false},
		{"streaming", `{"model":"m","temperature":0,"stream":true}`, false},
		{"unparseable", `{`, false},
	}
	for _, c := range cases {
		if got := cacheable([]byte(c.body)); got != c.want {
			t.Errorf("%s: cacheable(%s) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// TestReplayHeaderDropsRequestScoped is the whitelist's contract: headers that
// describe the stored payload survive a replay; the leader's correlation id,
// tenant quota headroom, per-request facts, and Date do not.
func TestReplayHeaderDropsRequestScoped(t *testing.T) {
	h := http.Header{}
	for k, v := range map[string]string{
		"Content-Type":                       "application/json",
		"X-Mainspring-Backend":               "llamacpp",
		"X-Mainspring-Device":                "cuda",
		"X-Mainspring-Warning":               "GPU offload requested but engine loaded on CPU",
		"X-Request-ID":                       "req_leader",
		"X-Mainspring-RateLimit-Limit":       "90",
		"X-Mainspring-RateLimit-Remaining":   "89",
		"X-Mainspring-RateLimit-Reset":       "60",
		"X-Mainspring-TokenBudget-Limit":     "9000",
		"X-Mainspring-TokenBudget-Remaining": "8994",
		"X-Mainspring-TokenBudget-Reset":     "60",
		"X-Mainspring-Context-Method":        "exact",
		"X-Mainspring-Context-Warning":       "prompt (9014 tokens) exceeds context",
		"X-Mainspring-Clamped":               "9999->4096",
		"X-Mainspring-Retries":               "2",
		"Date":                               "Mon, 01 Jan 2035 00:00:00 GMT",
		"X-Some-Header-Nobody-Thought-About": "leaked",
	} {
		h.Set(k, v)
	}

	want := http.Header{
		"Content-Type":         {"application/json"},
		"X-Mainspring-Backend": {"llamacpp"},
		"X-Mainspring-Device":  {"cuda"},
		"X-Mainspring-Warning": {"GPU offload requested but engine loaded on CPU"},
	}
	if got := replayHeader(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayHeader = %v, want %v", got, want)
	}
}
