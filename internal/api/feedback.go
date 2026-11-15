package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

// G8: a person's verdict on a case's diagnosis feeds the record of the action it planned; a note they
// propose waits on Knowledge until someone activates it.

const maxNote = 2000

// SetVerdict records the caller's verdict (right or wrong) on a case's diagnosis.
func (s *Server) SetVerdict(ctx context.Context, c Caller, via, caseID, verdict string) error {
	if err := s.Check(ctx, c, ActFeedback); err != nil {
		return err
	}
	if _, err := s.Store.Case(ctx, caseID); errors.Is(err, store.ErrNoSuchCase) {
		return problem(404, "no such case")
	} else if err != nil {
		return err
	}
	if verdict != store.VerdictRight && verdict != store.VerdictWrong {
		return problem(400, "a verdict is right or wrong")
	}
	if err := s.Store.SetVerdict(ctx, caseID, strings.TrimPrefix(c.Name, "user:"), verdict); err != nil {
		return err
	}
	s.Log.Info("case verdict", "case", caseID, "verdict", verdict, "by", c.Name, "via", via)
	return nil
}

// ProposeNote keeps a note from a case, scoped to its kind, as proposed: it takes effect only once activated.
func (s *Server) ProposeNote(ctx context.Context, c Caller, via, caseID, text string) (int64, error) {
	if err := s.Check(ctx, c, ActNotePropose); err != nil {
		return 0, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, problem(400, "a note needs its text")
	}
	if len([]rune(text)) > maxNote {
		return 0, problem(400, "a note is at most %d characters", maxNote)
	}
	cs, err := s.Store.Case(ctx, caseID)
	if errors.Is(err, store.ErrNoSuchCase) {
		return 0, problem(404, "no such case")
	} else if err != nil {
		return 0, err
	}
	id, err := s.Store.ProposeNote(ctx, "kind:"+cs.Kind, text, caseID, strings.TrimPrefix(c.Name, "user:"))
	if err == nil {
		// on the case's timeline too, so the case shows it and a review routine sees what it proposed
		short := []rune(text)
		if len(short) > 120 {
			short = append(short[:120], '…')
		}
		_ = s.Store.AddCaseEvent(ctx, caseID, "note.proposed", c.Name, fmt.Sprintf("note %d: %s", id, string(short)))
	}
	if err != nil {
		return 0, err
	}
	s.Log.Info("note proposed", "note", id, "case", caseID, "by", c.Name, "via", via)
	return id, nil
}

// DecideNote activates or drops a note.
func (s *Server) DecideNote(ctx context.Context, c Caller, via string, id int64, to string) error {
	if err := s.Check(ctx, c, ActNotes); err != nil {
		return err
	}
	if err := s.Store.DecideNote(ctx, id, to, strings.TrimPrefix(c.Name, "user:")); err != nil {
		return problem(409, "%s", err.Error())
	}
	s.Log.Info("note decided", "note", id, "to", to, "by", c.Name, "via", via)
	return nil
}
