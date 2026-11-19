package input

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

const data = "../../runtime/testdata"

func goldens(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(data, "input-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if filepath.Base(f) != "input-invalid.json" {
			out = append(out, f)
		}
	}
	if len(out) < 7 {
		t.Fatalf("the golden inputs: %v", out)
	}
	return out
}

// The golden files the Python checker validates (runtime/test_schemacheck.py) decode strictly here and marshal back to the same
// JSON: the envelope loses nothing and adds nothing.
func TestGoldenInputsRoundTrip(t *testing.T) {
	for _, f := range goldens(t) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			e, err := Decode(raw)
			if err != nil {
				t.Fatalf("a valid golden input did not decode: %v", err)
			}
			back, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(back, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip changed it:\n want %s\n  got %s", raw, back)
			}
		})
	}
}

// The invalid inputs the schema refuses: Go refuses every one it can see, and the ones it cannot (a version of 0 reads as absent)
// are marked in the file.
func TestInvalidInputsAreRefused(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(data, "input-invalid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string          `json:"name"`
		Doc  json.RawMessage `json:"doc"`
		Go   bool            `json:"go"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 30 {
		t.Fatalf("%d invalid cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := Decode(c.Doc)
			if c.Go && err == nil {
				t.Fatalf("Go accepted what the schema refuses: %s", c.Doc)
			}
			if !c.Go && err != nil {
				t.Fatalf("marked as one Go cannot see, but it refused: %v", err)
			}
		})
	}
}

func TestDecodeIsStrict(t *testing.T) {
	good := `{"schema_version":1,"input_key":"k","routine":"a/b","kind":"tick"}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"unknown field":   `{"schema_version":1,"input_key":"k","routine":"a/b","kind":"tick","extra":1}`,
		"two values":      good + good,
		"not an object":   `[1]`,
		"unknown nested":  `{"schema_version":1,"input_key":"k","routine":"a/b","kind":"tick","commit":{"repo":"o/r","sha":"0123456","branch":"m"}}`,
		"trailing text":   good + " x",
		"an empty ledger": `{"schema_version":1,"input_key":"k","routine":"a/b","kind":"tick","ledger":{"rows":[]}}`,
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKindOfAnInput(t *testing.T) {
	for _, c := range []struct {
		trigger string
		handed  bool
		want    string
	}{
		{"schedule", false, "tick"}, {"manual", false, "tick"}, {"case", true, "case"}, {"check", true, "check"},
		{"execute", true, "execute"}, {"manual", true, "case"}, {"", false, "tick"},
	} {
		if got := Kind(c.trigger, c.handed); got != c.want {
			t.Errorf("Kind(%q, %v) = %s, want %s", c.trigger, c.handed, got, c.want)
		}
	}
}

func TestHandedKeepsWhatIsJSONUnderItsField(t *testing.T) {
	got := Handed(map[string]string{
		"FLOTILLA_CASE": `{"id":"c1"}`, "FLOTILLA_PLAN": `{"action":"a.b"}`, "FLOTILLA_CHECK": `{"verify":"x"}`,
		"FLOTILLA_RESUME": `{"run_id":"r0"}`, "FLEETD_STAGE": "shadow", "FOO": "bar",
	})
	var m map[string]map[string]any
	if err := json.Unmarshal(got, &m); err != nil || len(m) != 4 || m["case"]["id"] != "c1" || m["plan"]["action"] != "a.b" || m["check"]["verify"] != "x" || m["resume"]["run_id"] != "r0" {
		t.Fatalf("%s %v", got, err)
	}
	if string(Handed(nil)) != "{}" || string(Handed(map[string]string{"FLOTILLA_CASE": "not json"})) != "{}" || string(Handed(map[string]string{"FLOTILLA_CASE": "[1]"})) != "{}" {
		t.Fatalf("nothing JSON handed gives {}: %s", Handed(map[string]string{"FLOTILLA_CASE": "not json"}))
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFromStoreMakesAValidEnvelope(t *testing.T) {
	st, ctx := openStore(t), context.Background()
	in := store.Input{Key: "live-1", Routine: "ops/backup-monitor", Kind: store.InputTick, Version: 2, Trigger: "schedule", Actor: "flotillad",
		StartedAt: "2026-11-19T10:00:00.000000000Z", MarkFilings: 7, MarkEntries: 9, LiveCaseKeys: []string{"alert=a"},
		CommitRepo: "o/r", CommitSHA: "0123456789abcdef", SnapshotPaths: []string{"state"}, SnapshotState: store.SnapshotHeld,
		Handed: Handed(map[string]string{"FLOTILLA_CASE": `{"id":"c1"}`})}
	if err := st.NewInput(ctx, in); err != nil {
		t.Fatal(err)
	}
	back, err := st.Input(ctx, "live-1")
	if err != nil {
		t.Fatal(err)
	}
	e, err := FromStore(back)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("a recorded input is a valid envelope: %v", err)
	}
	if e.TickTime != in.StartedAt || e.Version != 2 || *e.Ledger.MarkFilings != 7 || *e.Ledger.MarkEntries != 9 ||
		(*e.LiveCaseKeys)[0] != "alert=a" || e.Commit.SHA != "0123456789abcdef" || e.StateSnapshot.Paths[0] != "state" || string(e.Case) != `{"id":"c1"}` {
		t.Fatalf("%+v", e)
	}
	// a case run has no tick time; one with no live case keys still says so (an empty list, not absent)
	in2 := store.Input{Key: "live-2", Routine: "ops/rerun-backup", Kind: store.InputCase, StartedAt: "2026-11-19T10:01:00.000000000Z"}
	if err := st.NewInput(ctx, in2); err != nil {
		t.Fatal(err)
	}
	b2, _ := st.Input(ctx, "live-2")
	e2, err := FromStore(b2)
	if err != nil || e2.TickTime != "" || e2.LiveCaseKeys == nil || len(*e2.LiveCaseKeys) != 0 || e2.Commit != nil || e2.StateSnapshot != nil {
		t.Fatalf("%+v %v", e2, err)
	}
	if raw, _ := json.Marshal(e2); !strings.Contains(string(raw), `"live_case_keys":[]`) {
		t.Fatalf("an empty list is kept as one: %s", raw)
	}
	if _, err := Decode(mustJSON(t, e2)); err != nil {
		t.Fatalf("what flotillad writes decodes: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The ledger rows as of the mark: later live rows are not in it, and the envelope is still valid.
func TestWithLedgerReadsTheRowsAsOfTheMark(t *testing.T) {
	st, ctx := openStore(t), context.Background()
	ns := "triage/cycle"
	st.WriteFiling(ctx, ns, map[string]string{"fingerprint": "a", "repo": "o/r", "issue": "1"}, "r0")
	st.AppendEntry(ctx, ns, "decision", map[string]string{"fingerprint": "a", "disposition": "file"}, "r0")
	st.AppendEntry(ctx, ns, "recurrence", map[string]string{"fingerprint": "a"}, "r0")
	mf, me, err := st.LedgerMark(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.NewInput(ctx, store.Input{Key: "live-1", Routine: ns, Kind: store.InputTick, StartedAt: "2026-11-19T10:00:00.000000000Z", MarkFilings: mf, MarkEntries: me}); err != nil {
		t.Fatal(err)
	}
	// the live run writes after the mark
	st.WriteFiling(ctx, ns, map[string]string{"fingerprint": "b", "repo": "o/r", "issue": "2"}, "live-1")
	st.AppendEntry(ctx, ns, "decision", map[string]string{"fingerprint": "b"}, "live-1")
	in, _ := st.Input(ctx, "live-1")
	e, _ := FromStore(in)
	if err := WithLedger(ctx, st, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Ledger.Filings) != 1 || e.Ledger.Filings[0]["fingerprint"] != "a" || len(e.Ledger.Entries) != 2 ||
		e.Ledger.Entries[0].Kind != "decision" || e.Ledger.Entries[1].Kind != "recurrence" {
		t.Fatalf("%+v", e.Ledger)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	// without marks there is nothing to read as of
	bare := Envelope{SchemaVersion: 1, InputKey: "k", Routine: ns, Kind: "tick"}
	if err := WithLedger(ctx, st, &bare); err == nil {
		t.Fatal("an input with no ledger marks has no ledger as of its start")
	}
}

func TestCheckNamesWhatIsWrong(t *testing.T) {
	e := Envelope{SchemaVersion: 2, InputKey: "", Routine: "Bad", Kind: "weekly", Commit: &Commit{Repo: "", SHA: "zz"}}
	err := e.Validate()
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"schema_version", "input_key", "routine", "kind", "commit.repo", "commit.sha"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// ---- the export folder

func TestAnExportFolderOutsideARepoIsWrittenPrivately(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "exports", "in-1")
	st := &StateSnapshot{Paths: []string{"state"}}
	e := Envelope{SchemaVersion: 1, InputKey: "in-1", Routine: "triage/cycle", Kind: "tick", StateSnapshot: st}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := WriteFolder(dir, e, strings.NewReader("tar bytes"), now); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"": 0o700, EnvelopeFile: 0o600, StateFile: 0o600, ReadmeFile: 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%q is %o, want %o", name, info.Mode().Perm(), want)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, EnvelopeFile))
	back, err := Decode(raw)
	if err != nil || back.StateSnapshot.Tar != StateFile {
		t.Fatalf("the envelope names its archive: %+v %v", back, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, StateFile)); string(b) != "tar bytes" {
		t.Fatalf("the archive: %q", b)
	}
	readme, _ := os.ReadFile(filepath.Join(dir, ReadmeFile))
	for _, want := range []string{"PRODUCTION DATA", "in-1", "triage/cycle", "2026-11-19T12:00:00Z", "outside any repository", "state.tar.gz"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("the README lacks %q: %s", want, readme)
		}
	}
}

func TestAnExportRefusesAFolderInsideAGitWorkTree(t *testing.T) {
	e := Envelope{SchemaVersion: 1, InputKey: "in-1", Routine: "triage/cycle", Kind: "tick"}
	// a repository, a worktree (.git is a file), and a folder two levels down in each
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"the repo root":          repo,
		"a folder in the repo":   filepath.Join(repo, "docs", "exports"),
		"a worktree root":        wt,
		"a folder in a worktree": filepath.Join(wt, "a", "b", "c"),
	} {
		err := WriteFolder(dir, e, nil, time.Now())
		if err == nil || !strings.Contains(err.Error(), "inside a git work tree") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, EnvelopeFile)); err == nil {
			t.Errorf("%s: a file was written", name)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "docs")); err == nil {
		t.Error("a refused export created folders in the repository")
	}
}

func TestAnExportFollowsASymlinkBeforeJudgingTheFolder(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(outside, "innocent")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	e := Envelope{SchemaVersion: 1, InputKey: "in-1", Routine: "triage/cycle", Kind: "tick"}
	if err := WriteFolder(filepath.Join(link, "out"), e, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "inside a git work tree") {
		t.Fatalf("a link into a repository is a folder in it: %v", err)
	}
}

func TestAnExportRefusesWhatItWouldMixWith(t *testing.T) {
	e := Envelope{SchemaVersion: 1, InputKey: "in-1", Routine: "triage/cycle", Kind: "tick"}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "something"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFolder(dir, e, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a folder with something in it: %v", err)
	}
	if err := WriteFolder("", e, nil, time.Now()); err == nil {
		t.Fatal("no folder")
	}
	// an empty existing folder is fine, and ends up 0700
	empty := filepath.Join(t.TempDir(), "e")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFolder(empty, e, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(empty); info.Mode().Perm() != 0o700 {
		t.Errorf("the folder is %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(empty, StateFile)); err == nil {
		t.Error("a state archive with no state")
	}
}

func TestAnInvalidEnvelopeWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	if err := WriteFolder(dir, Envelope{SchemaVersion: 1, Kind: "tick"}, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the folder was created for an invalid envelope")
	}
	e := Envelope{SchemaVersion: 1, InputKey: "k", Routine: "a/b", Kind: "tick"}
	if err := WriteFolder(dir, e, strings.NewReader("x"), time.Now()); err == nil {
		t.Fatal("a state archive for an envelope with no state paths")
	}
}

type brokenReader struct{ sent bool }

func (b *brokenReader) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "half an archive"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

// A state stream that breaks leaves none of the production data behind: not the half archive, not the envelope, not the folder
// the export made (an existing empty folder stays, as the operator made it).
func TestAFailedExportLeavesNothingBehind(t *testing.T) {
	e := Envelope{SchemaVersion: 1, InputKey: "k", Routine: "a/b", Kind: "tick", StateSnapshot: &StateSnapshot{Paths: []string{"state"}}}
	made := filepath.Join(t.TempDir(), "new")
	if err := WriteFolder(made, e, &brokenReader{}, time.Now()); err == nil {
		t.Fatal("a broken stream was written as whole")
	}
	if _, err := os.Stat(made); err == nil {
		t.Fatal("the folder the export made is still there")
	}
	existing := filepath.Join(t.TempDir(), "mine")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFolder(existing, e, &brokenReader{}, time.Now()); err == nil {
		t.Fatal("accepted")
	}
	if left, _ := os.ReadDir(existing); len(left) != 0 {
		t.Fatalf("files left in the operator's folder: %v", left)
	}
	if e.StateSnapshot.Tar != "" {
		t.Fatal("the caller's envelope was changed")
	}
}
