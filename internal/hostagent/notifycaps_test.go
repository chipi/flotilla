package hostagent

import (
	"errors"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/hostwire"
)

// notifyBefore is NotifyBody as the agents before notices decode it, strictly.
type notifyBefore struct {
	Nonce      string `json:"nonce"`
	Case       string `json:"case"`
	Title      string `json:"title"`
	Plan       string `json:"plan"`
	PlanHash   string `json:"plan_hash"`
	URL        string `json:"url"`
	Approvable bool   `json:"approvable"`
	Why        string `json:"why,omitempty"`
}

// rawAgent connects as host-01 with the given hello capabilities and reads what flotillad sends it.
func rawAgent(t *testing.T, w world, caps []string) func() hostwire.Frame {
	t.Helper()
	a := w.agent(t, "host-01", &fakeExec{})
	chain, err := a.LoadChain()
	if err != nil {
		t.Fatal(err)
	}
	c := rawDial(t, w, chain, a.Key)
	if err := hostwire.Write(c, hostwire.Hello, hostwire.HelloBody{Version: "x", Clock: time.Now(), Caps: caps}); err != nil {
		t.Fatal(err)
	}
	if f, err := hostwire.Read(c); err != nil || f.Type != hostwire.Welcome {
		t.Fatalf("welcome: %v %v", f.Type, err)
	}
	waitFor(t, "the raw agent to connect", connected(w, "host-01"))
	return func() hostwire.Frame {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := hostwire.Read(c)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
}

var question = hostwire.NotifyBody{Nonce: "n", Case: "c", Title: "t", Plan: "p", PlanHash: "h", URL: "u", Approvable: true, Kind: hostwire.KindPlan,
	Answers: []hostwire.NotifyAnswer{{ID: "approve", Label: "Approve", Allowed: true}}}

// An agent that does not say it reads answers (one from before them) is sent plans in the old shape, which its strict
// decoding accepts, and is sent no notice: flotillad is told so, and marks nothing sent.
func TestAnAgentWithoutAnswersGetsPlansInTheOldShapeAndNoNotices(t *testing.T) {
	w := newWorld(t)
	next := rawAgent(t, w, nil)
	if err := w.link.Notify("host-01", question); err != nil {
		t.Fatal(err)
	}
	var old notifyBefore
	if f := next(); f.Type != hostwire.Notify || f.Decode(&old) != nil || old.Nonce != "n" || !old.Approvable {
		t.Fatalf("the plan an older agent was sent: %+v", f)
	}
	notice := question
	notice.Kind = hostwire.KindNotice
	if err := w.link.Notify("host-01", notice); !errors.Is(err, hostlink.ErrNotSupported) {
		t.Fatalf("a notice to an agent that cannot read it: %v", err)
	}
}

// An agent that says it reads answers is sent the whole body.
func TestAnAgentWithAnswersGetsTheWholeBody(t *testing.T) {
	w := newWorld(t)
	next := rawAgent(t, w, []string{hostwire.CapNotifyAnswers})
	notice := question
	notice.Kind, notice.Repeat = hostwire.KindNotice, true
	if err := w.link.Notify("host-01", notice); err != nil {
		t.Fatal(err)
	}
	var got hostwire.NotifyBody
	if f := next(); f.Type != hostwire.Notify || f.Decode(&got) != nil || got.Kind != hostwire.KindNotice || !got.Repeat || len(got.Answers) != 1 {
		t.Fatalf("the notice a capable agent was sent: %+v", got)
	}
}

// The agent reads a notify body with fields it does not know (a newer flotillad), and the other frames stay strict.
func TestAgentReadsANotifyBodyLeniently(t *testing.T) {
	f := hostwire.Frame{Type: hostwire.Notify, Body: []byte(`{"nonce":"n","case":"c","title":"t","plan":"p","plan_hash":"","url":"u","approvable":false,"from_the_future":1}`)}
	var n hostwire.NotifyBody
	if err := f.DecodeLenient(&n); err != nil || n.Nonce != "n" {
		t.Fatalf("lenient: %+v %v", n, err)
	}
	if err := f.Decode(&n); err == nil {
		t.Fatal("the strict decoding accepted an unknown field")
	}
}
