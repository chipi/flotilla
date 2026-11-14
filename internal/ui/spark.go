package ui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// spark is a 14-day sparkline drawn by the server (mockup v3's ScoreCard): the line, the area under
// it, and the dashed average of the first seven days, in a 120×34 box.
type spark struct {
	Line, Area string
	AvgY       string
	Label      string
}

func makeSpark(vals []float64) spark {
	if len(vals) < 2 {
		vals = append(vals, vals...)
		if len(vals) < 2 {
			vals = []float64{0, 0}
		}
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	span := hi - lo
	if span == 0 {
		span = 1
	}
	y := func(v float64) float64 { return 30 - (v-lo)*26/span }
	pts := make([]string, len(vals))
	for i, v := range vals {
		pts[i] = fmt.Sprintf("%.1f,%.1f", float64(i)*120/float64(len(vals)-1), y(v))
	}
	line := strings.Join(pts, " ")
	n := min(7, len(vals))
	avg := 0.0
	for _, v := range vals[:n] {
		avg += v
	}
	avg /= float64(n)
	return spark{Line: line, Area: "0,34 " + line + " 120,34", AvgY: fmt.Sprintf("%.1f", y(avg)),
		Label: "Last 14 days; dashed line is the previous 7-day average"}
}

// scoreCard is mockup v3's ScoreCard.
type scoreCard struct {
	Label, Value, Unit, Delta string
	Tone                      string // good | bad | neutral
	Arrow                     string // ▲ ▼ ■
	Spark                     spark
}

// days are the last n days' dates (UTC), oldest first, ending today.
func days(now time.Time, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = now.AddDate(0, 0, i-n+1).UTC().Format("2006-01-02")
	}
	return out
}

// countCard is a card whose value is today's count (or the last value), compared with the average of
// the seven days before it. up says whether more is better (true), worse (false), or neither (nil).
func countCard(label, unit string, series []float64, value string, up *bool) scoreCard {
	last := series[len(series)-1]
	prev := series[max(0, len(series)-8) : len(series)-1]
	avg := 0.0
	for _, v := range prev {
		avg += v
	}
	if len(prev) > 0 {
		avg /= float64(len(prev))
	}
	c := scoreCard{Label: label, Unit: unit, Value: value, Spark: makeSpark(series), Tone: "neutral", Arrow: "■"}
	if c.Value == "" {
		c.Value = trimNum(last)
	}
	d := last - avg
	switch {
	case math.Abs(d) < 0.05:
		c.Delta = "= previous 7-day average"
	case avg == 0:
		c.Delta = fmt.Sprintf("%+g vs previous 7-day average", math.Round(d*10)/10)
	default:
		c.Delta = fmt.Sprintf("%+.0f%% vs previous 7-day average", d/avg*100)
	}
	if math.Abs(d) >= 0.05 {
		c.Arrow = "▲"
		if d < 0 {
			c.Arrow = "▼"
		}
		if up != nil {
			if (d > 0) == *up {
				c.Tone = "good"
			} else {
				c.Tone = "bad"
			}
		}
	}
	return c
}

func trimNum(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

var (
	moreIsBetter = func() *bool { b := true; return &b }()
	lessIsBetter = func() *bool { b := false; return &b }()
)
