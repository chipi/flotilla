package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The run's swimlane, drawn server-side as one SVG: a lane for the harness, one for each role in the run's
// calls (by first appearance), one for the gates; then a row for each tool the agents called. An episode is
// a box from its first to its last call with a tick per turn; a tool call is a mark in its tool's row; an
// arrow hands the work from one episode to the next. The CSP allows no inline style, so every colour is a class.

const (
	swimW      = 1100.0
	swimX0     = 150.0
	swimX1     = 1080.0
	swimTop    = 26.0
	swimLaneH  = 52.0
	swimGateH  = 40.0
	swimToolH  = 22.0
	maxToolRow = 12
)

type swimLane struct {
	Y, H   float64
	Label  string
	Who    string // code | agent | tool: the accent bar's colour
	Tool   bool
	Text   float64 // the label's baseline
	AY, AH float64 // the accent bar
}

type swimEp struct {
	X, Y, W, H     float64
	TickY1, TickY2 float64
	Ticks          []float64
	Label          string
	LX, LY         float64
	LAnchor        string
	Title          string
	Href           string
	Sel, Cut       bool
}

type swimMark struct {
	X, Y  float64
	Class string
	Title string
}

type swimArrow struct{ X1, Y1, X2, Y2 float64 }

type swimText struct {
	X, Y   float64
	Text   string
	Class  string
	Anchor string
}

type swimStepMark struct {
	X, Y float64
	Bad  bool
	Text swimText
}

type swimGrid struct {
	X     float64
	Label string
}

type swimBox struct {
	X, Y, W, H float64
}

type swim struct {
	W, H                  float64
	Lanes                 []swimLane
	Eps                   []swimEp
	Calls                 []swimMark
	Arrows                []swimArrow
	Steps                 []swimStepMark
	Cuts                  []swimText
	Grid                  []swimGrid
	Harness               *swimBox
	GridY1, GridY2, TimeY float64
	GroupY                float64
	Tools                 bool
	Aria                  string
}

var gridSteps = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
	15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// gridFor picks the finest step that gives at most 8 lines over the span.
func gridFor(span time.Duration) time.Duration {
	for _, s := range gridSteps {
		if span/s <= 8 {
			return s
		}
	}
	return gridSteps[len(gridSteps)-1]
}

func cutText(e flowEp) string {
	last := e.Calls[len(e.Calls)-1]
	switch {
	case last.Error != "":
		return "ended in an error"
	case last.FinishReason == "length":
		return "cut at the token cap"
	case last.FinishReason == "tool_calls":
		return "cut off: its last turn was still calling tools"
	}
	return "cut off: its last turn did not end with a stop"
}

