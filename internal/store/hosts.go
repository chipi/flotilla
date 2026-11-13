package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// EnrolmentValidity is how long an enrolment token can be used (ADR-0006 §3).
const EnrolmentValidity = 15 * time.Minute

var (
	ErrEnrolment  = errors.New("the enrolment token is unknown, used, expired or for another host")
	ErrNoSuchHost = errors.New("no such host")
	ErrNotCurrent = errors.New("not the host's current certificate")
)

type Host struct {
	Name         string  `json:"name"`
	CertSHA256   string  `json:"cert_sha256"` // the current certificate's hostca.Fingerprint
	CertNotAfter string  `json:"cert_not_after"`
	CreatedAt    string  `json:"created_at"`
	EnrolledAt   *string `json:"enrolled_at"`
	RevokedAt    *string `json:"revoked_at"`
	// what flotillad last heard from the host's agent
	Connected     bool   `json:"connected"`
	Version       string `json:"version"`
	ConnectedAt   string `json:"connected_at"`
	LastContact   string `json:"last_contact"`
	ClockOffsetMS int64  `json:"clock_offset_ms"` // the agent's clock minus flotillad's
	ChainHead     string `json:"chain_head"`
}

// HostContact is what flotillad heard from a host's agent (internal/hostlink).
type HostContact struct {
	Host        string
	Connected   bool
	Version     string
	ConnectedAt time.Time
	LastContact time.Time
	ClockOffset time.Duration
	ChainHead   string
}

// RecordHostContact keeps a host's connection state; a host flotillad never enrolled is not added.
func (s *Store) RecordHostContact(ctx context.Context, c HostContact) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET connected = ?, version = ?, connected_at = ?, last_contact = ?,
		clock_offset_ms = ?, chain_head = ? WHERE name = ?`, c.Connected, c.Version, c.ConnectedAt.UTC().Format(ts),
		c.LastContact.UTC().Format(ts), c.ClockOffset.Milliseconds(), c.ChainHead, c.Host)
	return err
}

// DisconnectHosts marks every host not connected: no connection outlives flotillad.
func (s *Store) DisconnectHosts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET connected = 0`)
	return err
}

func event(ctx context.Context, tx *sql.Tx, at, actor, via, kind, subject, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, kind, subject, detail)
	return err
}

// AddEnrolment records an enrolment token's hash for a host, valid EnrolmentValidity, and the host if
// it is new. Enrolling a revoked host again takes a new token: that is how it comes back.
func (s *Store) AddEnrolment(ctx context.Context, host, tokenSHA256, actor, via string) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	now := s.now()
	at, until := now.Format(ts), now.Add(EnrolmentValidity)
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosts (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`, host, at); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO host_enrolments (token_sha256, host, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tokenSHA256, host, actor, at, until.Format(ts)); err != nil {
		return time.Time{}, err
	}
	if err := event(ctx, tx, at, actor, via, "host.enrolment", "host:"+host, "token valid until "+until.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	return until, tx.Commit()
}

// Enrol spends an enrolment token for host and records the certificate issue made for it, in one
// transaction: issue runs only for a token that is good, and the token is spent only if it succeeds.
// The new certificate becomes the host's only accepted one; a revoked host is no longer revoked.
func (s *Store) Enrol(ctx context.Context, host, tokenSHA256 string, issue func() (cert string, notAfter time.Time, err error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	at := now.Format(ts)
	res, err := tx.ExecContext(ctx, `UPDATE host_enrolments SET used_at = ? WHERE token_sha256 = ? AND host = ? AND used_at IS NULL AND expires_at > ?`,
		at, tokenSHA256, host, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrEnrolment
	}
	cert, notAfter, err := issue()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_sha256 = ?, cert_not_after = ?, enrolled_at = ?, revoked_at = NULL WHERE name = ?`,
		cert, notAfter.UTC().Format(ts), at, host); err != nil {
		return err
	}
	if err := event(ctx, tx, at, "host:"+host, "enrolment", "host.enrolled", "host:"+host, "certificate "+cert); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeHost clears a host's certificate: flotillad, the only verifier, refuses it from now on.
func (s *Store) RevokeHost(ctx context.Context, host, actor, via string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	var cert string
	if err := tx.QueryRowContext(ctx, `SELECT cert_sha256 FROM hosts WHERE name = ?`, host).Scan(&cert); errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchHost
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_sha256 = '', cert_not_after = '', revoked_at = ? WHERE name = ?`, at, host); err != nil {
		return err
	}
	// its unused enrolment tokens go with it
	if _, err := tx.ExecContext(ctx, `UPDATE host_enrolments SET used_at = ? WHERE host = ? AND used_at IS NULL`, at, host); err != nil {
		return err
	}
	if err := event(ctx, tx, at, actor, via, "host.revoked", "host:"+host, "certificate "+cert); err != nil {
		return err
	}
	return tx.Commit()
}

// HostCertCurrent reports whether cert (a hostca.Fingerprint, never a serial: serials are no secret) is
// host's current certificate. Revoking clears it, so a revoked host has none.
func (s *Store) HostCertCurrent(ctx context.Context, host, cert string) (bool, error) {
	if cert == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE name = ? AND cert_sha256 = ?`,
		host, cert).Scan(&n)
	return n == 1, err
}

// Hosts lists every host.
func (s *Store) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, cert_sha256, cert_not_after, created_at, enrolled_at, revoked_at,
		connected, version, connected_at, last_contact, clock_offset_ms, chain_head FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hs := []Host{}
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.Name, &h.CertSHA256, &h.CertNotAfter, &h.CreatedAt, &h.EnrolledAt, &h.RevokedAt,
			&h.Connected, &h.Version, &h.ConnectedAt, &h.LastContact, &h.ClockOffsetMS, &h.ChainHead); err != nil {
			return nil, err
		}
		hs = append(hs, h)
	}
	return hs, rows.Err()
}

