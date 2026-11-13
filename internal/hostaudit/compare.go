package hostaudit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Record is flotillad's record of one execution it sent to the host (store.HostExecution's status: a
// host status, or sent, not_sent, unknown).
type Record struct {
	ID     string
	Status string
}

// Finding is one line of an audit: Bad findings fail it.
type Finding struct {
	Bad    bool
	Detail string
}

// Compare audits a host's log against flotillad's record (ADR-0006 §8, `flotilla host audit`):
//   - the log's chain verifies, and it leads to storedHead, the head flotillad last heard: a log
//     rewritten after it reported fails here;
//   - every execution the host logged as started, flotillad sent: the host ran nothing it was not
//     sent;
//   - every execution flotillad recorded with a result, the host logged with that result; one flotillad
//     holds as unknown (the connection was lost) is resolved by the host's own line, reported as such;
//   - one flotillad never got to the host (not_sent) the host never logged.
//
// Lines refused before verifying carry the id the envelope claimed (unverified), so they are reported,
// never counted as executions.
func Compare(log []byte, records []Record, storedHead string) []Finding {
	var out []Finding
	bad := func(f string, a ...any) { out = append(out, Finding{true, fmt.Sprintf(f, a...)}) }
	good := func(f string, a ...any) { out = append(out, Finding{false, fmt.Sprintf(f, a...)}) }

	hashes, err := Verify(bytes.NewReader(log))
	if err != nil {
		bad("the log's chain: %v", err)
		return out
	}
	good("the log's chain verifies: %d lines", len(hashes))
	if Leads(hashes, storedHead) {
		good("it leads to the head flotillad stored (%s)", short(storedHead))
	} else {
		bad("it does not lead to the head flotillad stored (%s): the log was rewritten after it reported", short(storedHead))
	}

	started := map[string]bool{}
	result := map[string]string{} // the last result the host logged, per id
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			bad("a line does not decode: %v", err)
			continue
		}
		switch e.Event {
		case Started:
			started[e.ExecutionID] = true
		case Ended, Refused:
			if e.ExecutionID != "" {
				result[e.ExecutionID] = e.Status
			}
		}
	}

	sent := map[string]Record{}
	for _, r := range records {
		sent[r.ID] = r
	}
	for _, id := range sortedKeys(started) {
		if _, ok := sent[id]; !ok {
			bad("%s: the host ran it, and flotillad has no record of sending it", id)
		}
	}
	for _, r := range records {
		hostSays, logged := result[r.ID]
		switch {
		case r.Status == "not_sent":
			if logged || started[r.ID] {
				bad("%s: flotillad recorded it as never sent, and the host logged it (%s)", r.ID, hostSays)
			}
		case r.Status == "sent" || r.Status == "unknown":
			if logged {
				good("%s: flotillad holds it as %s; the host's log says %s", r.ID, r.Status, hostSays)
			} else if started[r.ID] {
				good("%s: flotillad holds it as %s; the host started it and logged no result (stopped mid-run)", r.ID, r.Status)
			} else {
				good("%s: flotillad holds it as %s; the host never logged it (it did not arrive)", r.ID, r.Status)
			}
		case !logged:
			bad("%s: flotillad recorded %s, and the host's log has no result for it", r.ID, r.Status)
		case hostSays != r.Status:
			bad("%s: flotillad recorded %s, the host logged %s", r.ID, r.Status, hostSays)
		default:
			good("%s: %s on both sides", r.ID, r.Status)
		}
	}
	for _, id := range sortedKeys(result) {
		if _, ok := sent[id]; !ok && !started[id] {
			good("%s: refused by the host (%s), not one flotillad sent: a refusal of an envelope that did not verify carries the id it claimed", id, result[id])
		}
	}
	return out
}

// Failed reports whether any finding is bad.
func Failed(fs []Finding) bool {
	for _, f := range fs {
		if f.Bad {
			return true
		}
	}
	return false
}

func short(h string) string {
	if h == "" {
		return "none: nothing reported yet"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// String is one finding's line.
func (f Finding) String() string {
	if f.Bad {
		return "FAIL " + f.Detail
	}
	return "ok   " + strings.TrimSpace(f.Detail)
}
