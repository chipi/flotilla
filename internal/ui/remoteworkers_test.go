package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
)

// A remote worker refused for its version (2026-11-23: the bugfix workers on sha-52bfbb9 after flotillad moved to sha-ff08c18, for
// hours, with nothing on screen) is wrong right now, and its Hosts row says why; a connected one is neither.
func TestARefusedRemoteWorkerIsWrongRightNow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, h := range []string{"w-bugfix-omarchy", "w-bugfix-act-omarchy"} {
		if _, err := f.st.AddEnrolment(ctx, h, "t-"+h, "user:ada", "test"); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Enrol(ctx, h, "t-"+h, func() (string, time.Time, error) { return "c-" + h, time.Now().Add(720 * time.Hour), nil }); err != nil {
			t.Fatal(err)
		}
	}
	f.ui.API.Remote = func() []api.RemoteWorker {
		return []api.RemoteWorker{
			{Worker: "bugfix", Identity: "w-bugfix-omarchy", Refused: "version sha-52bfbb9, not flotillad's sha-ff08c18"},
			{Worker: "bugfix-act", Identity: "w-bugfix-act-omarchy", Version: "test", Connected: true},
		}
	}
	routines, workers, err := f.ui.API.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.ui.wrongNow(ctx, time.Now(), routines, workers)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range w.Items {
		if strings.HasPrefix(x.What, "worker ") {
			got = append(got, x.What+": "+x.Detail)
		}
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "worker bugfix is refused: ") || !strings.Contains(got[0], "sha-52bfbb9") {
		t.Fatalf("wrong right now: %q", got)
	}
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/hosts", ck, nil, nil)
	if !strings.Contains(page, "worker link refused") || !strings.Contains(page, "version sha-52bfbb9, not flotillad") || !strings.Contains(page, "worker link bugfix-act, connected") {
		t.Fatal("the Hosts page does not say which link is refused and which is connected")
	}
}
