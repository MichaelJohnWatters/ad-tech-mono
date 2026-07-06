// Package identityobserve auto-builds the identity graph from observed
// identifiers. The SSP (and, in future, other services) publish an
// ObservedEvent per request; a dedicated consumer feeds those into an Observer
// that batches, dedupes, and writes edges to the identity graph. Splitting the
// observation (cheap, on many pods) from the write (one consumer) keeps the
// write off every serving pod and gives the probabilistic fingerprint state a
// single global view.
package identityobserve

import "encoding/json"

// SchemaVersion is field-1-style versioning for the JSON event.
const SchemaVersion = 1

// Signal is one identifier seen on a request, with its source/type.
type Signal struct {
	Value  string `json:"v"`
	Source string `json:"s"`
}

// ObservedEvent is published (JSON, best-effort Core-style) per request that
// carries identity signals. IDs are the identifiers present; Fingerprint is the
// IP+UA used for probabilistic matching (empty when unavailable).
type ObservedEvent struct {
	SchemaVersion int      `json:"schema_version"`
	TraceID       string   `json:"trace_id,omitempty"`
	IDs           []Signal `json:"ids"`
	Fingerprint   string   `json:"fp,omitempty"`
}

// Marshal encodes the event, stamping the schema version.
func Marshal(e ObservedEvent) ([]byte, error) {
	e.SchemaVersion = SchemaVersion
	return json.Marshal(e)
}

// Unmarshal decodes an event payload.
func Unmarshal(data []byte) (ObservedEvent, error) {
	var e ObservedEvent
	err := json.Unmarshal(data, &e)
	return e, err
}
