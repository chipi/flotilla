// flotilla — the command line for flotillad's API.
//
//	flotilla status
//	flotilla runs [fleet/routine] [-n 20]
//	flotilla events [-n 50]
//	flotilla run fleet/routine
//	flotilla pause|resume fleet/routine
//	flotilla stage fleet/routine shadow|propose|live      (operator only)
//	flotilla stop|start fleet                          (start: operator only)
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
			Version  string `json:"version"`
			Routines []map[string]any
			Workers  map[string]any
		}
		get("/v1/status", &out)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintf(w, "flotillad %s\nROUTINE\tSTAGE\tEVERY\tLAST OK\tSTALE\tSPEND TODAY\tSTATE\n", out.Version)
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
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t$%.4f/$%.2f\t%s\n", j["id"], j["stage"], j["every"], orDash(j["last_success"]),
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
		fmt.Fprintln(w, "STARTED\tROUTINE\tSTATUS\tSTAGE\tSECONDS\tSPEND\tTRIGGER\tDETAIL")
		for _, r := range out.Runs {
			d, _ := r["detail"].(string)
			if len(d) > 80 {
				d = d[len(d)-80:]
			}
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v/%v\t%s\n", r["started_at"], r["routine"], r["status"], r["stage"],
				orDash(r["duration_s"]), r["spend_usd"], r["trigger"], r["actor"], strings.ReplaceAll(d, "\n", " "))
		}
		w.Flush()
	case "events":
		var out struct{ Events []map[string]any }
		get("/v1/events?limit="+n, &out)
		for _, e := range out.Events {
			fmt.Printf("%v  %-16v %-14v %-22v %v %v\n", e["at"], e["actor"], e["kind"], e["subject"], e["via"], e["detail"])
		}
	case "run", "pause", "resume":
		need(args, 1, cmd+" fleet/routine")
		post("/v1/routines/"+args[0]+"/"+cmd, nil)
	case "stage":
		need(args, 2, "stage fleet/routine shadow|propose|live")
		post("/v1/routines/"+args[0]+"/stage", map[string]string{"stage": args[1]})
	case "stop", "start":
		need(args, 1, cmd+" fleet")
		post("/v1/fleets/"+args[0]+"/"+cmd, nil)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: flotilla status | runs [routine] [-n N] | events [-n N] | run|pause|resume ROUTINE | stage ROUTINE STAGE | stop|start FLEET | hash-token")
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
