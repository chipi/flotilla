// Package runtoken is the per-run credential (v2 step 2, design section 4): flotillad gives every run its own token at dispatch,
// and every request the run makes to flotillad is resolved token, run, then the routine, version and shadow flag recorded on the
// run when it started. The routine or ledger namespace a request names is ignored, so no run can write under another routine's
// or version's name, and a shadow run's token can only ever name the shadow's own key.
//
// Nothing here is persisted: flotillad holds only each token's SHA-256, in memory, from before the worker is asked to start the
// run until it ends (a restart orphans every run, RecoverInterrupted cancels them, so no token outlives its flotillad).
package runtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Run is what a token stands for, fixed when it is issued. Promote, roll back and retire never change it: a run finishes as the
// version it started as.
type Run struct {
	ID       string // the run's id
	Routine  string // the live routine id, "<fleet>/<routine>"
	Version  int
	Worker   string // the worker the run was sent to
	Shadow   bool   // a shadow run: its rows are keyed Key(), never the live routine's
	InputKey string // a shadow's input: the live run whose input it was given ("" for a live run)
}

// Key is the routine key every row of the run is written under: the live routine's id, or "<id>@v<n>" for a shadow.
func (r Run) Key() string {
	if r.Shadow {
		return fmt.Sprintf("%s@v%d", r.Routine, r.Version)
	}
	return r.Routine
}

// Registry holds the tokens of the runs in flight.
type Registry struct {
	mu    sync.RWMutex
	byTok map[[sha256.Size]byte]Run
	byRun map[string][sha256.Size]byte
}

func New() *Registry {
	return &Registry{byTok: map[[sha256.Size]byte]Run{}, byRun: map[string][sha256.Size]byte{}}
}

// ErrIssued: a run has one token; asking for a second is a bug in the caller.
var ErrIssued = errors.New("this run already has a token")

// Issue makes the run's token: 32 random bytes, hex. It is shown once, to the caller, and only its hash is kept.
func (r *Registry) Issue(run Run) (string, error) {
	if run.ID == "" || run.Routine == "" {
		return "", errors.New("a run token needs the run's id and its routine")
	}
	if strings.Contains(run.Routine, "@") {
		return "", fmt.Errorf("a run token names the live routine id, not a shadow key (%q)", run.Routine)
	}
	if run.Version < 1 {
		return "", errors.New("a run token needs the run's version")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	h := sha256.Sum256([]byte(tok))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byRun[run.ID]; ok {
		return "", fmt.Errorf("%w (%s)", ErrIssued, run.ID)
	}
	r.byTok[h] = run
	r.byRun[run.ID] = h
	return tok, nil
}

// Resolve is the run a token was issued for, false for a token that was never issued or has been revoked.
func (r *Registry) Resolve(token string) (Run, bool) {
	if token == "" {
		return Run{}, false
	}
	h := sha256.Sum256([]byte(token))
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.byTok[h]
	return run, ok
}

// Revoke ends a run's token. A run with none (or one already revoked) is fine.
func (r *Registry) Revoke(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.byRun[runID]; ok {
		delete(r.byTok, h)
		delete(r.byRun, runID)
	}
}

// Len is how many tokens are live.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byTok)
}
