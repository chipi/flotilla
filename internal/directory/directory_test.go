package directory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

func write(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "people.yaml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFile(t *testing.T) {
	p := write(t, `people:
  - email: Ada@Example.org
    name: Ada Lovelace
    ids: {slack: U0ADA, github: ada}
  - email: grace@example.org
    name: Grace Hopper
`, 0o600)
	people, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 || people[0].Email != "ada@example.org" || people[0].IDs["github"] != "ada" || len(people[1].IDs) != 0 {
		t.Fatalf("%+v", people)
	}
}

func TestReadFileRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
		want string
	}{
		"group readable": {"people: []\n", 0o640, "readable by its owner only"},
		"no name":        {"people:\n  - email: a@x.org\n", 0o600, "needs an email and a name"},
		"no email":       {"people:\n  - name: A\n", 0o600, "needs an email and a name"},
		"twice":          {"people:\n  - {email: a@x.org, name: A}\n  - {email: A@x.org, name: B}\n", 0o600, "listed twice"},
		"shared id":      {"people:\n  - {email: a@x.org, name: A, ids: {slack: U1}}\n  - {email: b@x.org, name: B, ids: {slack: U1}}\n", 0o600, "both a@x.org's and b@x.org's"},
		"bad tool":       {"people:\n  - {email: a@x.org, name: A, ids: {Slack: U1}}\n", 0o600, "short lowercase word"},
		"unknown field":  {"people:\n  - {email: a@x.org, name: A, role: admin}\n", 0o600, "field role not found"},
	} {
		_, err := ReadFile(write(t, c.body, c.mode))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// RFC-0014 §7: a sync adding, changing and removing a person, each change in the ledger; a removed person resolves to
// "unknown" on the next lookup; an id that moves between two people in one sync.
func TestSyncAddChangeRemove(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	actor := Actor("file")
	ada := store.Person{Email: "ada@example.org", Name: "Ada", IDs: map[string]string{"slack": "U0ADA"}}
	grace := store.Person{Email: "grace@example.org", Name: "Grace", IDs: map[string]string{"github": "grace"}}
	ch, err := st.SyncPeople(ctx, []store.Person{ada, grace}, actor)
	if err != nil || len(ch) != 2 || ch[0].Kind != "person.added" {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch, _ := st.SyncPeople(ctx, []store.Person{ada, grace}, actor); len(ch) != 0 {
		t.Fatalf("an unchanged sync made changes: %+v", ch)
	}
	if p, ok, _ := st.PersonBy(ctx, "slack", "U0ADA"); !ok || p.Name != "Ada" {
		t.Fatalf("the Slack id does not resolve to Ada: %+v %v", p, ok)
	}
	ada.Name = "Ada Lovelace"
	ada.IDs = map[string]string{"slack": "U0ADA", "github": "ada"}
	ch, err = st.SyncPeople(ctx, []store.Person{ada}, actor)
	if err != nil || len(ch) != 2 {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch[0].Kind != "person.changed" || ch[0].Detail != `name "Ada" -> "Ada Lovelace"; github ada added` {
		t.Errorf("change: %+v", ch[0])
	}
	if ch[1].Kind != "person.removed" || ch[1].Email != "grace@example.org" {
		t.Errorf("removal: %+v", ch[1])
	}
	if _, ok, _ := st.PersonBy(ctx, "github", "grace"); ok {
		t.Error("a removed person still resolves")
	}
	ev, err := st.Events(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ev {
		if strings.HasPrefix(e.Kind, "person.") && e.Actor == actor {
			n++
		}
	}
	if n != 4 {
		t.Errorf("%d person events in the ledger, want 4 (two added, one changed, one removed)", n)
	}
	// The Slack id moves from Ada to Grace in one sync.
	ada.IDs = map[string]string{"github": "ada"}
	grace.IDs = map[string]string{"slack": "U0ADA"}
	if _, err := st.SyncPeople(ctx, []store.Person{ada, grace}, actor); err != nil {
		t.Fatalf("an id that moved: %v", err)
	}
	if p, ok, _ := st.PersonBy(ctx, "slack", "U0ADA"); !ok || p.Email != "grace@example.org" {
		t.Errorf("the moved id resolves to %+v", p)
	}
}

func TestMissing(t *testing.T) {
	people := []store.Person{{Email: "a@x.org", Name: "A", IDs: map[string]string{"slack": "U1"}}}
	if got := Missing(people, "slack", []string{"U1", "U2"}); len(got) != 1 || got[0] != "U2" {
		t.Errorf("%v", got)
	}
}
