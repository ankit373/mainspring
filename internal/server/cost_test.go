package server

import (
	"math"
	"testing"
)

func TestCostFor(t *testing.T) {
	s := &Server{}
	s.SetCostRates(map[string]CostRate{
		"gpt-x": NewCostRate(3.0, 15.0), // $3 / $15 per Mtok
	})

	// 1000 prompt + 500 completion tokens.
	got := s.costFor("gpt-x", 1000, 500, true)
	want := 1000.0/1e6*3.0 + 500.0/1e6*15.0 // 0.003 + 0.0075 = 0.0105
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}

	// Estimate-only usage is never priced.
	if c := s.costFor("gpt-x", 1000, 500, false); c != 0 {
		t.Fatalf("estimate-only cost = %v, want 0", c)
	}
	// Model without a rate costs 0.
	if c := s.costFor("local", 1000, 500, true); c != 0 {
		t.Fatalf("unpriced model cost = %v, want 0", c)
	}
	// No rates configured at all → 0, no panic.
	var bare Server
	if c := bare.costFor("gpt-x", 10, 10, true); c != 0 {
		t.Fatalf("no-rates cost = %v, want 0", c)
	}
}
