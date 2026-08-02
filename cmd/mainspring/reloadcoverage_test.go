package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/config"
)

// reloadAppliedLive names the top-level config fields that a SIGHUP/admin reload
// genuinely re-applies. Everything else must be reported as requiring a restart.
//
// The value is why, so that adding a field here is a claim someone can check
// rather than a way to silence the test below.
var reloadAppliedLive = map[string]string{
	"models":   "scheduler.Reload re-applies model specs and evicts what changed",
	"aliases":  "scheduler.Reload revalidates and swaps the alias table",
	"tenants":  "Authenticator.Reload swaps the tenant set",
	"api_keys": "folded into tenants by authTenants, then Authenticator.Reload",
}

// Every top-level config field must be either applied on reload or reported as
// needing a restart. Silence is the one outcome that is never acceptable.
//
// #130 found that reloadConfig re-applied only model specs, aliases and tenants
// while every other Phase 8–15 setting was silently ignored — and the docs
// promised that "unsafe changes are logged", which only addr and backend
// actually got. warnUnappliedChanges closed that, but nothing stopped it
// reopening: the existing tests name five fields by hand, so a field added later
// is silently unhandled again, exactly as before.
//
// This walks config.Config by reflection instead, so a new field fails here
// until someone decides which side it belongs on.
func TestEveryConfigFieldIsAppliedOrReportedOnReload(t *testing.T) {
	startup := config.Config{}
	changed := config.Config{}
	cv := reflect.ValueOf(&changed).Elem()
	ct := cv.Type()

	// Give every scalar field a value distinct from the zero baseline, so a
	// missing warnRestartRequired call shows up as silence.
	var scalars []string
	for i := 0; i < ct.NumField(); i++ {
		name := yamlName(ct.Field(i))
		if name == "" || name == "-" {
			continue
		}
		f := cv.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("changed-" + name)
		case reflect.Int, reflect.Int64:
			f.SetInt(4242)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Float64:
			f.SetFloat(42.5)
		default:
			// Slices and maps are the composite ones; they belong to the
			// applied-live set and are checked for membership below.
			if _, ok := reloadAppliedLive[name]; !ok {
				t.Errorf("field %q (%s) is neither a scalar this test can vary nor a "+
					"documented applied-live field — decide which and say so in reloadAppliedLive",
					name, f.Kind())
			}
			continue
		}
		scalars = append(scalars, name)
	}

	if len(scalars) < 25 {
		t.Fatalf("only found %d scalar fields; reflection is probably not seeing the struct", len(scalars))
	}

	out := captureStderr(t, func() { warnUnappliedChanges(startup, changed) })

	for _, name := range scalars {
		if why, live := reloadAppliedLive[name]; live {
			t.Logf("%s: applied live (%s)", name, why)
			continue
		}
		if !strings.Contains(out, name+" changed") {
			t.Errorf("config field %q changes silently on reload: it is neither in "+
				"reloadAppliedLive nor reported by warnUnappliedChanges.\n"+
				"Add a warnRestartRequired(%q, …) call, or document it as applied live.",
				name, name)
		}
	}
}

// A stale entry in reloadAppliedLive would silently excuse a field that no
// longer exists, and quietly weaken the test above after a rename.
func TestReloadAppliedLiveNamesRealFields(t *testing.T) {
	known := map[string]bool{}
	ct := reflect.TypeOf(config.Config{})
	for i := 0; i < ct.NumField(); i++ {
		if n := yamlName(ct.Field(i)); n != "" {
			known[n] = true
		}
	}
	for name := range reloadAppliedLive {
		if !known[name] {
			t.Errorf("reloadAppliedLive names %q, which is not a config.Config field", name)
		}
	}
}

// yamlName returns a struct field's yaml key, minus any options.
func yamlName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}
