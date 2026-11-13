package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

func TestCasesAPI(t *testing.T) {
	ts, st := server(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini"}
	id, _, _ := st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "age 20.4h")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	c, out := call(t, ts, "GET", "/v1/cases", agentTok, "")
	if c != 200 || len(out["cases"].([]any)) != 1 {
		t.Fatalf("open cases: %d %v", c, out)
	}
	c, out = call(t, ts, "GET", "/v1/cases/"+id, agentTok, "")
	if c != 200 || out["case"].(map[string]any)["status"] != cases.HandedOver || len(out["events"].([]any)) != 2 {
		t.Fatalf("the case and its timeline: %d %v", c, out)
	}
	if c, _ := call(t, ts, "GET", "/v1/cases/case-nope", agentTok, ""); c != 404 {
		t.Fatalf("no such case: %d", c)
	}
	_ = st.MoveCase(ctx, id, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", "")
	if _, out := call(t, ts, "GET", "/v1/cases", agentTok, ""); len(out["cases"].([]any)) != 0 {
		t.Fatalf("a closed case listed as open: %v", out)
	}
	if _, out := call(t, ts, "GET", "/v1/cases?all=1", agentTok, ""); len(out["cases"].([]any)) != 1 {
		t.Fatalf("all=1: %v", out)
	}
}
