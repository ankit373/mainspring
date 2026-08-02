package util

import "testing"

func TestPromLabelValueEscapesExactlyTheThreeSpecialChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain-model:7b`, `plain-model:7b`},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{`a"\` + "\n", `a\"\\\n`},
		// A tab is legal, unescaped, inside a quoted label value. %q would emit
		// `\t`, which the Prometheus text parser rejects as an invalid escape.
		{"a\tb", "a\tb"},
		{"café", "café"},
	}
	for _, c := range cases {
		if got := PromLabelValue(c.in); got != c.want {
			t.Errorf("PromLabelValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