// buildSwim lays the flow out. sel is the opened episode's number (0 for none); href(n) its link.
func buildSwim(f *runFlow, sel int, loc *time.Location, href func(int) string) swim {
	s := swim{W: swimW}
	span := f.End.Sub(f.Start)
	if span < time.Second {
		span = time.Second
	}
	plot := (swimX1 - swimX0) * 0.94
	x := func(t time.Time) float64 { return swimX0 + float64(t.Sub(f.Start))/float64(span)*plot }

	idx := map[string]int{}
	y := swimTop
	addLane := func(key, label, who string, h float64) {
		if _, ok := idx[key]; ok {
			return
		}
		idx[key] = len(s.Lanes)
		s.Lanes = append(s.Lanes, swimLane{Y: y, H: h, Label: shortText(label, 18), Who: who, Text: y + h/2 + 4, AY: y + 4, AH: h - 8})
		y += h
	}
	addLane("harness", "the harness", "code", swimLaneH)
	for _, r := range f.Roles {
		addLane(r, r, "agent", swimLaneH)
	}
	for _, st := range f.Steps {
		if st.Role == "gates" {
			addLane("gates", "gates", "code", swimGateH)
			break
		}
	}
	laneOf := func(role string) *swimLane {
		if i, ok := idx[role]; ok {
			return &s.Lanes[i]
		}
		return &s.Lanes[0]
	}
	gatesAt, hasGates := idx["gates"]
	var gatesY, gatesH float64
	if hasGates {
		gatesY, gatesH = s.Lanes[gatesAt].Y, s.Lanes[gatesAt].H
	}
	lanesBottom := y

	// tool rows, the most used first
	count := map[string]int{}
	for _, e := range f.Eps {
		for _, t := range e.Tools {
			count[t.Name]++
		}
	}
	names := make([]string, 0, len(count))
	for n := range count {
		names = append(names, n)
	}
	sort.Slice(names, func(a, b int) bool {
		if count[names[a]] != count[names[b]] {
			return count[names[a]] > count[names[b]]
		}
		return names[a] < names[b]
	})
	row := map[string]int{}
	rows := names
	if len(rows) > maxToolRow {
		rows = append(append([]string(nil), names[:maxToolRow-1]...), "other")
	}
	for i, n := range rows {
		row[n] = i
	}
	toolTop := lanesBottom + 16
	if len(rows) > 0 {
		s.Tools = true
		s.GroupY = lanesBottom + 11
		for i, n := range rows {
			ty := toolTop + float64(i)*swimToolH
			s.Lanes = append(s.Lanes, swimLane{Y: ty, H: swimToolH, Label: shortText(n, 14), Who: "tool", Tool: true, Text: ty + 15, AY: ty + 4, AH: swimToolH - 8})
		}
		y = toolTop + float64(len(rows))*swimToolH
	}
	s.GridY1, s.GridY2, s.TimeY, s.H = swimTop, y, y+15, y+22

	// time grid
	step := gridFor(span)
	for t := f.Start.Truncate(step).Add(step); t.Before(f.End.Add(step/4)) && len(s.Grid) < 12; t = t.Add(step) {
		lbl := t.In(loc).Format("15:04")
		if step < time.Minute {
			lbl = t.In(loc).Format("15:04:05")
		}
		s.Grid = append(s.Grid, swimGrid{X: x(t), Label: lbl})
	}

	// episodes, ticks, tool marks
	for _, e := range f.Eps {
		l := laneOf(e.Role)
		ex := x(e.Start)
		ew := x(e.End) - ex
		if ew < 6 {
			ew = 6
		}
		if ex+ew > swimX1 {
			ex = swimX1 - ew
		}
		se := swimEp{X: ex, Y: l.Y + 6, W: ew, H: 32, Href: href(e.N), Sel: e.N == sel, Cut: e.Cut,
			TickY1: l.Y + 9, TickY2: l.Y + 35, LX: ex + 2, LY: l.Y + 50}
		se.Label = plural(len(e.Calls), "turn", "turns")
		se.LAnchor = "start"
		if ex+110 > swimX1 {
			se.LX, se.LAnchor = ex+ew, "end"
		}
		if e.Cost != nil {
			se.Label += " · " + usd3(*e.Cost)
		}
		se.Title = fmt.Sprintf("%d · %s: %s, %s", e.N, e.Label, plural(len(e.Calls), "turn", "turns"), plural(len(e.Tools), "tool call", "tool calls"))
		for _, c := range e.Calls {
			cx := x(timeOr(c.At, e.Start))
			cx = max(ex+1, min(cx, ex+ew-1))
			se.Ticks = append(se.Ticks, cx)
			for j, t := range parseTools(c.Tools) {
				name := t.Name
				if _, ok := row[name]; !ok {
					name = "other"
				}
				ri, ok := row[name]
				if !ok {
					continue
				}
				cls := "swim__call"
				switch {
				case t.Failed():
					cls += " swim__call--err"
				case t.WritesFile():
					cls += " swim__call--edit"
				}
				title := t.Name
				if t.Arg != "" {
					title += " " + shortText(t.Arg, 60)
				}
				s.Calls = append(s.Calls, swimMark{X: cx - 1.5 + float64(min(j, 6))*1.6, Y: toolTop + float64(ri)*swimToolH + 5, Class: cls, Title: title})
			}
		}
		s.Eps = append(s.Eps, se)
		if e.Cut {
			t := swimText{X: ex + ew + 6, Y: l.Y + 26, Text: cutText(e), Class: "swim__cut", Anchor: "start"}
			if t.X+230 > swimW {
				t.X, t.Anchor = ex-6, "end"
			}
			s.Cuts = append(s.Cuts, t)
		}
	}
	if n := len(s.Eps); n > 0 {
		hl := laneOf("harness")
		first, last := s.Eps[0].X, s.Eps[n-1].X+s.Eps[n-1].W
		for _, e := range s.Eps {
			first, last = min(first, e.X), max(last, e.X+e.W)
		}
		s.Harness = &swimBox{X: first - 6, Y: hl.Y + 14, W: last - first + 12, H: 22}
	}

	// hand-offs: the harness to the first episode, then each episode to the next, by way of a gate when one
	// stood between them
	if len(s.Eps) > 0 {
		hl := laneOf("harness")
		s.Arrows = append(s.Arrows, swimArrow{X1: s.Eps[0].X, Y1: hl.Y + 26, X2: s.Eps[0].X, Y2: s.Eps[0].Y})
	}
	for i := 0; i+1 < len(s.Eps); i++ {
		a, b := s.Eps[i], s.Eps[i+1]
		var gate *flowStep
		for k := range f.Steps {
			st := &f.Steps[k]
			if st.Role == "gates" && !st.At.Before(f.Eps[i].End.Add(-time.Second)) && !st.At.After(f.Eps[i+1].Start.Add(time.Second)) {
				gate = st
				break
			}
		}
		// edge is where an arrow leaves the box above/below it, towards a target at ty
		edge := func(top, h, ty float64) float64 {
			if ty > top {
				return top + h
			}
			return top
		}
		switch {
		case gate != nil && hasGates:
			gx, gy := x(gate.At), gatesY+gatesH/2
			ya, yb := edge(a.Y, a.H, gy), edge(b.Y, b.H, gy)
			s.Arrows = append(s.Arrows, swimArrow{X1: a.X + a.W, Y1: ya, X2: gx, Y2: gy}, swimArrow{X1: gx, Y1: gy, X2: b.X, Y2: yb})
		case a.Y == b.Y:
			s.Arrows = append(s.Arrows, swimArrow{X1: a.X + a.W, Y1: a.Y + a.H/2, X2: b.X, Y2: b.Y + b.H/2})
		default:
			ya, yb := edge(a.Y, a.H, b.Y), edge(b.Y, b.H, a.Y)
			s.Arrows = append(s.Arrows, swimArrow{X1: a.X + a.W, Y1: ya, X2: b.X, Y2: yb})
		}
	}

	// step rows: a mark in the lane of the role that wrote it (the harness's when that role has no lane)
	for _, st := range f.Steps {
		l := laneOf(st.Role)
		cy := l.Y + l.H/2
		if l != &s.Lanes[0] && !(hasGates && l == &s.Lanes[gatesAt]) {
			cy = l.Y + l.H - 8
		}
		sx := x(st.At)
		m := swimStepMark{X: sx, Y: cy, Bad: st.Tone == "bad"}
		txt := st.Detail
		if st.Role != "gates" || st.Tone != "bad" {
			txt = shortText(txt, 44)
		}
		m.Text = swimText{X: sx + 7, Y: cy + 4, Text: txt, Class: "swim__steptext", Anchor: "start"}
		if st.Tone == "bad" {
			m.Text.Class += " swim__steptext--bad"
		}
		if sx > swimX0+0.55*(swimX1-swimX0) {
			m.Text.X, m.Text.Anchor = sx-7, "end"
		}
		s.Steps = append(s.Steps, m)
	}
	roles := strings.Join(f.Roles, ", ")
	s.Aria = "The run, role by role: " + roles + "; then each tool the agents called"
	return s
}
