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

const versionUsage = `versions ROUTINE | promote ROUTINE vN --autonomy L --reason R [--no-pairs] [--gate NAME=pass|fail[@REF]]... [--eval RUN_ID=pass|fail]... | rollback ROUTINE --autonomy L --reason R | retire ROUTINE vN --reason R | rerecord ROUTINE vN --reason R (operator: record the folder as it is now as the live or previous version's hash)`

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
		gate, err := api.ParseGate(g)
		if err != nil {
			return "", 0, req, err
		}
		req.Gates = append(req.Gates, gate)
	}
	for _, e := range flagValues(&args, "--eval") {
		ev, err := api.ParseEval(e)
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
		if len(args) > 0 && (args[0] == "rerecord" || args[0] == "shadow-run") {
			versionCmd(args[0], args[1:], reason)
			return
		}
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
		req.Autonomy = level(req.Autonomy)
		post("/v1/routines/"+routine+"/versions/"+strconv.Itoa(n)+"/promote", req)
	case "rollback":
		autonomy := flagValue(&args, "--autonomy")
		need(args, 1, "rollback ROUTINE --autonomy L --reason R")
		post("/v1/routines/"+args[0]+"/rollback", api.RollbackRequest{Autonomy: level(autonomy), Reason: reason})
	case "retire":
		need(args, 2, "retire ROUTINE vN --reason R")
		n, err := versionNumber(args[1])
		if err != nil {
			fail(err)
		}
		post("/v1/routines/"+args[0]+"/versions/"+strconv.Itoa(n)+"/retire", map[string]string{"reason": reason})
	case "shadow-run":
		input := flagValue(&args, "--input")
		need(args, 2, "versions shadow-run ROUTINE vN --input KEY")
		n, err := versionNumber(args[1])
		if err != nil || input == "" {
			fail(fmt.Errorf("usage: flotilla versions shadow-run ROUTINE vN --input KEY (a live run's id)"))
		}
		post("/v1/routines/"+args[0]+"/versions/"+strconv.Itoa(n)+"/shadow-run", api.ShadowRunRequest{Input: input})
	case "rerecord":
		need(args, 2, "versions rerecord ROUTINE vN --reason R")
		n, err := versionNumber(args[1])
		if err != nil {
			fail(err)
		}
		post("/v1/routines/"+args[0]+"/versions/"+strconv.Itoa(n)+"/rerecord", map[string]string{"reason": reason})
	}
}
