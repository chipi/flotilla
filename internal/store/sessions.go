package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Sessions (ADR-0014): the cookie carries a random id; only its hash is stored. A session lasts 30 days
// from sign-in and ends after 7 days without use. The user's role is read on every lookup, so a
// demotion or revocation takes effect at once.
const (
	SessionMaxAge  = 30 * 24 * time.Hour
	SessionMaxIdle = 7 * 24 * time.Hour
	touchEvery     = time.Minute // last_seen is written at most this often
)

type Session struct {
	User string
	Role string
	CSRF string
}

// StartSession records a new session for an active user, and the sign-in event.
func (s *Store) StartSession(ctx context.Context, idSHA256, user, csrf string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := activeRole(ctx, tx, user); err != nil {
		return err
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id_sha256, user_name, csrf, created_at, last_seen) VALUES (?, ?, ?, ?, ?)`,
		idSHA256, user, csrf, at, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, "user:"+user, "ui", "user.signed-in", "user:"+user, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionByID returns the live session for a cookie id's hash, or nil when there is none, it is too old
// or idle, or its user is revoked. An expired session is deleted on the way.
func (s *Store) SessionByID(ctx context.Context, idSHA256 string) (*Session, error) {
	var x Session
	var created, seen string
	err := s.db.QueryRowContext(ctx, `SELECT s.user_name, u.role, s.csrf, s.created_at, s.last_seen
		FROM sessions s JOIN users u ON u.name = s.user_name
		WHERE s.id_sha256 = ? AND u.revoked_at IS NULL`, idSHA256).Scan(&x.User, &x.Role, &x.CSRF, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	c, err1 := time.Parse(ts, created)
	l, err2 := time.Parse(ts, seen)
	if err1 != nil || err2 != nil || now.Sub(c) > SessionMaxAge || now.Sub(l) > SessionMaxIdle {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_sha256 = ?`, idSHA256)
		return nil, nil
	}
	if now.Sub(l) > touchEvery {
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE id_sha256 = ?`, now.Format(ts), idSHA256)
	}
	return &x, nil
}

// EndSession ends one session (signing out on this device).
func (s *Store) EndSession(ctx context.Context, idSHA256 string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_sha256 = ?`, idSHA256)
	return err
}

// EndSessions ends every session of a user and records who did it (signing out everywhere, or an
// admin ending someone's sessions). It returns how many ended.
func (s *Store) EndSessions(ctx context.Context, user, actor, via string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_name = ?`, user)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	kind := "user.sessions-ended"
	if actor == "user:"+user {
		kind = "user.signed-out-everywhere"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		s.now().Format(ts), actor, via, kind, "user:"+user, ""); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// PruneSessions deletes sessions past either limit; flotillad runs it with its daily backup.
func (s *Store) PruneSessions(ctx context.Context) (int64, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE created_at < ? OR last_seen < ?`,
		now.Add(-SessionMaxAge).Format(ts), now.Add(-SessionMaxIdle).Format(ts))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
