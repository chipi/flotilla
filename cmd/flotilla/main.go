// flotilla — the command line for flotillad's API.
//
//	flotilla status
//	flotilla runs [fleet/routine] [-n 20]
//	flotilla events [-n 50]
//	flotilla run fleet/routine
//	flotilla pause fleet/routine                        (the run in flight finishes; only stop and cancel end it)
//	flotilla resume fleet/routine --reason "..."        (operator; loosening needs a reason)
//	flotilla autonomy fleet/routine record|propose|auto [--reason "..."]   (operator; raising needs one)
//	flotilla versions fleet/routine                     (its versions: state, hash, who moved them and why)
//	flotilla promote fleet/routine vN --autonomy L --reason "..." [--no-pairs] [--gate NAME=pass|fail[@REF]]... [--eval RUN_ID=pass|fail]...   (operator)
//	flotilla rollback fleet/routine --autonomy L --reason "..."   (operator: the previous version is live at once)
//	flotilla retire fleet/routine vN --reason "..."     (operator: stops a shadow)
//	flotilla versions rerecord fleet/routine vN --reason "..."   (operator: the folder as it is now becomes the live or previous version's recorded hash)
//	flotilla stop fleet                                 (also cancels the fleet's runs in flight)
//	flotilla cancel RUN_ID --reason "..."               (operator: one run in flight)
//	flotilla start fleet --reason "..."                 (operator)
//	flotilla case approve ID [--plan HASH] | reject ID --reason "..." (operator: a proposed plan)
//	flotilla case answers ID                            (operator: what the case asks now, and the answers it offers)
//	flotilla case answer ID ANSWER [--plan HASH] [--reason "..."]  (operator: approve, reject, dismiss, close, reassign or acknowledge)
//	flotilla case close ID --reason "..."               (operator: an escalated case; releases its target)
//	flotilla case reassign ID --reason "..."            (operator: an escalated case back to its routine)
//	flotilla user add NAME --role admin|operator|viewer (admin; the first admin with the host's operator token)
//	flotilla user list | role NAME ROLE | rotate NAME | revoke NAME   (admin)
//	flotilla eval record --routine R --prompt-sha S --model M --metric NAME=VALUE [--n N] [--repeats N] [--source TEXT]   (operator)
//	flotilla eval list [--routine R]                     (what evals measured for a routine's prompt on a model)
//	flotilla hash-token < token                        (the line for flotillad's callers file)
//	flotilla host list | enrolment NAME | revoke NAME     (operator: host agents; the token is shown once)
//	flotilla host audit NAME --log FILE                (the host's audit log against flotillad's record)
//	flotilla ca root DIR                               (offline: the host agents' root CA, kept by the operator)
//	flotilla ca intermediate ROOTDIR OUTDIR            (offline: the intermediate for flotillad's secrets volume)
//	flotilla ca signing-key DIR ID                     (offline: the execution-signing key; prints the hosts' line)
//
// FLOTILLA_URL (default http://127.0.0.1:8484) says where; FLOTILLA_TOKEN, or FLOTILLA_TOKEN_FILE, who.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/hostaudit"
	"github.com/chipi/flotilla/internal/spec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "health" { // the container healthcheck: flotillad answers on its API, no token needed
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base() + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	if cmd == "hash-token" {
		tok, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println(api.HashToken(strings.TrimSpace(tok)))
		return
	}
	if cmd == "ca" { // offline, on the operator's machine: no flotillad involved
		if err := caCmd(args, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "flotilla ca:", err)
			os.Exit(1)
		}
		return
	}
	n := "20"
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			n = args[i+1]
			args = append(args[:i], args[i+2:]...)
			break
		}
	}
	reason := flagValue(&args, "--reason")
	role := flagValue(&args, "--role")
	// -json: the API's answer as it is, for scripts (deploy/server/mirror.sh)
	for i := 0; i < len(args); i++ {
		if args[i] == "-json" {
			args = append(args[:i], args[i+1:]...)
			switch cmd {
			case "status":
				os.Stdout.Write(do("GET", "/v1/status", nil))
			case "versions":
				need(args, 1, "versions ROUTINE -json")
				os.Stdout.Write(do("GET", "/v1/routines/"+args[0]+"/versions", nil))
			case "runs":
				q := "/v1/runs?limit=" + n
				if len(args) > 0 {
					q += "&routine=" + args[0]
				}
				os.Stdout.Write(do("GET", q, nil))
			default:
				fmt.Fprintln(os.Stderr, "-json works with status, runs and versions")
				os.Exit(2)
			}
			return
		}
	}
	switch cmd {
	case "status":
		var out struct {
			Version string `json:"version"`
			Freeze  struct {
				Frozen bool
				By, At string
			}
			Routines []map[string]any
			Workers  map[string]any
			Examples []map[string]any
		}
		get("/v1/status", &out)
		if out.Freeze.Frozen {
			fmt.Printf("FROZEN since %s by %s: every routine runs at record (AUTONOMY shows what runs; SET, the routine's own)\n", out.Freeze.At, out.Freeze.By)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintf(w, "flotillad %s\nROUTINE\tAUTONOMY\tSET\tEVERY\tLAST OK\tSTALE\tSPEND TODAY\tSTATE\n", out.Version)
		for _, j := range out.Routines {
			state := []string{}
			if j["enabled"] != true {
				state = append(state, "disabled")
			}
			if j["paused"] == true {
				state = append(state, "paused")
			}
			if j["fleet_stopped"] == true {
				state = append(state, "STOPPED")
			}
			if r, _ := j["running"].(string); r != "" {
				state = append(state, "running "+r)
			}
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t$%.4f/$%.2f\t%s\n", j["id"], j["autonomy"], j["autonomy_set"], j["every"], orDash(j["last_success"]),
				j["stale"], j["spend_today_usd"], j["budget_day_usd"], strings.Join(state, ","))
		}
		exampleRows(w, out.Examples)
		w.Flush()
		for name, h := range out.Workers {
			b, _ := json.Marshal(h)
			fmt.Printf("worker %s: %s\n", name, b)
		}
	case "runs":
		q := "/v1/runs?limit=" + n
		if len(args) > 0 {
			q += "&routine=" + args[0]
		}
		var out struct{ Runs []map[string]any }
		get(q, &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "STARTED\tROUTINE\tSTATUS\tAUTONOMY\tSECONDS\tSPEND\tTRIGGER\tDETAIL")
		for _, r := range out.Runs {
			d, _ := r["detail"].(string)
			if len(d) > 80 {
				d = d[len(d)-80:]
			}
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v/%v\t%s\n", r["started_at"], r["routine"], r["status"], r["autonomy"],
				orDash(r["duration_s"]), r["spend_usd"], r["trigger"], r["actor"], strings.ReplaceAll(d, "\n", " "))
		}
		w.Flush()
	case "events":
		var out struct{ Events []map[string]any }
		get("/v1/events?limit="+n, &out)
		for _, e := range out.Events {
			fmt.Printf("%v  %-16v %-14v %-22v %v %v\n", e["at"], e["actor"], e["kind"], e["subject"], e["via"], e["detail"])
		}
	case "run", "pause":
		need(args, 1, cmd+" fleet/routine")
		post("/v1/routines/"+args[0]+"/"+cmd, nil)
	case "resume":
		need(args, 1, `resume fleet/routine --reason "..."`)
		post("/v1/routines/"+args[0]+"/resume", map[string]string{"reason": reason})
	case "autonomy":
		if len(args) == 1 && args[0] == "freeze" {
			post("/v1/freeze", nil)
			return
		}
		if len(args) == 1 && args[0] == "unfreeze" {
			post("/v1/unfreeze", map[string]string{"reason": reason})
			return
		}
		need(args, 2, `autonomy fleet/routine record|propose|auto [--reason "..."] | autonomy freeze | autonomy unfreeze --reason "..."`)
		post("/v1/routines/"+args[0]+"/autonomy", map[string]string{"autonomy": level(args[1]), "reason": reason})
	case "versions", "promote", "rollback", "retire":
		versionCmd(cmd, args, reason)
	case "input":
		inputCmd(args)
	case "pairs":
		pairsCmd(args)
	case "stop":
		need(args, 1, "stop fleet")
		post("/v1/fleets/"+args[0]+"/stop", nil)
	case "cancel":
		need(args, 1, `cancel RUN_ID --reason "..."`)
		post("/v1/runs/"+args[0]+"/cancel", map[string]string{"reason": reason})
	case "start":
		need(args, 1, `start fleet --reason "..."`)
		post("/v1/fleets/"+args[0]+"/start", map[string]string{"reason": reason})
	case "case":
		if len(args) > 0 && (args[0] == "approve" || args[0] == "reject" || args[0] == "close" || args[0] == "reassign" || args[0] == "answer" || args[0] == "answers") {
			caseDecision(args, reason)
			return
		}
		caseCmd(args, n)
	case "hold":
		holdCmd(args, reason)
	case "action":
		actionCmd(args, reason)
	case "host":
		hostCmd(args)
	case "eval":
		evalCmd(args, n)
	case "user":
		userCmd(args, role)
	default:
		usage()
	}
}

