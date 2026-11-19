package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/chipi/flotilla/internal/input"
	"github.com/chipi/flotilla/internal/store"
)

const pairsUsage = `pairs ROUTINE vN [--since T] [--until T] [--show all|differ|unpaired] [--json] | pairs export ROUTINE vN --to DIR [--since T] [--until T]  (T is RFC 3339)`

// pairsCmd is `pairs`: what a shadow version and the live version each decided on the same inputs, per subject
// (docs/wip/v2-design.md section 4). export writes the JSON to a folder outside any repository, for the installation's comparisons and evals.
func pairsCmd(args []string) {
	since, until, show, to := flagValue(&args, "--since"), flagValue(&args, "--until"), flagValue(&args, "--show"), flagValue(&args, "--to")
	asJSON := flagBool(&args, "--json")
	export := len(args) > 0 && args[0] == "export"
	if export {
		args = args[1:]
	}
	need(args, 2, pairsUsage)
	n, err := versionNumber(args[1])
	if err != nil {
		fail(err)
	}
	q := url.Values{"version": {fmt.Sprint(n)}}
	for k, v := range map[string]string{"since": since, "until": until, "show": show} {
		if v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/routines/" + args[0] + "/pairs?" + q.Encode()
	if export {
		if to == "" {
			fail(fmt.Errorf("usage: flotilla %s", pairsUsage))
		}
		if _, err := input.CheckFolder(to); err != nil {
			fail(err)
		}
		raw := do("GET", path, nil)
		var pretty json.RawMessage
		b, _ := json.MarshalIndent(json.RawMessage(raw), "", "  ")
		pretty = b
		if err := input.WriteJSON(to, "pairs.json", pretty, fmt.Sprintf("the pairs of %s v%d", args[0], n), time.Now()); err != nil {
			fail(err)
		}
		where, _ := filepath.Abs(to)
		fmt.Printf("exported the pairs of %s v%d to %s (production data: keep it outside any repository)\n", args[0], n, where)
		return
	}
	raw := do("GET", path, nil)
	if asJSON {
		os.Stdout.Write(raw)
		return
	}
	var ps store.PairSet
	if err := json.Unmarshal(raw, &ps); err != nil {
		fail(err)
	}
	printPairs(ps)
}

func printPairs(ps store.PairSet) {
	fmt.Printf("%s · v%d shadow vs v%d live: %d pairs · %d differ · %d unpaired\n", ps.Routine, ps.Version, ps.LiveVersion, ps.Paired, ps.Differ, ps.Unpaired)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "MARK\tSUBJECT\tINPUT\tLIVE v%d\tv%d\n", ps.LiveVersion, ps.Version)
	for _, p := range ps.Pairs {
		mark := "differ"
		switch {
		case p.Live == nil || p.Shadow == nil:
			mark = "unpaired"
		case p.Same:
			mark = "same"
		}
		text := func(s *store.PairSide) string {
			if s == nil {
				return "-"
			}
			return s.Text
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, p.Subject, p.InputKey, text(p.Live), text(p.Shadow))
	}
	w.Flush()
}
