// Package event implements the log record format specified in docs/EVENTS.md.
//
// EVENTS.md is normative; this package implements it and does not extend it.
package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Version is the schema version this build writes and can read (EVENTS.md §1.2).
const Version = 1

// Source records how the asserting actor came to believe the event (EVENTS.md §4).
const (
	SourceObserved = "observed"
	SourceAsserted = "asserted"
	SourceInferred = "inferred"
)

// Event is one record in the log.
//
// It is a map rather than a struct so that fields written by a newer version
// survive a read intact (EVENTS.md §1.3 requires ignoring unknown fields, not
// dropping them) and so canonicalization hashes exactly the fields present.
type Event map[string]any

// isHexID reports whether s is 64 lowercase hex characters (EVENTS.md §1.2).
//
// Hand-rolled rather than a regexp: this runs once per event on every read, and
// the regexp version measured at 10% of total fold time.
func isHexID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ErrUnsupportedVersion reports a log written by a newer weft than this one.
// Silently mis-folding such a log is the one failure EVENTS.md §1.3 refuses to
// tolerate, so this is fatal rather than skippable.
type ErrUnsupportedVersion struct {
	Got  int
	Have int
}

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("log schema v%d is newer than supported v%d", e.Got, e.Have)
}

// New builds an unsealed event. Callers add kind-specific fields, then Seal.
//
// Timestamps carry sub-second precision, and that is load-bearing rather than
// cosmetic. Event ids hash the whole record including "at", and the fold treats
// a repeated id as a crashed-and-retried write (EVENTS.md §5.2). At second
// granularity two genuinely distinct assertions made in the same second — a
// node reopened and reopened again, the same claim renewed — hash identically
// and the second is silently dropped as a duplicate.
func New(kind, actor, source string, at time.Time) Event {
	return Event{
		"v":      json.Number("1"),
		"ev":     kind,
		"at":     at.Format(time.RFC3339Nano),
		"by":     actor,
		"source": source,
	}
}

// Kind returns the event kind (the "ev" field).
func (e Event) Kind() string { s, _ := e["ev"].(string); return s }

// ID returns the event id, empty if unsealed.
func (e Event) ID() string { s, _ := e["id"].(string); return s }

// Str returns a string field, empty if absent or not a string.
func (e Event) Str(k string) string { s, _ := e[k].(string); return s }

// Version returns the schema version, or 0 if absent or malformed.
func (e Event) Version() int {
	n, ok := e["v"].(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil {
		return 0
	}
	return int(i)
}

// Hash computes the event id: sha256 over the canonical serialization of the
// event with "id" absent (EVENTS.md §2.1). Excluding id from its own preimage
// is what makes the definition non-circular.
func (e Event) Hash() (string, error) {
	preimage := make(Event, len(e))
	for k, v := range e {
		if k == "id" {
			continue
		}
		preimage[k] = v
	}
	var buf bytes.Buffer
	if err := canonical(&buf, map[string]any(preimage)); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// Seal computes and sets the event id. It is an error to seal twice, which
// would silently produce an id that does not match the payload.
func (e Event) Seal() error {
	if _, ok := e["id"]; ok {
		return fmt.Errorf("event already sealed")
	}
	h, err := e.Hash()
	if err != nil {
		return err
	}
	e["id"] = h
	return nil
}

// Marshal renders the event as one canonical line, without the trailing LF.
//
// Writing the canonical form (rather than any valid JSON) means a re-read event
// hashes to the id it carries, which the conformance fixtures check.
func (e Event) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	if err := canonical(&buf, map[string]any(e)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Parse decodes one log line and validates the envelope (EVENTS.md §1.2).
//
// Unknown fields are retained; an unknown "ev" is *not* an error here, because
// the fold is what decides to ignore it (EVENTS.md §1.3).
func Parse(line []byte) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	// Numbers stay exact: EVENTS.md §1.1 bans floats precisely so that a
	// decode/encode round trip cannot perturb a value, which float64 would.
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing content after JSON object")
	}
	// §1.1's byte rules bind every writer, so a number that breaks them makes the
	// line invalid rather than merely unrecognized. Checked here so that whatever
	// Parse accepts, Marshal can write back — otherwise the event carries an id
	// that cannot be recomputed from it.
	if err := checkNumbers(raw); err != nil {
		return nil, err
	}
	e := Event(raw)

	v := e.Version()
	if v == 0 {
		return nil, fmt.Errorf("missing or malformed %q", "v")
	}
	if v > Version {
		return nil, &ErrUnsupportedVersion{Got: v, Have: Version}
	}
	if e.Kind() == "" {
		return nil, fmt.Errorf("missing %q", "ev")
	}
	if !isHexID(e.ID()) {
		return nil, fmt.Errorf("%q must be 64 lowercase hex chars", "id")
	}
	if e.Str("by") == "" {
		return nil, fmt.Errorf("missing %q", "by")
	}
	switch e.Str("source") {
	case SourceObserved, SourceAsserted, SourceInferred:
	default:
		return nil, fmt.Errorf("invalid %q: %q", "source", e.Str("source"))
	}
	if _, err := time.Parse(time.RFC3339, e.Str("at")); err != nil {
		return nil, fmt.Errorf("invalid %q: %w", "at", err)
	}
	return e, nil
}

// Verify reports whether the event's id matches its payload.
func (e Event) Verify() error {
	want, err := e.Hash()
	if err != nil {
		return err
	}
	if got := e.ID(); got != want {
		return fmt.Errorf("id mismatch: carries %s, hashes to %s", got, want)
	}
	return nil
}