// RenewHost records host's next certificate in place of the current one, oldCert, which must still be
// current: a renewal on a connection made with a replaced or revoked certificate is refused. Both are
// hostca fingerprints.
func (s *Store) RenewHost(ctx context.Context, host, oldCert, cert string, notAfter time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	res, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_sha256 = ?, cert_not_after = ? WHERE name = ? AND cert_sha256 = ? AND cert_sha256 <> ''`,
		cert, notAfter.UTC().Format(ts), host, oldCert)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotCurrent
	}
	if err := event(ctx, tx, at, "host:"+host, "connection", "host.renewed", "host:"+host, oldCert+" -> "+cert); err != nil {
		return err
	}
	return tx.Commit()
}

// Statuses of an execution flotillad sent to a host, besides the host's own (hostexec: done, failed,
// unknown, precondition_false, refused).
const (
	ExecSent    = "sent"     // on its way, or its result not yet back
	ExecNotSent = "not_sent" // the host was not connected: nothing reached it, and it may be sent again
)

var ErrExecutionExists = errors.New("this execution was already sent")

// HostExecution is flotillad's record of one signed execution: the envelope it sent, and what came back.
type HostExecution struct {
	ID           string  `json:"id"`
	CaseID       string  `json:"case_id"`
	PlanID       int64   `json:"plan_id"`
	Host         string  `json:"host"`
	Action       string  `json:"action"`
	Approved     string  `json:"approved"`
	Envelope     []byte  `json:"-"`
	Status       string  `json:"status"`
	ExitCode     int     `json:"exit_code"`
	OutputSHA256 string  `json:"output_sha256"`
	Detail       string  `json:"detail"`
	ChainHead    string  `json:"chain_head"`
	SentAt       string  `json:"sent_at"`
	EndedAt      *string `json:"ended_at"`
}

// StartHostExecution records an execution as sent, before it goes. An id seen before is refused unless
// its last attempt reached no one (not_sent): then this attempt, with its new envelope, replaces it.
func (s *Store) StartHostExecution(ctx context.Context, x HostExecution) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO host_executions (id, case_id, plan_id, host, action, approved, envelope, status, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET envelope = excluded.envelope, status = excluded.status, sent_at = excluded.sent_at, detail = ''
		WHERE host_executions.status = ?`,
		x.ID, x.CaseID, x.PlanID, x.Host, x.Action, x.Approved, x.Envelope, ExecSent, s.now().Format(ts), ExecNotSent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrExecutionExists
	}
	return nil
}

// EndHostExecution records what came back (or that nothing could be sent).
func (s *Store) EndHostExecution(ctx context.Context, id, status string, exitCode int, outputSHA, detail, chainHead string) error {
	var ended any
	if status != ExecNotSent {
		ended = s.now().Format(ts)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE host_executions SET status = ?, exit_code = ?, output_sha256 = ?, detail = ?, chain_head = ?, ended_at = ?
		WHERE id = ?`, status, exitCode, outputSHA, detail, chainHead, ended, id)
	return err
}

// HostExecution returns an execution by id, or nil.
func (s *Store) HostExecution(ctx context.Context, id string) (*HostExecution, error) {
	var x HostExecution
	err := s.db.QueryRowContext(ctx, `SELECT id, case_id, plan_id, host, action, approved, envelope, status, exit_code, output_sha256, detail,
		chain_head, sent_at, ended_at FROM host_executions WHERE id = ?`, id).Scan(&x.ID, &x.CaseID, &x.PlanID, &x.Host, &x.Action,
		&x.Approved, &x.Envelope, &x.Status, &x.ExitCode, &x.OutputSHA256, &x.Detail, &x.ChainHead, &x.SentAt, &x.EndedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &x, err
}

// HostExecutions lists flotillad's record of a host's executions, oldest first (`flotilla host audit`).
func (s *Store) HostExecutions(ctx context.Context, host string) ([]HostExecution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, case_id, plan_id, host, action, approved, status, exit_code, output_sha256,
		detail, chain_head, sent_at, ended_at FROM host_executions WHERE host = ? ORDER BY sent_at, id`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	xs := []HostExecution{}
	for rows.Next() {
		var x HostExecution
		if err := rows.Scan(&x.ID, &x.CaseID, &x.PlanID, &x.Host, &x.Action, &x.Approved, &x.Status, &x.ExitCode,
			&x.OutputSHA256, &x.Detail, &x.ChainHead, &x.SentAt, &x.EndedAt); err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	return xs, rows.Err()
}

// RecoverHostExecutions marks every execution left sent by the last flotillad unknown (its result, if
// any, was lost with it) and returns them: their cases escalate, their targets stay held.
func (s *Store) RecoverHostExecutions(ctx context.Context) ([]HostExecution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, case_id, host FROM host_executions WHERE status = ?`, ExecSent)
	if err != nil {
		return nil, err
	}
	var xs []HostExecution
	for rows.Next() {
		var x HostExecution
		if err := rows.Scan(&x.ID, &x.CaseID, &x.Host); err != nil {
			rows.Close()
			return nil, err
		}
		xs = append(xs, x)
	}
	rows.Close()
	for _, x := range xs {
		if err := s.EndHostExecution(ctx, x.ID, "unknown", 0, "", "flotillad stopped before its result came back", ""); err != nil {
			return nil, err
		}
	}
	return xs, nil
}
