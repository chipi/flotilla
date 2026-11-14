package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// A run's trace (mockup v3's Trace page, G2): its spans as VictoriaTraces holds them, read through its
// Jaeger query API at the address beside the OTLP one flotillad already sends to. Spans come from
// flotillad (the run) and from the routine's own telemetry (runtime/telemetry.py: model calls, tools).

type traceSpan struct {
	ID, Parent, Name, Service string
	Start, Dur                time.Duration
	Attrs                     [][2]string
	Depth                     int
	X, W                      float64 // the bar, in a 0..1000 box
	Took, StartText           string
	Kind                      string // run | flotillad | model | tool | knowledge | harness | other
	Href                      string
	On                        bool
}

type traceView struct {
	Spans    []traceSpan
	Sel      *traceSpan
	Total    string
	Ticks    []string
	Services []string
	Err      string
}

// tracesQueryBase is the VictoriaTraces query address: the OTLP URL up to /insert/.
func (u *Server) tracesQueryBase() string {
	if u.API.Sched == nil {
		return ""
	}
	base, _, ok := strings.Cut(u.API.Sched.OTLPTracesURL, "/insert/")
	if !ok {
		return ""
	}
	return base
}

type jaegerTrace struct {
	Data []struct {
		Spans []struct {
			SpanID        string `json:"spanID"`
			OperationName string `json:"operationName"`
			References    []struct {
				RefType string `json:"refType"`
				SpanID  string `json:"spanID"`
			} `json:"references"`
			StartTime int64 `json:"startTime"` // microseconds
			Duration  int64 `json:"duration"`
			Tags      []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"tags"`
			ProcessID string `json:"processID"`
		} `json:"spans"`
		Processes map[string]struct {
			ServiceName string `json:"serviceName"`
		} `json:"processes"`
	} `json:"data"`
}

func spanKind(name, service string) string {
	switch {
	case strings.HasPrefix(name, "run "):
		return "run"
	case strings.HasPrefix(name, "model."):
		return "model"
	case strings.HasPrefix(name, "tool"):
		return "tool"
	case strings.HasPrefix(name, "knowledge") || strings.HasPrefix(name, "memory"):
		return "knowledge"
	case strings.HasPrefix(name, "context") || strings.HasPrefix(name, "validate") || strings.HasPrefix(name, "harness"):
		return "harness"
	case service == "flotillad":
		return "flotillad"
	}
	return "other"
}

func durText(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// runTrace reads a run's trace; an empty view with Err says why there is none.
func (u *Server) runTrace(ctx context.Context, traceID, runID, sel string) traceView {
	base := u.tracesQueryBase()
	switch {
	case traceID == "" || strings.Trim(traceID, "0") == "":
		return traceView{Err: "This run has no trace id: it ran before tracing, or flotillad sends no traces."}
	case base == "":
		return traceView{Err: "flotillad has no traces address (otlp_traces_url), so it cannot read the trace."}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/select/jaeger/api/traces/"+traceID, nil)
	if err != nil {
		return traceView{Err: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return traceView{Err: "VictoriaTraces did not answer: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return traceView{Err: fmt.Sprintf("VictoriaTraces answered %d for this trace (it keeps traces for a limited time).", resp.StatusCode)}
	}
	var jt jaegerTrace
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&jt); err != nil || len(jt.Data) == 0 || len(jt.Data[0].Spans) == 0 {
		return traceView{Err: "VictoriaTraces has no spans for this trace (it keeps traces for a limited time)."}
	}
	t := jt.Data[0]
	var spans []traceSpan
	var t0, t1 int64
	services := map[string]bool{}
	for i, s := range t.Spans {
		sp := traceSpan{ID: s.SpanID, Name: s.OperationName, Service: t.Processes[s.ProcessID].ServiceName,
			Start: time.Duration(s.StartTime) * time.Microsecond, Dur: time.Duration(s.Duration) * time.Microsecond}
		for _, r := range s.References {
			if r.RefType == "CHILD_OF" {
				sp.Parent = r.SpanID
			}
		}
		for _, tg := range s.Tags {
			sp.Attrs = append(sp.Attrs, [2]string{tg.Key, fmt.Sprint(tg.Value)})
		}
		sort.Slice(sp.Attrs, func(a, b int) bool { return sp.Attrs[a][0] < sp.Attrs[b][0] })
		sp.Kind = spanKind(sp.Name, sp.Service)
		services[sp.Service] = true
		if i == 0 || s.StartTime < t0 {
			t0 = s.StartTime
		}
		if e := s.StartTime + s.Duration; e > t1 {
			t1 = e
		}
		spans = append(spans, sp)
	}
	byID := map[string]int{}
	for i, s := range spans {
		byID[s.ID] = i
	}
	depth := func(i int) int {
		d := 0
		for p := spans[i].Parent; p != "" && d < 20; d++ {
			j, ok := byID[p]
			if !ok {
				break
			}
			p = spans[j].Parent
		}
		return d
	}
	total := float64(t1 - t0)
	if total <= 0 {
		total = 1
	}
	v := traceView{Total: durText(time.Duration(t1-t0) * time.Microsecond), Services: sortStrings(services)}
	for i := range spans {
		s := &spans[i]
		s.Depth = depth(i)
		rel := float64(s.Start.Microseconds() - t0)
		s.X = rel / total * 1000
		s.W = float64(s.Dur.Microseconds()) / total * 1000
		if s.W < 4 {
			s.W = 4
		}
		s.Took, s.StartText = durText(s.Dur), durText(time.Duration(rel)*time.Microsecond)
		s.Href = "/runs/" + runID + "?span=" + s.ID
	}
	// Parents before their children, then by start.
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].Start < spans[b].Start })
	var ordered []traceSpan
	var walk func(parent string)
	seen := map[string]bool{}
	walk = func(parent string) {
		for _, s := range spans {
			if s.Parent == parent && !seen[s.ID] {
				seen[s.ID] = true
				ordered = append(ordered, s)
				walk(s.ID)
			}
		}
	}
	walk("")
	for _, s := range spans { // spans whose parent is not in this trace
		if !seen[s.ID] {
			seen[s.ID] = true
			ordered = append(ordered, s)
			walk(s.ID)
		}
	}
	for i := range ordered {
		if ordered[i].ID == sel || (sel == "" && i == 0) {
			ordered[i].On = true
			x := ordered[i]
			v.Sel = &x
		}
	}
	v.Spans = ordered
	for i := 0; i <= 3; i++ {
		v.Ticks = append(v.Ticks, durText(time.Duration(total*float64(i)/3)*time.Microsecond))
	}
	return v
}
