package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Roles a person can hold (ADR-0014), each including the one below it.
const (
	UserAdmin    = "admin"
	UserOperator = "operator"
	UserViewer   = "viewer"
)

var (
	ErrUserExists = errors.New("that name is taken (names are never reused, revoked or not)")
	ErrNoSuchUser = errors.New("no such user, or the user is revoked")
	ErrLastAdmin  = errors.New("there must always be an admin: this would leave none")
	ErrBadRole    = errors.New("role must be admin, operator or viewer")
)

type User struct {
	Name      string  `json:"name"`
	Role      string  `json:"role"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

func validRole(r string) bool { return r == UserAdmin || r == UserOperator || r == UserViewer }

func activeAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND revoked_at IS NULL`, UserAdmin).Scan(&n)
	return n, err
}

// ActiveAdmins counts the admins who are not revoked.
func (s *Store) ActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND revoked_at IS NULL`, UserAdmin).Scan(&n)
	return n, err
}

// AddUser adds a user with a token hash. With bootstrap (the callers-file operator token, held on the
// host), it must be an admin: the first one ("user.bootstrap", on an empty table) or, when admins exist
// but their tokens are lost, break-glass ("user.break-glass"); a lost token cannot be told from a kept
// one, so break-glass is always open to that host-held token and always recorded.
func (s *Store) AddUser(ctx context.Context, name, role, tokenSHA256, actor, via string, bootstrap bool) error {
	if !validRole(role) {
		return ErrBadRole
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind := "user.added"
	if bootstrap {
		if role != UserAdmin {
			return fmt.Errorf("the first user must be an admin")
		}
		var any int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&any); err != nil {
			return err
		}
		kind = "user.bootstrap"
		if any > 0 {
			kind = "user.break-glass"
		}
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return ErrUserExists
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (name, role, token_sha256, created_at) VALUES (?, ?, ?, ?)`,
		name, role, tokenSHA256, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, kind, "user:"+name, role); err != nil {
		return err
	}
	return tx.Commit()
}

// activeRole returns an active user's role, or ErrNoSuchUser.
func activeRole(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE name = ? AND revoked_at IS NULL`, name).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSuchUser
	}
	return role, err
}

// SetRole changes an active user's role. The last admin cannot stop being one.
func (s *Store) SetRole(ctx context.Context, name, role, actor, via string) error {
	if !validRole(role) {
		return ErrBadRole
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := activeRole(ctx, tx, name)
	if err != nil {
		return err
	}
	if old == UserAdmin && role != UserAdmin {
		n, err := activeAdmins(ctx, tx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE name = ?`, role, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, "user.role", "user:"+name, old+" -> "+role); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeUser revokes a user and ends their sessions. The last admin cannot be revoked.
func (s *Store) RevokeUser(ctx context.Context, name, actor, via string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	role, err := activeRole(ctx, tx, name)
	if err != nil {
		return err
	}
	if role == UserAdmin {
		n, err := activeAdmins(ctx, tx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE users SET revoked_at = ? WHERE name = ?`, at, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_name = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, "user.revoked", "user:"+name, role); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateToken gives an active user a new token hash and ends their sessions.
func (s *Store) RotateToken(ctx context.Context, name, tokenSHA256, actor, via string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := activeRole(ctx, tx, name); err != nil {
		return err
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE users SET token_sha256 = ? WHERE name = ?`, tokenSHA256, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_name = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, "user.rotated", "user:"+name, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// UserByToken returns the active user whose token hash this is, or nil.
func (s *Store) UserByToken(ctx context.Context, tokenSHA256 string) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT name, role, created_at FROM users WHERE token_sha256 = ? AND revoked_at IS NULL`,
		tokenSHA256).Scan(&u.Name, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Users lists every user, revoked ones too, by name.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, role, created_at, revoked_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name, &u.Role, &u.CreatedAt, &u.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
