package ui

import (
	"fmt"
	"html/template"
	"time"
)

// funcs are the templates' helpers. None returns template.HTML: everything from runs, events and the
// ledger is untrusted text (RFC-0001 §8) and is escaped where it is printed.
var funcs = template.FuncMap{
	// chip is the status chip's class (DESIGN.md: a status voice, always with its word).
	"chip": func(status string) string {
		switch status {
		case "ok", "file", "auto", "verified":
			return "chip chip--ok"
		case "running", "skipped", "propose", "escalate", "waiting":
			return "chip chip--wait"
		case "error", "timeout", "interrupted", "failed":
			return "chip chip--stop"
		}
		return "chip chip--neutral"
	},
	// when shortens an RFC 3339 time to the minute, in UTC.
	"when": func(v any) string {
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case *string:
			if x == nil {
				return "–"
			}
			s = *x
		default:
			return "–"
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC().Format("Jan 2 15:04") + "Z"
			}
		}
		if len(s) > 16 {
			return s[:16]
		}
		return s
	},
	"secs": func(v *float64) string {
		if v == nil {
			return "–"
		}
		if *v < 60 {
			return fmt.Sprintf("%.1f s", *v)
		}
		return fmt.Sprintf("%.0f min", *v/60)
	},
	"usd": func(v float64) string { return fmt.Sprintf("$%.4f", v) },
	// short cuts text to n runes, with an ellipsis.
	"short": func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
	"tail": func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return "…" + string(r[len(r)-n:])
	},
}
