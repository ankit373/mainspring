package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Strict decoding exists because the alternative is the failure this whole
// server is built against. A config is decoded into a struct, so a key the
// struct does not know is simply dropped — and Lint() runs on the *parsed*
// struct, by which point the typo is gone. `enfore_context: true` therefore
// started a server with the guardrail off and said nothing, which is the silent
// degradation the README opens by complaining about, reintroduced through the
// config file.
//
// yaml.v3 reports unknown fields for us; what it does not do is say them well.
// "field olama_host not found in type config.Config" names a Go type the
// operator does not have and offers no way forward, so the messages are
// rewritten to name the key and, since a typo is usually a near-miss, the
// closest field that does exist.

// unknownFieldPrefix and unknownFieldMid bracket the key name in yaml.v3's own
// message: `line 4: field olama_host not found in type config.Config`.
const (
	unknownFieldPrefix = "field "
	unknownFieldMid    = " not found in type "
)

// explainDecodeError rewrites a yaml decode error so every unknown-field
// complaint names the key and suggests the nearest real field. Anything that is
// not an unknown-field error (a genuine type mismatch, malformed YAML) is left
// exactly as it was — it already says something true.
func explainDecodeError(err error) error {
	te, ok := err.(*yaml.TypeError)
	if !ok {
		return err
	}
	known := configFields()
	out := make([]string, 0, len(te.Errors))
	for _, msg := range te.Errors {
		out = append(out, explainOne(msg, known))
	}
	return fmt.Errorf("%s", strings.Join(out, "\n  "))
}

// configFields is every yaml key name the Config schema knows, at any depth.
func configFields() []string {
	return knownFields(reflect.TypeOf(Config{}), map[reflect.Type]bool{})
}

// explainOne rewrites a single yaml error line, or returns it unchanged.
func explainOne(msg string, known []string) string {
	where, rest, ok := strings.Cut(msg, unknownFieldPrefix)
	if !ok {
		return msg
	}
	key, _, ok := strings.Cut(rest, unknownFieldMid)
	if !ok {
		return msg
	}
	out := where + "unknown field " + quote(key)
	if near := nearest(key, known); near != "" {
		out += " (did you mean " + quote(near) + "?)"
	}
	return out
}

func quote(s string) string { return "\"" + s + "\"" }

// knownFields collects every yaml key name reachable in t, at any depth. A flat
// set is deliberate: it makes the suggestion for a misspelled *nested* key as
// useful as one for a top-level key, and an operator who is told the right
// spelling can see for themselves where it belongs. seen guards against a type
// that reaches itself.
func knownFields(t reflect.Type, seen map[reflect.Type]bool) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return nil
	}
	seen[t] = true

	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" {
			continue
		}
		if tag == "" {
			// yaml.v3's default for an untagged field is the lowercased name.
			tag = strings.ToLower(f.Name)
		}
		names = append(names, tag)
		names = append(names, knownFields(f.Type, seen)...)
	}
	sort.Strings(names)
	return names
}

// prefixFloor is how much of a shared prefix makes two config keys plausibly the
// same setting. Three characters is enough to link `ctx_size` to `ctx` without
// linking `xy` to everything.
const prefixFloor = 3

// nearest returns the known field closest to key, or "" when nothing is close
// enough to be worth suggesting — a wrong guess costs more than no guess.
//
// Two kinds of mistake get made in a config file, and edit distance only sees
// one. A misspelling (`enfore_context`) is a character or two away. A
// wrong-but-related name (`ctx_size` for `ctx`, `ollama_url` for `ollama_host`)
// can be arbitrarily far by distance while being obvious to a reader, so a
// shared prefix is tried when distance finds nothing.
func nearest(key string, known []string) string {
	// The distance bound scales with length: a short key must not match half the
	// schema, and a long one should still tolerate a transposition or two.
	limit := len(key)/3 + 1
	best, bestDist := "", limit+1
	for _, k := range known {
		if k == key {
			continue // reported at the wrong nesting level, not misspelled
		}
		if d := editDistance(key, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	if bestDist <= limit {
		return best
	}

	// Longest shared prefix wins; among equals, the shorter field, which is the
	// more likely intent (`ctx` over `ctx_something_else`).
	bestShared := 0
	for _, k := range known {
		if k == key {
			continue
		}
		n := sharedPrefix(key, k)
		if n < prefixFloor || (n != len(key) && n != len(k)) {
			continue // one must be a prefix of the other, not merely similar
		}
		if n > bestShared || (n == bestShared && len(k) < len(best)) {
			best, bestShared = k, n
		}
	}
	if bestShared < prefixFloor {
		return ""
	}
	return best
}

// sharedPrefix is the number of leading bytes a and b have in common.
func sharedPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// editDistance is Levenshtein distance over bytes, which is what config keys
// are: ASCII with underscores.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
