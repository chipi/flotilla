package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The directory's copy (RFC-0014 §4.6): who a chat user, a code-host login or a tracker account is, the same person everywhere.
// A sync replaces the copy with what the source read and records each change in the ledger; a lookup reads the copy only, so a
// directory that is down never stops a case. Who someone is, never what they may do: roles and pinned users decide that.

// Person is one person of the directory: keyed by email, with their id in each tool (tool name to id).
type Person struct {
	Email string            `json:"email"`
	Name  string            `json:"name"`
	IDs   map[string]string `json:"ids,omitempty"`
}

// PersonChange is one change a sync made: Kind is person.added, person.changed or person.removed; Detail says what moved.
type PersonChange struct {
	Email, Kind, Detail string
}

// SyncPeople makes the copy the source's people, in one transaction: a new person is person.added, one whose name or ids
// moved is person.changed (the detail names the fields), one the source no longer has is person.removed. Each is an event
// with the actor (the source, e.g. "directory:file") and via "directory". An unchanged person writes nothing.
func (s *Store) SyncPeople(ctx context.Context, people []Person, actor string) ([]PersonChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	old := map[string]Person{}
	rows, err := tx.QueryContext(ctx, `SELECT email, name, ids FROM people`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Person
		var ids string
		if err := rows.Scan(&p.Email, &p.Name, &ids); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(ids), &p.IDs)
		old[p.Email] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	at := s.now().Format(ts)
	event := func(kind, email, detail string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, 'directory', ?, ?, ?)`, at, actor, kind, email, detail)
		return err
	}
	var out []PersonChange
	seen := map[string]bool{}
	sorted := append([]Person(nil), people...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Email < sorted[b].Email })
	for _, p := range sorted {
		seen[p.Email] = true
		body, _ := json.Marshal(p.IDs)
		was, had := old[p.Email]
		var kind, detail string
		switch {
		case !had:
			kind, detail = "person.added", describePerson(p)
			if _, err := tx.ExecContext(ctx, `INSERT INTO people (email, name, ids, updated_at) VALUES (?, ?, ?, ?)`, p.Email, p.Name, string(body), at); err != nil {
				return nil, err
			}
		default:
			diff := personDiff(was, p)
			if len(diff) == 0 {
				continue
			}
			kind, detail = "person.changed", strings.Join(diff, "; ")
			if _, err := tx.ExecContext(ctx, `UPDATE people SET name = ?, ids = ?, updated_at = ? WHERE email = ?`, p.Name, string(body), at, p.Email); err != nil {
				return nil, err
			}
		}
		if err := event(kind, p.Email, detail); err != nil {
			return nil, err
		}
		out = append(out, PersonChange{Email: p.Email, Kind: kind, Detail: detail})
	}
	var gone []string
	for email := range old {
		if !seen[email] {
			gone = append(gone, email)
		}
	}
	sort.Strings(gone)
	for _, email := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM people WHERE email = ?`, email); err != nil {
			return nil, err
		}
		if err := event("person.removed", email, "no longer in the directory"); err != nil {
			return nil, err
		}
		out = append(out, PersonChange{Email: email, Kind: "person.removed", Detail: "no longer in the directory"})
	}
	// The ids are rebuilt whole: an id that moved from one person to another in this sync never meets its old row.
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_ids`); err != nil {
		return nil, err
	}
	for _, p := range sorted {
		for _, tool := range sortedTools(p.IDs) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_ids (tool, id, email) VALUES (?, ?, ?)`, tool, p.IDs[tool], p.Email); err != nil {
				return nil, fmt.Errorf("person %s: %s id %s: %w", p.Email, tool, p.IDs[tool], err)
			}
		}
	}
	return out, tx.Commit()
}

// PersonBy is the person whose id in tool is id; ok is false for someone the directory does not know ("unknown").
func (s *Store) PersonBy(ctx context.Context, tool, id string) (Person, bool, error) {
	var p Person
	var ids string
	err := s.db.QueryRowContext(ctx, `SELECT p.email, p.name, p.ids FROM person_ids i JOIN people p ON p.email = i.email WHERE i.tool = ? AND i.id = ?`,
		tool, id).Scan(&p.Email, &p.Name, &ids)
	if err == sql.ErrNoRows {
		return Person{}, false, nil
	}
	if err != nil {
		return Person{}, false, err
	}
	_ = json.Unmarshal([]byte(ids), &p.IDs)
	return p, true, nil
}

// People is the directory's copy, by email.
func (s *Store) People(ctx context.Context) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, name, ids FROM people ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		var ids string
		if err := rows.Scan(&p.Email, &p.Name, &ids); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ids), &p.IDs)
		out = append(out, p)
	}
	return out, rows.Err()
}

func describePerson(p Person) string {
	parts := []string{p.Name}
	for _, tool := range sortedTools(p.IDs) {
		parts = append(parts, tool+" "+p.IDs[tool])
	}
	return strings.Join(parts, "; ")
}

func personDiff(was, now Person) []string {
	var out []string
	if was.Name != now.Name {
		out = append(out, fmt.Sprintf("name %q -> %q", was.Name, now.Name))
	}
	tools := map[string]bool{}
	for t := range was.IDs {
		tools[t] = true
	}
	for t := range now.IDs {
		tools[t] = true
	}
	all := make([]string, 0, len(tools))
	for t := range tools {
		all = append(all, t)
	}
	sort.Strings(all)
	for _, t := range all {
		a, b := was.IDs[t], now.IDs[t]
		switch {
		case a == b:
		case a == "":
			out = append(out, fmt.Sprintf("%s %s added", t, b))
		case b == "":
			out = append(out, fmt.Sprintf("%s %s removed", t, a))
		default:
			out = append(out, fmt.Sprintf("%s %s -> %s", t, a, b))
		}
	}
	return out
}

func sortedTools(ids map[string]string) []string {
	out := make([]string, 0, len(ids))
	for t := range ids {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
