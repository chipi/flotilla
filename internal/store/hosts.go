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
	CertSerial   string  `json:"cert_serial"`
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
func (s *Store) Enrol(ctx context.Context, host, tokenSHA256 string, issue func() (serial string, notAfter time.Time, err error)) error {
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
	serial, notAfter, err := issue()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_serial = ?, cert_not_after = ?, enrolled_at = ?, revoked_at = NULL WHERE name = ?`,
		serial, notAfter.UTC().Format(ts), at, host); err != nil {
		return err
	}
	if err := event(ctx, tx, at, "host:"+host, "enrolment", "host.enrolled", "host:"+host, "certificate "+serial); err != nil {
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
	var serial string
	if err := tx.QueryRowContext(ctx, `SELECT cert_serial FROM hosts WHERE name = ?`, host).Scan(&serial); errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchHost
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_serial = '', cert_not_after = '', revoked_at = ? WHERE name = ?`, at, host); err != nil {
		return err
	}
	// its unused enrolment tokens go with it
	if _, err := tx.ExecContext(ctx, `UPDATE host_enrolments SET used_at = ? WHERE host = ? AND used_at IS NULL`, at, host); err != nil {
		return err
	}
	if err := event(ctx, tx, at, actor, via, "host.revoked", "host:"+host, "certificate "+serial); err != nil {
		return err
	}
	return tx.Commit()
}

// HostCertCurrent reports whether serial is host's current certificate. Revoking clears it, so a
// revoked host has none.
func (s *Store) HostCertCurrent(ctx context.Context, host, serial string) (bool, error) {
	if serial == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE name = ? AND cert_serial = ?`,
		host, serial).Scan(&n)
	return n == 1, err
}

// Hosts lists every host.
func (s *Store) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, cert_serial, cert_not_after, created_at, enrolled_at, revoked_at,
		connected, version, connected_at, last_contact, clock_offset_ms, chain_head FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hs := []Host{}
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.Name, &h.CertSerial, &h.CertNotAfter, &h.CreatedAt, &h.EnrolledAt, &h.RevokedAt,
			&h.Connected, &h.Version, &h.ConnectedAt, &h.LastContact, &h.ClockOffsetMS, &h.ChainHead); err != nil {
			return nil, err
		}
		hs = append(hs, h)
	}
	return hs, rows.Err()
}

// RenewHost records host's next certificate in place of the current one, oldSerial, which must still be
// current: a renewal on a connection made with a replaced or revoked certificate is refused.
func (s *Store) RenewHost(ctx context.Context, host, oldSerial, serial string, notAfter time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	res, err := tx.ExecContext(ctx, `UPDATE hosts SET cert_serial = ?, cert_not_after = ? WHERE name = ? AND cert_serial = ? AND cert_serial <> ''`,
		serial, notAfter.UTC().Format(ts), host, oldSerial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotCurrent
	}
	if err := event(ctx, tx, at, "host:"+host, "connection", "host.renewed", "host:"+host, oldSerial+" -> "+serial); err != nil {
		return err
	}
	return tx.Commit()
}
