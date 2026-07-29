package server

import "testing"

func TestEstimatePromptTokensCountsEmbeddingsInput(t *testing.T) {
	// A bare-string input: "aaaaaaaaaa aaaaaaaaaa" (21 chars) → ceil(21/4)=6.
	n := estimatePromptTokens([]byte(`{"model":"m1","input":"aaaaaaaaaa aaaaaaaaaa"}`))
	if n != 6 {
		t.Fatalf("string input estimate = %d, want 6", n)
	}

	// An array input: each element counted independently and summed.
	n = estimatePromptTokens([]byte(`{"model":"m1","input":["aaaa","bbbb"]}`)) // 4+4 chars → 1+1=2
	if n != 2 {
		t.Fatalf("array input estimate = %d, want 2", n)
	}

	// No input field at all → 0 (unchanged behavior for chat requests without it).
	if n := estimatePromptTokens([]byte(`{"model":"m1","messages":[]}`)); n != 0 {
		t.Fatalf("no-input estimate = %d, want 0", n)
	}
}

func TestInputStrings(t *testing.T) {
	if got := inputStrings([]byte(`"hello"`)); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("bare string: got %v", got)
	}
	if got := inputStrings([]byte(`["a","b","c"]`)); len(got) != 3 {
		t.Fatalf("array: got %v", got)
	}
	if got := inputStrings(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	// Pre-tokenized integer array: unsupported shape, yields nil (not an error).
	if got := inputStrings([]byte(`[1,2,3]`)); got != nil {
		t.Fatalf("int array: got %v, want nil", got)
	}
}
