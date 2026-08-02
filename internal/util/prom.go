package util

import "strings"

// promLabelEscaper implements the only three escapes the Prometheus text
// exposition format defines for a label value: `\` → `\\`, `"` → `\"`, and a
// line feed → `\n`.
var promLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// PromLabelValue escapes a string for use inside a quoted Prometheus label
// value, e.g. `{model="` + PromLabelValue(id) + `"}`.
//
// Do NOT use Go's %q for this. %q is Go string-literal quoting, which coincides
// with Prometheus only for `\`, `"` and newline: for a tab, a carriage return,
// or invalid UTF-8 it emits `\t`, `\r` or `\xNN`, and the reference Prometheus
// text parser rejects any escape it does not define — failing the whole scrape,
// not just the one sample. Using %q *around* this function is worse still: it
// double-escapes, so a model id containing a quote or a backslash silently
// yields a corrupt label.
func PromLabelValue(s string) string { return promLabelEscaper.Replace(s) }
