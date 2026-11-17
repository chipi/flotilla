package ui

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

// funcs are the templates' helpers. None returns template.HTML: everything from runs, events and the
// ledger is untrusted text (RFC-0001 §8) and is escaped where it is printed.
var funcs = template.FuncMap{
	// chip is a routine or ledger value's voice (DESIGN.md: a status voice, always with its word).
	"chip": func(status string) string {
		switch status {
		case "ok", "file", "auto", "verified":
			return "chip chip--ok"
		case "running", "skipped", "cancelled", "propose", "escalate", "waiting", "defer", "awaiting_approval", "executing", "checking":
			return "chip chip--wait"
		case "error", "timeout", "interrupted", "failed", "escalated", "refuse":
			return "chip chip--stop"
		}
		return "chip chip--neutral"
	},
	// when is a time to the minute, in UTC (Users, where exact dates matter).
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
		if t, ok := parseTime(s); ok {
			return t.UTC().Format("2 Jan 2006 15:04") + "Z"
		}
		return s
	},
	// short cuts text to n runes, with an ellipsis.
	"short": func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
	"search": func(t tableQ, id, label, placeholder string) searchBox {
		return searchBox{T: t, ID: id, Label: label, Placeholder: placeholder}
	},
	"gap":   func(id, text string) gapNote { return gapNote{ID: id, Text: text} },
	"pivot": pivotHref,
	// level is an autonomy level's chip class.
	"level": func(l string) string { return "level level--" + l },
	// bar is the width, out of 100, of a progress bar drawn as SVG (the CSP allows no inline style).
	"pct": func(n, of int) int {
		if of <= 0 {
			return 0
		}
		return min(100, n*100/of)
	},
	"add":        func(a, b int) int { return a + b },
	"f1":         func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"whyWaiting": whyWaiting,
	"plural":     plural,
	"hash":       shortHash,
	"kindWords":  func(k string) string { return strings.ReplaceAll(k, "_", " ") },
	"deref":      func(p *int64) int64 { return *p },
	"toolNames":  toolNames,
	"usd":        func(p *float64) string { return fmt.Sprintf("$%.4f", *p) },
	"secsp":      func(p *float64) string { return secsText(*p) },
	"decisionCtx": func(v any, csrf string, canAct bool, back string) decisionView {
		var d decision
		switch x := v.(type) {
		case decision:
			d = x
		case *decision:
			d = *x
		}
		return decisionView{D: d, CSRF: csrf, CanAct: canAct, Back: back}
	},
}

type searchBox struct {
	T                      tableQ
	ID, Label, Placeholder string
}

type gapNote struct{ ID, Text string }

// secsText writes a run's duration: "0.8 s", "48 s", "2 m 11 s".
func secsText(v float64) string {
	switch {
	case v < 10:
		return fmt.Sprintf("%.1f s", v)
	case v < 60:
		return fmt.Sprintf("%.0f s", v)
	}
	d := time.Duration(v * float64(time.Second)).Round(time.Second)
	m := int(d.Minutes())
	if m >= 60 {
		return fmt.Sprintf("%d h %02d m", m/60, m%60)
	}
	return fmt.Sprintf("%d m %02d s", m, int(d.Seconds())%60)
}

// decisionView is what the decision template needs besides the box: the form's token and who may answer.
type decisionView struct {
	D      decision
	CSRF   string
	CanAct bool
	Back   string
}