// caseDecision is `flotilla case approve ID [--plan HASH] | case reject|close|reassign ID --reason "..."`
// (RFC-0004 §5): an operator's answer to a proposed plan, or their way out of an escalated case.
// Approving sends it on to its host, signed (with --plan, only if that is still the case's plan);
// rejecting escalates the case; closing an escalated case releases its target; reassigning hands it
// back to its acting routine.
func caseDecision(args []string, reason string) {
	plan := flagValue(&args, "--plan")
	if len(args) > 0 && args[0] == "answers" {
		need(args, 2, `case answers ID`)
		var set struct {
			Case, Kind string
			Question   struct{ Title, Text string }
			Answers    []struct {
				ID, Label, Why string
				Allowed        bool
			}
		}
		get("/v1/cases/"+args[1]+"/answers", &set)
		fmt.Printf("%s (%s): %s\n", set.Case, set.Kind, set.Question.Title)
		for _, a := range set.Answers {
			if a.Allowed {
				fmt.Printf("  %s\n", a.ID)
			} else {
				fmt.Printf("  %s (not now: %s)\n", a.ID, a.Why)
			}
		}
		return
	}
	if len(args) > 0 && args[0] == "answer" {
		need(args, 3, `case answer ID ANSWER [--plan HASH] [--reason "..."]`)
		post("/v1/cases/"+args[1]+"/answers", map[string]string{"answer": args[2], "plan": plan, "reason": reason})
		return
	}
	need(args, 2, `case approve ID [--plan HASH] | case reject|close|reassign ID --reason "..." | case answers ID | case answer ID ANSWER [--plan HASH] [--reason "..."]`)
	if args[0] == "approve" {
		post("/v1/cases/"+args[1]+"/approve", map[string]string{"plan": plan})
		return
	}
	post("/v1/cases/"+args[1]+"/"+args[0], map[string]string{"reason": reason})
}

