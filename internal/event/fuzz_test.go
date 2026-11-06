package event

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Fuzzing the envelope, because the JSONL log is the contract and other tools
// both read and write it (BUILD.md §1). Identity is content-addressed, so the
// worst failure here is not a crash: it is two writers disagreeing about
// whether they asserted the same fact, or an id that cannot be recomputed from
// the bytes that carry it.
//
// A Fuzz target runs its seed corpus under a plain `go test`, so these are
// ordinary regression tests even when nobody passes -fuzz. Run the search with
// `make fuzz`.

// seedFromConformance feeds the committed fixtures in as seeds. They are the
// portable statement of the format (EVENTS.md §7), which makes them the best
// starting points the repo has — mutation from a real event reaches far more
// of the parser than mutation from a hand-written literal.
func seedFromConformance(f *testing.F) {
	f.Helper()
	paths, err := filepath.Glob("../../testdata/conformance/*.jsonl")
	if err != nil || len(paths) == 0 {
		f.Fatalf("no conformance fixtures to seed from: %v", err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(line) > 0 {
				f.Add(line)
			}
		}
	}
}

// FuzzParseRoundTrip pins the relationship between the two halves of the
// format: anything Parse accepts, Marshal must be able to write back.
//
// They can drift apart while each looks correct alone, and did — Parse accepted
// integers wider than int64 (legal under §1.1, and §1.3 requires retaining
// fields a reader does not recognize) while canonicalNumber narrowed through
// Int64 and rejected them.
func FuzzParseRoundTrip(f *testing.F) {
	seedFromConformance(f)
	// A number too wide for int64: the case that motivated the target.
	f.Add([]byte(`{"v":1,"ev":"x","id":"` + bytes64('a') + `","at":"2026-07-31T09:14:22Z","by":"u","source":"observed","n":123456789012345678901234567890}`))
	// Nesting, unicode, and escapes, which the string and container encoders own.
	f.Add([]byte(`{"v":1,"ev":"x","id":"` + bytes64('b') + `","at":"2026-07-31T09:14:22Z","by":"u","source":"inferred","k":{"z":[1,"é😀",null,{"a":-0}]}}`))

	f.Fuzz(func(t *testing.T, line []byte) {
		e, err := Parse(line)
		if err != nil {
			return // rejecting input is fine; accepting-then-failing is not
		}

		out, err := e.Marshal()
		if err != nil {
			t.Fatalf("Parse accepted a line Marshal cannot write: %v\n%q", err, line)
		}

		// Marshal must not depend on map iteration order, which Go randomizes.
		// An unsorted encoder would give the same event two ids across runs.
		if again, err := e.Marshal(); err != nil || !bytes.Equal(out, again) {
			t.Fatalf("Marshal is not deterministic:\n%q\n%q\n(err %v)", out, again, err)
		}

		// The canonical form has to be a fixed point, or re-writing a log that
		// was already canonical would change every id in it.
		reparsed, err := Parse(out)
		if err != nil {
			t.Fatalf("canonical output does not re-parse: %v\n%q -> %q", err, line, out)
		}
		out2, err := reparsed.Marshal()
		if err != nil {
			t.Fatalf("re-parsed event will not marshal: %v\n%q", err, out)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("canonical form is not a fixed point:\n%q\n%q", out, out2)
		}

		// Identity survives the round trip. Hashing is what assigns ids, so if a
		// trip through the log changed the hash, a re-read event could never be
		// matched against the one that was written.
		if h1, h2 := hashWithoutID(t, e), hashWithoutID(t, reparsed); h1 != h2 {
			t.Fatalf("hash changed across a round trip: %s != %s\n%q", h1, h2, line)
		}
	})
}

// hashWithoutID seals a copy so the event's own id does not feed its hash.
func hashWithoutID(t *testing.T, e Event) string {
	t.Helper()
	clone := Event{}
	for k, v := range e {
		if k != "id" {
			clone[k] = v
		}
	}
	h, err := clone.Hash()
	if err != nil {
		t.Fatalf("Hash failed on an event that marshals: %v", err)
	}
	return h
}

func bytes64(c byte) string { return string(bytes.Repeat([]byte{c}, 64)) }
