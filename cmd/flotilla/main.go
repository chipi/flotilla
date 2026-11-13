// flotilla — the command line for flotillad's API.
//
//	flotilla status
//	flotilla runs [fleet/routine] [-n 20]
//	flotilla events [-n 50]
//	flotilla run fleet/routine
//	flotilla pause fleet/routine
//	flotilla resume fleet/routine --reason "..."        (operator; loosening needs a reason)
//	flotilla autonomy fleet/routine shadow|propose|auto [--reason "..."]   (operator; raising needs one)
//	flotilla stop fleet
//	flotilla start fleet --reason "..."                 (operator)
//	flotilla user add NAME --role admin|operator|viewer (admin; the first admin with the host's operator token)
//	flotilla user list | role NAME ROLE | rotate NAME | revoke NAME   (admin)
//	flotilla hash-token < token                        (the line for flotillad's callers file)
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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chipi/flotilla/internal/api"
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
	// -json: the API's answer as it is, for scripts (deploy/mini/mirror.sh)
	for i := 0; i < len(args); i++ {
		if args[i] == "-json" {
			args = append(args[:i], args[i+1:]...)
			switch cmd {
			case "status":
				os.Stdout.Write(do("GET", "/v1/status", nil))
			case "runs":
				q := "/v1/runs?limit=" + n
				if len(args) > 0 {
					q += "&routine=" + args[0]
				}
				os.Stdout.Write(do("GET", q, nil))
			default:
				fmt.Fprintln(os.Stderr, "-json works with status and runs")
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
		}
		get("/v1/status", &out)
		if out.Freeze.Frozen {
			fmt.Printf("FROZEN since %s by %s: every routine runs at shadow (AUTONOMY shows what runs; SET, the routine's own)\n", out.Freeze.At, out.Freeze.By)
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
		need(args, 2, `autonomy fleet/routine shadow|propose|auto [--reason "..."] | autonomy freeze | autonomy unfreeze --reason "..."`)
		post("/v1/routines/"+args[0]+"/autonomy", map[string]string{"autonomy": args[1], "reason": reason})
	case "stop":
		need(args, 1, "stop fleet")
		post("/v1/fleets/"+args[0]+"/stop", nil)
	case "start":
		need(args, 1, `start fleet --reason "..."`)
		post("/v1/fleets/"+args[0]+"/start", map[string]string{"reason": reason})
	case "case":
		caseCmd(args, n)
	case "hold":
		holdCmd(args, reason)
	case "user":
		userCmd(args, role)
	default:
		usage()
	}
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
	fmt.Fprintln(os.Stderr, "usage: flotilla status | runs [routine] [-n N] | events [-n N] | run|pause ROUTINE | resume ROUTINE --reason R | autonomy ROUTINE LEVEL [--reason R] | autonomy freeze | autonomy unfreeze --reason R | case list [--all] | case show ID | hold SCOPE DURATION --reason R | hold list | hold lift ID --reason R | stop FLEET | start FLEET --reason R | user add|list|role|rotate|revoke | hash-token")
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

func post(path string, body any) {
	fmt.Println(string(bytes.TrimSpace(do("POST", path, body))))
}
