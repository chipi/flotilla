// Package hostaudit is a host agent's audit log (ADR-0006 §8, security T15): append-only JSONL, each
// line naming the sha256 of the exact bytes of the line before it. The hash of the last line is the
// chain head; every result the agent sends carries it, and flotillad keeps it per host. A removed or
// rewritten line breaks the chain from the next line on; a rewritten or removed last line shows when
// the log no longer leads to the head flotillad stored (Leads).
//
// The log is verified whole when it is opened, and a broken or torn one is refused: the agent does not
// run executions on a log it cannot vouch for. The log's directory is root-owned (install, H8); shipping
// it off the host is the host's log collector's job, not the agent's.
package hostaudit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Events in the log.
const (
	Started = "started" // committed before the command runs
	Ended   = "result"  // what it did
	Refused = "refused" // nothing ran
	Resent  = "resent"  // an identical resend got the recorded result; nothing ran
)

// Entry is one line: who approved, what ran (the resolved command), when, the result, the output's hash.
type Entry struct {
	Seq         int64     `json:"seq"`
	Time        time.Time `json:"time"`
	Event       string    `json:"event"`
	ExecutionID string    `json:"execution_id,omitempty"`
	CaseID      string    `json:"case_id,omitempty"`
	Action      string    `json:"action,omitempty"`
	Approved    string    `json:"approved,omitempty"`
	Argv        []string  `json:"argv,omitempty"`
	Status      string    `json:"status,omitempty"`
	ExitCode    int       `json:"exit_code,omitempty"`
	OutputSHA   string    `json:"output_sha256,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Prev        string    `json:"prev"`
}

// Log is an open audit log, appended to by one agent.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	seq  int64
	head string
}

// Open verifies the log at path (creating it when absent) and opens it for appending.
func Open(path string) (*Log, error) {
	r, err := os.Open(path)
	var hashes []string
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		hashes, err = Verify(r)
		r.Close()
		if err != nil {
			return nil, fmt.Errorf("the audit log %s: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, seq: int64(len(hashes))}
	if len(hashes) > 0 {
		l.head = hashes[len(hashes)-1]
	}
	return l, nil
}

// Head is the hash of the last line ("" for an empty log).
func (l *Log) Head() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head
}

// Append writes e as the next line (its Seq and Prev are set here), synced to disk, and returns the new
// head. On an error the head is unchanged.
func (l *Log) Append(e Entry) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq, e.Prev = l.seq+1, l.head
	line, err := json.Marshal(e)
	if err != nil {
		return l.head, err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return l.head, err
	}
	if err := l.f.Sync(); err != nil {
		return l.head, err
	}
	l.seq, l.head = e.Seq, hash(line)
	return l.head, nil
}

func (l *Log) Close() error { return l.f.Close() }

func hash(line []byte) string {
	s := sha256.Sum256(line)
	return hex.EncodeToString(s[:])
}

// Verify reads a whole log and checks its chain: every line a strict entry naming the hash of the line
// before it (the first names none), ending in a newline. It returns each line's hash. (Seq is for
// readers: a line removed, moved or edited already breaks the chain.)
func Verify(r io.Reader) ([]string, error) {
	br := bufio.NewReader(r)
	var hashes []string
	prev := ""
	for {
		line, err := br.ReadBytes('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return hashes, nil
		}
		n := len(hashes) + 1
		if err != nil {
			if errors.Is(err, io.EOF) {
				return hashes, fmt.Errorf("line %d is torn (no newline)", n)
			}
			return hashes, err
		}
		line = line[:len(line)-1]
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return hashes, fmt.Errorf("line %d: %v", n, err)
		}
		if e.Prev != prev {
			return hashes, fmt.Errorf("line %d does not follow line %d: the chain is broken", n, n-1)
		}
		prev = hash(line)
		hashes = append(hashes, prev)
	}
}

// Leads reports whether a verified log (its hashes) leads to head, a chain head flotillad stored: the
// log may have grown since, but every line up to head is the one flotillad was told about.
func Leads(hashes []string, head string) bool {
	if head == "" {
		return true
	}
	for _, h := range hashes {
		if h == head {
			return true
		}
	}
	return false
}
