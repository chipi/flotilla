package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Log lines from VictoriaLogs (G5): what a run printed (the worker logs each line as "run output"),
// and flotillad's own lines about it, by run, by routine or by fleet. flotillad.yaml vlogs_url names
// the address; without it the pages say so.

type logLine struct {
	At, Level, Routine, Text string
	Bad                      bool
}

// logsQuery asks VictoriaLogs a LogsQL query and returns at most limit lines, newest first.
func (u *Server) logsQuery(ctx context.Context, query string, limit int) ([]logLine, error) {
	if u.LogsURL == "" {
		return nil, fmt.Errorf("flotillad has no logs address (flotillad.yaml vlogs_url)")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	q := url.Values{"query": {query + fmt.Sprintf(" | sort by (_time desc) | limit %d", limit)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.LogsURL, "/")+"/select/logsql/query",
		strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("VictoriaLogs did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("VictoriaLogs answered %d", resp.StatusCode)
	}
	var out []logLine
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		l := logLine{Level: strings.ToUpper(m["level"]), Routine: m["routine"], Text: m["line"]}
		if l.Text == "" {
			l.Text = m["msg"]
			for _, k := range []string{"status", "detail", "err"} {
				if m[k] != "" {
					l.Text += " " + k + "=" + m[k]
				}
			}
		}
		if t, ok := parseTime(m["_time"]); ok {
			l.At = t.In(u.loc()).Format("Jan 2 15:04:05")
		}
		l.Bad = l.Level == "WARN" || l.Level == "WARNING" || l.Level == "ERROR"
		out = append(out, l)
	}
	return out, sc.Err()
}

// logsText is lines as a log pane: time, level, routine, text.
func logsText(lines []logLine, withRoutine bool) string {
	var b strings.Builder
	for _, l := range lines {
		lvl := l.Level
		if lvl == "" {
			lvl = "-"
		}
		if withRoutine {
			fmt.Fprintf(&b, "%s %-5s %-24s %s\n", l.At, lvl, l.Routine, l.Text)
		} else {
			fmt.Fprintf(&b, "%s %-5s %s\n", l.At, lvl, l.Text)
		}
	}
	return b.String()
}

// logsQuote makes a value safe inside a LogsQL quoted phrase.
func logsQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// levelFilter is the level select's LogsQL: all, warnings and errors, or errors.
func levelFilter(level string) string {
	switch level {
	case "warn":
		return ` level:~"(?i)^(warn|warning|error)$"`
	case "error":
		return ` level:~"(?i)^error$"`
	}
	return ""
}