// caseCmd is `flotilla case list [--all] | show ID` (ADR-0005): the open cases, or one case's whole story.
func caseCmd(args []string, n string) {
	need(args, 1, "case list [--all] [-n N] | case show ID")
	switch args[0] {
	case "list":
		q := "/v1/cases?limit=" + n
		if len(args) > 1 && args[1] == "--all" {
			q += "&all=1"
		}
		var out struct{ Cases []map[string]any }
		get(q, &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "CASE\tKIND\tTARGET\tSTATUS\tSINCE\tRESOLUTION")
		for _, c := range out.Cases {
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n", c["id"], c["kind"], c["target"], c["status"], c["status_at"], orDash(c["resolution"]))
		}
		w.Flush()
	case "show":
		need(args, 2, "case show ID")
		var out struct {
			Case   map[string]any
			Events []map[string]any
		}
		get("/v1/cases/"+args[1], &out)
		c := out.Case
		res := ""
		if r, _ := c["resolution"].(string); r != "" {
			res = " (" + r + ")"
		}
		fmt.Printf("%v  %v  %v\nstatus %v since %v%s\nopened %v  key %v\n", c["id"], c["kind"], c["target"], c["status"], c["status_at"],
			res, c["opened_at"], c["canonical_key"])
		if n, _ := c["repeats"].(float64); n > 0 { // an unchanged detection is counted, not an event
			fmt.Printf("repeated %v times unchanged, the last %v\n", n, c["repeat_at"])
		}
		if s, _ := c["stage"].(string); s != "" {
			fmt.Printf("stage %v (handed on %v times)\n", s, c["stage_changes"])
		}
		for _, e := range out.Events {
			move := ""
			if e["to"] != nil && e["to"] != "" {
				move = fmt.Sprintf(" %v->%v", orDash(e["from"]), e["to"])
			}
			fmt.Printf("  %v  %-18v %-20v by %-9v%v  %v\n", e["at"], e["kind"], e["actor"], orDash(e["by"]), move, orDash(e["detail"]))
		}
	default:
		usage()
	}
}

