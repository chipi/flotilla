// Package fleets holds what ships with flotillad beside the example fleets' folders: the harness capability table, embedded so a
// fleets folder an installation mounts over this one cannot shadow it (ADR-0018, ADR-0020).
package fleets

import _ "embed"

// HarnessesJSON is fleets/harnesses.json, generated from the adapters by `python3 runtime/adapter.py capabilities --write`.
//
//go:embed harnesses.json
var HarnessesJSON []byte
