package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/chipi/flotilla/internal/input"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// Frozen inputs (v2 step 2): what a live run was handed, kept so its shadows, a replay or an eval can be given exactly that. An
// input holds production data (a case's contents, the routine's ledger rows, its state), so reading one, pinning one and exporting
// one are an operator's or an admin's (ActInput), never an agent's and never through MCP, and each read of the rows or the state
// is an event in the log with who made it.

// MaxStateExport bounds what an export streams from a worker: the worker's own snapshot cap, with room for the archive.
const MaxStateExport = 512 << 20

func (s *Server) inputRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/inputs/{key}", s.auth(ActInput, s.inputShow))
	m.Handle("GET /v1/inputs/{key}/state", s.auth(ActInput, s.inputState))
	m.Handle("POST /v1/inputs/{key}/pin", s.auth(ActInput, s.inputPin))
	m.Handle("POST /v1/inputs/{key}/unpin", s.auth(ActInput, s.inputPin))
}

// InputView is a recorded input as an operator reads it: the envelope (with the ledger rows as of its mark when asked), what is
// held for it, and what its shadows did.
type InputView struct {
	Envelope      input.Envelope    `json:"envelope"`
	SnapshotState string            `json:"snapshot_state"`
	Pinned        bool              `json:"pinned"`
	ShadowRuns    []store.ShadowRun `json:"shadow_runs"`
}

// InputOf reads one input; withLedger adds the rows of the routine's ledger as of the input's marks (production data: it is an
// event, `input.export`, naming who read it).
func (s *Server) InputOf(ctx context.Context, c Caller, via, key string, withLedger bool) (InputView, error) {
	var out InputView
	if err := s.Check(ctx, c, ActInput); err != nil {
		return out, err
	}
	in, err := s.Store.Input(ctx, key)
	if errors.Is(err, store.ErrNoSuchInput) {
		return out, problem(404, "no such input")
	}
	if err != nil {
		return out, err
	}
	e, err := input.FromStore(in)
	if err != nil {
		return out, err
	}
	if withLedger {
		if err := input.WithLedger(ctx, s.Store, &e); err != nil {
			return out, err
		}
		if err := s.Store.Event(ctx, c.Name, via, "input.export", key, "the ledger rows as of the input's marks"); err != nil {
			return out, err
		}
	}
	runs, err := s.Store.ShadowRuns(ctx, store.ShadowRunFilter{InputKey: key})
	if err != nil {
		return out, err
	}
	if runs == nil {
		runs = []store.ShadowRun{}
	}
	return InputView{Envelope: e, SnapshotState: in.SnapshotState, Pinned: in.Pinned, ShadowRuns: runs}, nil
}

func (s *Server) inputShow(w http.ResponseWriter, r *http.Request, c Caller) {
	v, err := s.InputOf(r.Context(), c, "api", r.PathValue("key"), r.URL.Query().Get("ledger") == "1")
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	writeJSON(w, 200, v)
}

// PinInput keeps (pin) or stops keeping (unpin) an input's state snapshot past the end of its shadow runs, so an export can
// still read it. One event, with who asked.
func (s *Server) PinInput(ctx context.Context, c Caller, via, key string, pin bool) error {
	if err := s.Check(ctx, c, ActInput); err != nil {
		return err
	}
	if err := s.Store.PinInput(ctx, key, pin); err != nil {
		if errors.Is(err, store.ErrNoSuchInput) {
			return problem(404, "no such input")
		}
		return err
	}
	kind := "input.unpin"
	if pin {
		kind = "input.pin"
	}
	if err := s.Store.Event(ctx, c.Name, via, kind, key, ""); err != nil {
		return err
	}
	if !pin {
		s.Sched.ReleaseSnapshot(ctx, key) // nothing waits for it and nothing pins it: the snapshot goes
	}
	return nil
}

func (s *Server) inputPin(w http.ResponseWriter, r *http.Request, c Caller) {
	pin := r.URL.Path[len(r.URL.Path)-len("/pin"):] == "/pin"
	err := s.PinInput(r.Context(), c, "api", r.PathValue("key"), pin)
	writeAction(w, err, 200, map[string]string{"input": r.PathValue("key"), "pinned": fmt.Sprint(pin)})
}

// StateOf streams the state snapshot the live worker holds for an input, a tar.gz, into w. Only a snapshot that is held: one
// deleted when its shadows ended is gone, and the way to keep one is to pin the input first. A remote worker's is not reachable
// from here.
func (s *Server) StateOf(ctx context.Context, c Caller, via, key string, w io.Writer) (int64, error) {
	if err := s.Check(ctx, c, ActInput); err != nil {
		return 0, err
	}
	in, err := s.Store.Input(ctx, key)
	if errors.Is(err, store.ErrNoSuchInput) {
		return 0, problem(404, "no such input")
	}
	if err != nil {
		return 0, err
	}
	if in.SnapshotState != store.SnapshotHeld {
		return 0, problem(409, "the input's state snapshot is %s: pin an input before its shadows end to keep it", in.SnapshotState)
	}
	j, err := s.routineByID(in.Routine)
	if err != nil {
		return 0, err
	}
	wc := s.Workers[j.Worker]
	if wc == nil {
		return 0, problem(409, "%s runs on a worker flotillad has no direct line to (a remote worker): its snapshot is not reachable from here", in.Routine)
	}
	// the event first: production data leaving is on the record even if the stream then fails
	if err := s.Store.Event(ctx, c.Name, via, "input.export", key, "the state snapshot"); err != nil {
		return 0, err
	}
	n, err := wc.FetchSnapshot(ctx, key, w, MaxStateExport)
	switch {
	case errors.Is(err, worker.ErrNoSuchSnapshot):
		return n, problem(404, "the worker no longer has that snapshot")
	case err != nil:
		return n, err
	}
	return n, nil
}

// archiveWriter sends the archive as the worker streams it: its headers go out with the first byte, so a refusal before any byte is a
// plain JSON error, and a failure after the first one aborts the connection (the client sees a broken download, never a short
// archive that looks whole).
type archiveWriter struct {
	w http.ResponseWriter
	n int64
}

func (a *archiveWriter) Write(b []byte) (int, error) {
	if a.n == 0 && len(b) > 0 {
		a.w.Header().Set("Content-Type", "application/gzip")
	}
	n, err := a.w.Write(b)
	a.n += int64(n)
	return n, err
}

func (s *Server) inputState(w http.ResponseWriter, r *http.Request, c Caller) {
	aw := &archiveWriter{w: w}
	if _, err := s.StateOf(r.Context(), c, "api", r.PathValue("key"), aw); err != nil {
		if aw.n > 0 {
			s.Log.Error("an input's state export failed part-way", "input", r.PathValue("key"), "caller", c.Name, "err", err)
			panic(http.ErrAbortHandler)
		}
		writeAction(w, err, 0, nil)
	}
}
