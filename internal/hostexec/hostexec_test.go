package hostexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/envelope"
	"github.com/chipi/flotilla/internal/hostaudit"
	"github.com/chipi/flotilla/internal/hostcfg"
)

var priv = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func config(t *testing.T, ceiling string) *hostcfg.Config {
	t.Helper()
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	c, err := hostcfg.Parse([]byte(`{"host": "mini", "user": "_flotilla", "ceiling": "` + ceiling + `",
	  "signing_keys": {"k1": "` + pub + `"},
	  "actions": {
	    "backup.rerun_observability": {
	      "argv": ["/usr/bin/sudo", "-n", "/bin/launchctl", "kickstart", "system/com.homelab.db-backup"],
	      "params": {"host": {"kind": "this_host"}}, "per_hour": 2, "timeout": "1s",
	      "pre": [{"kind": "launchd_not_running", "args": {"label": "system/com.homelab.db-backup"}}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeRun struct {
	mu       sync.Mutex
	calls    [][]string
	exit     int
	out      string
	timedOut bool
}

func (f *fakeRun) Run(ctx context.Context, argv []string, timeout time.Duration) (int, []byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	return f.exit, []byte(f.out), f.timedOut, nil
}

type fakeCheck struct{ holds bool }

func (f fakeCheck) Check(ctx context.Context, p hostcfg.Pre) (bool, string, error) {
	return f.holds, "launchctl says running", nil
}

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func exe(id string) envelope.Execution {
	return envelope.Execution{ID: id, CaseID: "case-1", Host: "mini", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, PlanHash: "h", Approved: "user:ada", SignedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
}

func sign(t *testing.T, e envelope.Execution) []byte {
	t.Helper()
	raw, err := envelope.Sign(priv, "k1", e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func setup(t *testing.T, ceiling string) (*Executor, *fakeRun, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.db")
	db, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	audit, err := hostaudit.Open(filepath.Join(filepath.Dir(path), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })
	run := &fakeRun{out: "kicked"}
	x := New(db, audit, config(t, ceiling), run, fakeCheck{holds: true})
	x.Now = func() time.Time { return now.Add(time.Minute) }
	return x, run, path
}

func TestRunsOnceAndAResendGetsTheResult(t *testing.T) {
	ctx := context.Background()
	x, run, _ := setup(t, "propose")
	raw := sign(t, exe("exec-1"))
	r := x.Handle(ctx, raw)
	if r.Status != Done || len(run.calls) != 1 || strings.Join(run.calls[0], " ") != "/usr/bin/sudo -n /bin/launchctl kickstart system/com.homelab.db-backup" {
		t.Fatalf("first: %+v %v", r, run.calls)
	}
	again := x.Handle(ctx, raw)
	if !again.Resent || again.Status != Done || again.OutputSHA != r.OutputSHA || len(run.calls) != 1 {
		t.Fatalf("an identical resend ran again or lost its result: %+v (%d runs)", again, len(run.calls))
	}
	// Same id, different (validly signed) bytes: a forgery.
	forged := exe("exec-1")
	forged.CaseID = "case-2"
	if r := x.Handle(ctx, sign(t, forged)); r.Status != Refused || !strings.Contains(r.Detail, "forgery") || len(run.calls) != 1 {
		t.Fatalf("a different envelope under a seen id: %+v", r)
	}
}

// Every outcome is a line in the audit log, `started` before the command, and each result carries the
// head after its own line.
func TestAuditLog(t *testing.T) {
	ctx := context.Background()
	x, run, path := setup(t, "propose")
	raw := sign(t, exe("exec-1"))
	ran := x.Handle(ctx, raw)
	resent := x.Handle(ctx, raw)
	auto := exe("exec-2")
	auto.Approved = "auto" // above the ceiling, propose
	refusedR := x.Handle(ctx, sign(t, auto))
	f, err := os.Open(filepath.Join(filepath.Dir(path), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	hashes, err := hostaudit.Verify(bytes.NewReader(body))
	if err != nil || len(hashes) != 4 {
		t.Fatalf("the log: %d lines, %v\n%s", len(hashes), err, body)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var e hostaudit.Entry
		json.Unmarshal([]byte(line), &e)
		got = append(got, e.Event+" "+e.ExecutionID+" "+e.Status+" "+e.Approved+" "+strings.Join(e.Argv, " "))
	}
	want := []string{
		"started exec-1  user:ada /usr/bin/sudo -n /bin/launchctl kickstart system/com.homelab.db-backup",
		"result exec-1 done user:ada ",
		"resent exec-1 done user:ada ",
		"refused exec-2 refused auto ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the log's lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if ran.ChainHead != hashes[1] || resent.ChainHead != hashes[2] || refusedR.ChainHead != hashes[3] {
		t.Fatalf("chain heads %s %s %s, want %v", ran.ChainHead, resent.ChainHead, refusedR.ChainHead, hashes)
	}
	// A log that cannot be written to: nothing runs.
	x.Audit.Close()
	if r := x.Handle(ctx, sign(t, exe("exec-3"))); r.Status != Refused || !strings.Contains(r.Detail, "audit log") || len(run.calls) != 1 {
		t.Fatalf("ran without a log: %+v, %d runs", r, len(run.calls))
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	auto := exe("exec-auto")
	auto.Approved = "auto"
	notAllowed := exe("exec-rm")
	notAllowed.Action = "rm.everything"
	otherHost := exe("exec-dgx")
	otherHost.Host, otherHost.Params = "dgx", map[string]string{"host": "dgx"}
	forgedKey, _ := envelope.Sign(other, "k1", exe("exec-forged"))
	for name, c := range map[string]struct {
		ceiling string
		raw     []byte
		want    string
	}{
		"another key":                  {"propose", forgedKey, "signature"},
		"another host":                 {"propose", sign(t, otherHost), "another host"},
		"not in this host's config":    {"propose", sign(t, notAllowed), "not allowed on this host"},
		"auto above a propose host":    {"propose", sign(t, auto), "above this host's ceiling"},
		"anything on a shadow host":    {"shadow", sign(t, exe("exec-s")), "accepts nothing"},
		"an extra parameter":           {"propose", sign(t, func() envelope.Execution { e := exe("exec-x"); e.Params["force"] = "1"; return e }()), "not declared"},
		"a parameter for another host": {"propose", sign(t, func() envelope.Execution { e := exe("exec-y"); e.Params["host"] = "dgx"; return e }()), "this is mini"},
	} {
		x, run, _ := setup(t, c.ceiling)
		r := x.Handle(ctx, c.raw)
		if r.Status != Refused || !strings.Contains(r.Detail, c.want) || len(run.calls) != 0 {
			t.Errorf("%s: %+v, %d runs", name, r, len(run.calls))
		}
	}
	// auto on an auto host runs
	x, run, _ := setup(t, "auto")
	if r := x.Handle(ctx, sign(t, auto)); r.Status != Done || len(run.calls) != 1 {
		t.Fatalf("auto on an auto host: %+v", r)
	}
}

func TestRateLimit(t *testing.T) {
	ctx := context.Background()
	x, run, _ := setup(t, "propose")
	for i, id := range []string{"a", "b", "c"} {
		r := x.Handle(ctx, sign(t, exe(id)))
		if i < 2 && r.Status != Done {
			t.Fatalf("execution %d under the limit: %+v", i, r)
		}
		if i == 2 && (r.Status != Refused || !strings.Contains(r.Detail, "its limit")) {
			t.Fatalf("the third in an hour (limit 2): %+v", r)
		}
	}
	if len(run.calls) != 2 {
		t.Fatalf("%d runs, want 2", len(run.calls))
	}
	x.Now = func() time.Time { return now.Add(62 * time.Minute) }
	late := exe("d")
	late.SignedAt, late.ExpiresAt = now.Add(61*time.Minute), now.Add(65*time.Minute)
	if r := x.Handle(ctx, sign(t, late)); r.Status != Done {
		t.Fatalf("an hour later: %+v", r)
	}
}

func TestOutcomes(t *testing.T) {
	ctx := context.Background()
	x, run, _ := setup(t, "propose")
	x.Check = fakeCheck{holds: false}
	if r := x.Handle(ctx, sign(t, exe("pre"))); r.Status != PreconditionFalse || len(run.calls) != 0 {
		t.Fatalf("a precondition that no longer holds: %+v, %d runs", r, len(run.calls))
	}
	x.Check = fakeCheck{holds: true}
	run.exit, run.out = 3, strings.Repeat("x", OutputMax+100)
	r := x.Handle(ctx, sign(t, exe("fail")))
	if r.Status != Failed || r.ExitCode != 3 || len(r.Output) != OutputMax || r.OutputSHA == "" {
		t.Fatalf("a failed command: status %s exit %d output %d", r.Status, r.ExitCode, len(r.Output))
	}
	run.exit, run.timedOut = 0, true
	if r := x.Handle(ctx, sign(t, exe("slow"))); r.Status != Unknown || !strings.Contains(r.Detail, "target stays held") {
		t.Fatalf("a timeout: %+v", r)
	}
}

// A started execution with no result (the agent died mid-run) is unknown after a restart and never runs
// again, even when its envelope is sent again.
func TestStartedIsUnknownAfterARestart(t *testing.T) {
	ctx := context.Background()
	x, run, path := setup(t, "propose")
	raw := sign(t, exe("crash"))
	_, payload, _ := envelope.Verify(raw, x.Cfg.Keys(), "mini", now.Add(time.Minute))
	if _, err := x.db.Exec(`INSERT INTO executions (id, payload, action, argv, status, started_at) VALUES ('crash', ?, 'backup.rerun_observability', '[]', 'started', ?)`,
		payload, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	x.db.Close()
	db, n, err := Open(path)
	if err != nil || n != 1 {
		t.Fatalf("reopen: %d marked, %v", n, err)
	}
	defer db.Close()
	y := New(db, x.Audit, x.Cfg, run, fakeCheck{holds: true})
	y.Now = x.Now
	if r := y.Handle(ctx, raw); r.Status != Unknown || !r.Resent || len(run.calls) != 0 {
		t.Fatalf("the crashed execution sent again: %+v, %d runs", r, len(run.calls))
	}
}

// The real runner: no shell, a timeout kills the whole group.
func TestExecRunner(t *testing.T) {
	ctx := context.Background()
	exit, out, timedOut, err := ExecRunner{}.Run(ctx, []string{"/bin/echo", "a;b", "$HOME"}, 5*time.Second)
	if err != nil || timedOut || exit != 0 || string(out) != "a;b $HOME\n" {
		t.Fatalf("echo: %d %q %v %v (a shell would have expanded it)", exit, out, timedOut, err)
	}
	marker := filepath.Join(t.TempDir(), "child-alive")
	start := time.Now()
	_, _, timedOut, _ = ExecRunner{}.Run(ctx, []string{"/bin/sh", "-c", "(sleep 2; touch " + marker + ") & sleep 30"}, 500*time.Millisecond)
	if !timedOut || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %v after %s", timedOut, time.Since(start))
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child of the timed-out command survived")
	}
	if exit, _, _, _ := (ExecRunner{}).Run(ctx, []string{"/bin/sh", "-c", "exit 4"}, 5*time.Second); exit != 4 {
		t.Fatalf("exit code %d", exit)
	}
}

// A command's environment is built, never the agent's own (which may hold its secrets).
func TestExecRunnerEnvironmentIsNotInherited(t *testing.T) {
	t.Setenv("FLOTILLA_TEST_SECRET", "s3cret")
	_, out, _, err := ExecRunner{}.Run(context.Background(), []string{"/usr/bin/env"}, 5*time.Second)
	if err != nil || strings.Contains(string(out), "s3cret") || !strings.Contains(string(out), "PATH=/usr/bin:/bin") {
		t.Fatalf("the command's environment: %q %v", out, err)
	}
}

type cannedRun struct {
	exit int
	out  string
}

func (c cannedRun) Run(ctx context.Context, argv []string, timeout time.Duration) (int, []byte, bool, error) {
	return c.exit, []byte(c.out), false, nil
}

// The mini's `launchctl print system/com.homelab.db-backup` (2026-11-13), cut to the lines around state.
const launchdIdle = "system/com.homelab.db-backup = {\n\tactive count = 0\n\tpath = /Library/LaunchDaemons/com.homelab.db-backup.plist\n\ttype = LaunchDaemon\n\tstate = not running\n\n\tprogram = /bin/bash\n}\n"

func TestLaunchdNotRunning(t *testing.T) {
	ctx := context.Background()
	pre := hostcfg.Pre{Kind: "launchd_not_running", Args: map[string]string{"label": "system/com.homelab.db-backup"}}
	if holds, detail, err := (LocalChecker{Run: cannedRun{out: launchdIdle}}).Check(ctx, pre); err != nil || !holds || detail != "system/com.homelab.db-backup is not running" {
		t.Fatalf("idle: %v %q %v", holds, detail, err)
	}
	running := strings.Replace(launchdIdle, "state = not running", "state = running", 1)
	if holds, _, err := (LocalChecker{Run: cannedRun{out: running}}).Check(ctx, pre); err != nil || holds {
		t.Fatalf("running: %v %v", holds, err)
	}
	for name, c := range map[string]struct {
		run Runner
		pre hostcfg.Pre
	}{
		"launchctl failed": {cannedRun{exit: 113, out: launchdIdle}, pre}, // a failed exit is not believed, whatever it printed
		"no state line":    {cannedRun{out: "system/x = {\n}\n"}, pre},
		"a bad label":      {cannedRun{out: launchdIdle}, hostcfg.Pre{Kind: "launchd_not_running", Args: map[string]string{"label": "system/x; rm -rf /"}}},
		"an unknown kind":  {cannedRun{out: launchdIdle}, hostcfg.Pre{Kind: "disk_free_above"}},
	} {
		if _, _, err := (LocalChecker{Run: c.run}).Check(ctx, c.pre); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
