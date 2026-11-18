package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

const versionUsage = `versions ROUTINE | promote ROUTINE vN --autonomy L --reason R [--no-pairs] [--gate NAME=pass|fail[@REF]]... [--eval RUN_ID=pass|fail]... | rollback ROUTINE --autonomy L --reason R | retire ROUTINE vN --reason R`

// flagBool removes the bare flag name from args and says whether it was there.
func flagBool(args *[]string, name string) bool {
	a := *args
	for i := range a {
		if a[i] == name {
			*args = append(a[:i], a[i+1:]...)
			return true
		}
	}
	return false
}

// flagValues removes every `name value` from args and returns the values, in order.
func flagValues(args *[]string, name string) []string {
	var out []string
	for v := flagValue(args, name); v != ""; v = flagValue(args, name) {
		out = append(out, v)
	}
	return out
}

// parseGate reads NAME=pass|fail[@REF].
func parseGate(s string) (store.Gate, error) {
	name, rest, ok := strings.Cut(s, "=")
	result, ref, _ := strings.Cut(rest, "@")
	if !ok || name == "" || (result != "pass" && result != "fail") {
		return store.Gate{}, fmt.Errorf("--gate %q: NAME=pass|fail[@REF]", s)
	}
	return store.Gate{Name: name, Result: result, Ref: ref}, nil
}

// parseEval reads RUN_ID=pass|fail.
func parseEval(s string) (store.EvalRun, error) {
	id, verdict, ok := strings.Cut(s, "=")
	if !ok || id == "" || (verdict != "pass" && verdict != "fail") {
		return store.EvalRun{}, fmt.Errorf("--eval %q: RUN_ID=pass|fail", s)
	}
	return store.EvalRun{RunID: id, Verdict: verdict}, nil
}

// versionNumber reads vN (or N).
func versionNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "v"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("the version is vN: v1, v2, ...")
	}
	return n, nil
}

// parsePromote reads `ROUTINE vN` and the promote flags.
func parsePromote(args []string, reason string) (string, int, api.PromoteRequest, error) {
	req := api.PromoteRequest{Reason: reason, Autonomy: flagValue(&args, "--autonomy"), WithoutPairs: flagBool(&args, "--no-pairs")}
	for _, g := range flagValues(&args, "--gate") {
		gate, err := parseGate(g)
		if err != nil {
			return "", 0, req, err
		}
		req.Gates = append(req.Gates, gate)
	}
	for _, e := range flagValues(&args, "--eval") {
		ev, err := parseEval(e)
		if err != nil {
			return "", 0, req, err
		}
		req.Evals = append(req.Evals, ev)
	}
	if len(args) != 2 {
		return "", 0, req, fmt.Errorf("usage: flotilla %s", versionUsage)
	}
	n, err := versionNumber(args[1])
	return args[0], n, req, err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

// versionCmd is `versions`, `promote`, `rollback` and `retire` (docs/wip/v2-design.md section 1): which version
// of a routine is live, and the operator's word that moves it.
func versionCmd(cmd string, args []string, reason string) {
	switch cmd {
	case "versions":
		need(args, 1, "versions ROUTINE")
		var out api.VersionsView
		get("/v1/routines/"+args[0]+"/versions", &out)
		fmt.Printf("%s: live v%d; a promote needs: %s\n", out.Routine, out.Live, strings.Join(out.Acceptance, ", "))
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "VERSION\tSTATE\tHASH\tDEPLOYED\tLIVE\tBY\tWHY")
		for _, v := range out.Versions {
			state := v.State
			if v.RetiredKind == store.RetiredRolledBack {
				state = "retired (rolled back)"
			}
			hash := v.Hash
			if len(hash) > 7 {
				hash = hash[:7]
			}
			fmt.Fprintf(w, "v%d\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Version, state, hash, v.DeployedAt, orDash(v.LiveAt), orDash(v.StateBy), orDash(v.StateWhy))
		}
		w.Flush()
	case "promote":
		routine, n, req, err := parsePromote(args, reason)
		if err != nil {
			fail(err)
		}
		post("/v1/routines/"+routine+"/versions/"+strconv.Itoa(n)+"/promote", req)
	case "rollback":
		autonomy := flagValue(&args, "--autonomy")
		need(args, 1, "rollback ROUTINE --autonomy L --reason R")
		post("/v1/routines/"+args[0]+"/rollback", api.RollbackRequest{Autonomy: autonomy, Reason: reason})
	case "retire":
		need(args, 2, "retire ROUTINE vN --reason R")
		n, err := versionNumber(args[1])
		if err != nil {
			fail(err)
		}
		post("/v1/routines/"+args[0]+"/versions/"+strconv.Itoa(n)+"/retire", map[string]string{"reason": reason})
	}
}