// holdCmd is `flotilla hold SCOPE DURATION --reason R | hold list [--all] | hold lift ID --reason R`
// (RFC-0004 §5.1): while a hold lasts, policy defers every plan on that host or target.
func holdCmd(args []string, reason string) {
	need(args, 1, `hold host:<name> <duration> --reason "..." | hold list [--all] | hold lift ID --reason "..."`)
	switch args[0] {
	case "list":
		q := "/v1/holds"
		if len(args) > 1 && args[1] == "--all" {
			q += "?all=1"
		}
		var out struct{ Holds []map[string]any }
		get(q, &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSCOPE\tUNTIL\tBY\tREASON\tLIFTED")
		for _, h := range out.Holds {
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n", h["id"], h["scope"], h["until"], h["actor"], h["reason"], orDash(h["lifted_at"]))
		}
		w.Flush()
	case "lift":
		need(args, 2, `hold lift ID --reason "..."`)
		post("/v1/holds/"+args[1]+"/lift", map[string]string{"reason": reason})
	default:
		need(args, 2, `hold host:<name> <duration> --reason "..."`)
		post("/v1/holds", map[string]string{"scope": args[0], "for": args[1], "reason": reason})
	}
}

// hostCmd is `flotilla host list | host enrolment NAME | host revoke NAME` (ADR-0006 §3): host agents.
// The enrolment token is printed once; it goes to the host inside the SSH session only.
func hostCmd(args []string) {
	need(args, 1, "host list | host enrolment NAME | host revoke NAME")
	switch args[0] {
	case "list":
		var out struct{ Hosts []map[string]any }
		get("/v1/hosts", &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "HOST\tCONNECTED\tVERSION\tLAST CONTACT\tCLOCK OFFSET\tCERTIFICATE UNTIL\tREVOKED\tCHAIN HEAD")
		for _, h := range out.Hosts {
			conn := "no"
			if h["connected"] == true {
				conn = "yes"
			}
			offset := "-"
			if ms, ok := h["clock_offset_ms"].(float64); ok && h["last_contact"] != "" {
				offset = (time.Duration(ms) * time.Millisecond).String()
			}
			head, _ := h["chain_head"].(string)
			if len(head) > 12 {
				head = head[:12]
			}
			fmt.Fprintf(w, "%v\t%s\t%v\t%v\t%s\t%v\t%v\t%v\n", h["name"], conn, orDash(h["version"]), orDash(h["last_contact"]), offset,
				orDash(h["cert_not_after"]), orDash(h["revoked_at"]), orDash(head))
		}
		w.Flush()
	case "enrolment":
		need(args, 2, "host enrolment NAME")
		post("/v1/hosts/"+args[1]+"/enrolment", nil)
	case "revoke":
		need(args, 2, "host revoke NAME")
		post("/v1/hosts/"+args[1]+"/revoke", nil)
	case "audit":
		logPath := flagValue(&args, "--log")
		need(args, 2, "host audit NAME --log FILE   (FILE: the host's audit log, fetched over SSH)")
		if logPath == "" {
			need(nil, 1, "host audit NAME --log FILE")
		}
		os.Exit(hostAudit(args[1], logPath))
	default:
		need(nil, 1, "host list | host enrolment NAME | host revoke NAME | host audit NAME --log FILE")
	}
}

// hostAudit compares a host's audit log with flotillad's record of what it sent there (ADR-0006 §8).
// It prints one line per finding and AUDIT PASS or AUDIT FAIL; the exit code is 1 on a failure.
// --log - reads the log from stdin (ssh HOST cat /var/log/flotilla-host/audit.jsonl | flotilla host audit ...).
func hostAudit(host, logPath string) int {
	var log []byte
	var err error
	if logPath == "-" {
		log, err = io.ReadAll(io.LimitReader(os.Stdin, 256<<20))
	} else {
		log, err = os.ReadFile(logPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var out struct {
		ChainHead  string `json:"chain_head"`
		Executions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"executions"`
	}
	get("/v1/hosts/"+host+"/executions", &out)
	var recs []hostaudit.Record
	for _, x := range out.Executions {
		recs = append(recs, hostaudit.Record{ID: x.ID, Status: x.Status})
	}
	fs := hostaudit.Compare(log, recs, out.ChainHead)
	for _, f := range fs {
		fmt.Println(f.String())
	}
	if hostaudit.Failed(fs) {
		fmt.Printf("AUDIT FAIL: %s, %d executions on record\n", host, len(recs))
		return 1
	}
	fmt.Printf("AUDIT PASS: %s, %d executions on record\n", host, len(recs))
	return 0
}

// actionCmd is `flotilla action list | action autonomy NAME LEVEL [--reason R]` (ADR-0008): an action's
// own autonomy, promoted one step at a time on its record.
func actionCmd(args []string, reason string) {
	need(args, 1, `action list | action autonomy NAME record|propose|auto [--reason "..."]`)
	switch args[0] {
	case "list":
		var out struct{ Actions []map[string]any }
		get("/v1/actions", &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ACTION\tAUTONOMY\tMAX\tPLANS\tRECORD WOULD BE\tVERIFIED\tFAILED")
		for _, a := range out.Actions {
			rec, _ := a["record"].(map[string]any)
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n", a["name"], a["autonomy"], a["max"], rec["plans"], rec["would"], rec["verified"], rec["failed"])
		}
		w.Flush()
	case "autonomy":
		need(args, 3, `action autonomy NAME record|propose|auto [--reason "..."]`)
		post("/v1/actions/"+args[1]+"/autonomy", map[string]string{"autonomy": level(args[2]), "reason": reason})
	default:
		usage()
	}
}

// userCmd is `flotilla user …` (ADR-0014). A new or rotated token is printed once.
func userCmd(args []string, role string) {
	need(args, 1, "user add|list|role|rotate|revoke …")
	switch args[0] {
	case "list":
		var out struct {
			Users []map[string]any
		}
		get("/v1/users", &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tROLE\tCREATED\tREVOKED")
		for _, u := range out.Users {
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\n", u["name"], u["role"], u["created_at"], orDash(u["revoked_at"]))
		}
		w.Flush()
	case "add":
		need(args, 2, "user add NAME --role admin|operator|viewer")
		if role == "" {
			fmt.Fprintln(os.Stderr, "user add needs --role admin|operator|viewer")
			os.Exit(2)
		}
		post("/v1/users", map[string]string{"name": args[1], "role": role})
	case "role":
		need(args, 3, "user role NAME admin|operator|viewer")
		post("/v1/users/"+args[1]+"/role", map[string]string{"role": args[2]})
	case "rotate", "revoke":
		need(args, 2, "user "+args[0]+" NAME")
		post("/v1/users/"+args[1]+"/"+args[0], nil)
	default:
		usage()
	}
}

// evalCmd is `flotilla eval record … | eval list [--routine R]`: the numbers an eval measured for a routine's
// prompt on a model. --metric may repeat, one result each.
func evalCmd(args []string, n string) {
	need(args, 1, "eval record --routine R --prompt-sha S --model M --metric NAME=VALUE [--n N] [--repeats N] [--source TEXT] | eval list [--routine R]")
	switch args[0] {
	case "list":
		rest := args[1:]
		q := "/v1/evals?limit=" + n
		if r := flagValue(&rest, "--routine"); r != "" {
			q += "&routine=" + r
		}
		var out struct{ Evals []map[string]any }
		get(q, &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "RECORDED\tROUTINE\tPROMPT\tMODEL\tMETRIC\tVALUE\tN\tREPEATS\tBY\tSOURCE")
		for _, e := range out.Evals {
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\n", e["recorded_at"], e["routine"], e["prompt_sha"], e["model"], e["metric"], e["value"],
				e["n"], e["repeats"], e["recorded_by"], orDash(e["source"]))
		}
		w.Flush()
	case "record":
		rest := args[1:]
		var metrics []string
		for m := flagValue(&rest, "--metric"); m != ""; m = flagValue(&rest, "--metric") {
			metrics = append(metrics, m)
		}
		base := map[string]any{"routine": flagValue(&rest, "--routine"), "prompt_sha": flagValue(&rest, "--prompt-sha"),
			"model": flagValue(&rest, "--model"), "source": flagValue(&rest, "--source")}
		for _, f := range []string{"--n", "--repeats"} {
			v := flagValue(&rest, f)
			if v == "" {
				continue
			}
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil || i < 0 {
				fmt.Fprintf(os.Stderr, "%s: a whole number, not %q\n", f, v)
				os.Exit(2)
			}
			base[strings.TrimPrefix(f, "--")] = i
		}
		if len(metrics) == 0 || len(rest) > 0 {
			fmt.Fprintln(os.Stderr, "usage: flotilla eval record --routine R --prompt-sha S --model M --metric NAME=VALUE [--n N] [--repeats N] [--source TEXT]")
			os.Exit(2)
		}
		for _, m := range metrics {
			name, val, ok := strings.Cut(m, "=")
			v, err := strconv.ParseFloat(val, 64)
			if !ok || name == "" || err != nil {
				fmt.Fprintf(os.Stderr, "--metric %q: NAME=NUMBER\n", m)
				os.Exit(2)
			}
			body := map[string]any{"metric": name, "value": v}
			for k, x := range base {
				body[k] = x
			}
			post("/v1/evals", body)
		}
	default:
		usage()
	}
}

// flagValue removes `name value` from args and returns the value ("" when absent).
func flagValue(args *[]string, name string) string {
	a := *args
	for i := 0; i < len(a); i++ {
		if a[i] == name && i+1 < len(a) {
			v := a[i+1]
			*args = append(a[:i], a[i+2:]...)
			return v
		}
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: flotilla status | runs [routine] [-n N] | events [-n N] | run|pause ROUTINE | resume ROUTINE --reason R | autonomy ROUTINE LEVEL [--reason R] | autonomy freeze | autonomy unfreeze --reason R | case list [--all] | case show ID | case approve ID [--plan HASH] | case reject|close|reassign ID --reason R | case answers ID | case answer ID ANSWER [--plan HASH] [--reason R] | hold SCOPE DURATION --reason R | hold list | hold lift ID --reason R | action list | action autonomy NAME LEVEL [--reason R] | versions ROUTINE | versions shadow-run ROUTINE vN --input KEY | promote ROUTINE vN --autonomy L --reason R [--no-pairs] [--gate NAME=pass|fail[@REF]]... [--eval RUN_ID=pass|fail]... | rollback ROUTINE --autonomy L --reason R | retire ROUTINE vN --reason R | pairs ROUTINE vN [--since T] [--until T] [--show differ|unpaired] [--json] | pairs export ROUTINE vN --to DIR | input show|pin|unpin KEY | input export KEY --to DIR [--with-ledger] [--with-state] | stop FLEET | cancel RUN_ID --reason R | start FLEET --reason R | eval record --routine R --prompt-sha S --model M --metric NAME=VALUE [--n N] [--repeats N] [--source TEXT] | eval list [--routine R] | user add|list|role|rotate|revoke | hash-token")
	os.Exit(2)
}

func need(args []string, n int, form string) {
	if len(args) < n {
		fmt.Fprintln(os.Stderr, "usage: flotilla", form)
		os.Exit(2)
	}
}

func orDash(v any) any {
	if v == nil || v == "" {
		return "-"
	}
	return v
}

func base() string {
	if u := os.Getenv("FLOTILLA_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:8484"
}

func token() string {
	if t := os.Getenv("FLOTILLA_TOKEN"); t != "" {
		return t
	}
	p := os.Getenv("FLOTILLA_TOKEN_FILE")
	if p == "" {
		fmt.Fprintln(os.Stderr, "neither FLOTILLA_TOKEN nor FLOTILLA_TOKEN_FILE is set")
		os.Exit(2)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return strings.TrimSpace(string(b))
}

func do(method, path string, body any) []byte {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base()+path, rd)
	req.Header.Set("Authorization", "Bearer "+token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "HTTP %d: %s\n", resp.StatusCode, bytes.TrimSpace(raw))
		os.Exit(1)
	}
	return raw
}

func get(path string, out any) {
	if err := json.Unmarshal(do("GET", path, nil), out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// level reads an autonomy word, the deprecated shadow as record, and says so on stderr.
func level(s string) string {
	l, note := spec.CanonicalLevel(s)
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return l
}

func post(path string, body any) {
	fmt.Println(string(bytes.TrimSpace(do("POST", path, body))))
}

// exampleRows prints the examples catalogue (flotillad.yaml examples_dir) after the routines: listed and never run, so the state is
// example and there is no autonomy, last run, stale flag or spend.
func exampleRows(w io.Writer, examples []map[string]any) {
	for _, e := range examples {
		fmt.Fprintf(w, "%v\t-\t-\t%v\t-\t-\t-\texample\n", e["id"], e["trigger"])
	}
}
