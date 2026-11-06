package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCanonicalVectors(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"keys sorted", map[string]any{"b": "2", "a": "1", "C": "3"}, `{"C":"3","a":"1","b":"2"}`},
		{"nested", map[string]any{"a": map[string]any{"z": "1", "y": "2"}}, `{"a":{"y":"2","z":"1"}}`},
		{"array order preserved", map[string]any{"a": []any{"c", "a", "b"}}, `{"a":["c","a","b"]}`},
		{"null", map[string]any{"a": nil}, `{"a":null}`},
		{"bool", map[string]any{"a": true, "b": false}, `{"a":true,"b":false}`},
		{"integer", map[string]any{"v": json.Number("1")}, `{"v":1}`},
		{"negative integer", map[string]any{"v": json.Number("-42")}, `{"v":-42}`},
		{"escapes", map[string]any{"a": "x\"y\\z"}, `{"a":"x\"y\\z"}`},
		{"control chars", map[string]any{"a": "\n\t\r"}, `{"a":"\n\t\r"}`},
		{"unit separator", map[string]any{"a": "\x1f"}, `{"a":"\u001f"}`},
		// Go's encoding/json escapes these for HTML safety; JCS does not.
		{"html chars unescaped", map[string]any{"a": "<&>"}, `{"a":"<&>"}`},
		{"unicode literal", map[string]any{"a": "café ☃"}, `{"a":"café ☃"}`},
		{"empty object", map[string]any{}, `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := canonical(&buf, tc.in); err != nil {
				t.Fatalf("canonical: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// EVENTS.md §1.1 bans floats so that canonicalization stays tractable. A float
// reaching the canonicalizer must fail loudly, not round.
func TestCanonicalRejectsFloats(t *testing.T) {
	for _, v := range []any{
		map[string]any{"a": json.Number("1.5")},
		map[string]any{"a": json.Number("1e3")},
		map[string]any{"a": 1.5},
	} {
		var buf bytes.Buffer
		if err := canonical(&buf, v); err == nil {
			t.Errorf("%v: expected error, got %s", v, buf.String())
		}
	}
}

// The preimage excludes id, which is what makes the definition non-circular
// (EVENTS.md §2.1).
func TestHashExcludesID(t *testing.T) {
	e := Event{"v": json.Number("1"), "ev": "node.add", "by": "user:x", "source": SourceAsserted}
	before, err := e.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Seal(); err != nil {
		t.Fatal(err)
	}
	after, err := e.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("hash changed once id was set: %s -> %s", before, after)
	}
	if e.ID() != before {
		t.Errorf("sealed id %s does not match hash %s", e.ID(), before)
	}
}

func TestSealTwiceFails(t *testing.T) {
	e := Event{"v": json.Number("1"), "ev": "x", "by": "user:x", "source": SourceAsserted}
	if err := e.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := e.Seal(); err == nil {
		t.Error("expected error sealing an already-sealed event")
	}
}

// Two actors adding the same title must produce different ids: this is event
// identity, not content identity (EVENTS.md §2.2).
func TestEventIdentityNotContentIdentity(t *testing.T) {
	at := time.Date(2026, 7, 31, 9, 14, 22, 0, time.UTC)
	a, err := NodeAdd("acme/api", "bug", "same title", "agent:one", SourceInferred, at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NodeAdd("acme/api", "bug", "same title", "agent:two", SourceInferred, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() == b.ID() {
		t.Error("different actors produced the same id; content identity would conflate independent observations")
	}
}

func TestRoundTripPreservesID(t *testing.T) {
	at := time.Date(2026, 7, 31, 9, 14, 22, 0, time.UTC)
	orig, err := NodeAdd("acme/api", "bug", "café \"quoted\" <tag>", "user:matt", SourceAsserted, at)
	if err != nil {
		t.Fatal(err)
	}
	line, err := orig.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if back.ID() != orig.ID() {
		t.Errorf("id changed across round trip: %s -> %s", orig.ID(), back.ID())
	}
	if err := back.Verify(); err != nil {
		t.Errorf("re-read event fails verification: %v", err)
	}
}

// Unknown fields belong to a newer writer and must survive, not error
// (EVENTS.md §1.3).
func TestParseKeepsUnknownFields(t *testing.T) {
	e := Event{
		"v": json.Number("1"), "ev": "node.add", "at": "2026-07-31T09:14:22Z",
		"by": "user:x", "source": SourceAsserted, "future_field": "keep me",
	}
	if err := e.Seal(); err != nil {
		t.Fatal(err)
	}
	line, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(line)
	if err != nil {
		t.Fatalf("unknown field caused a parse error: %v", err)
	}
	if back.Str("future_field") != "keep me" {
		t.Error("unknown field was dropped")
	}
}

func TestParseRejectsNewerVersion(t *testing.T) {
	line := []byte(`{"at":"2026-07-31T09:14:22Z","by":"user:x","ev":"node.add","id":"` +
		strings.Repeat("a", 64) + `","source":"asserted","v":2}`)
	_, err := Parse(line)
	var unsupported *ErrUnsupportedVersion
	if err == nil {
		t.Fatal("expected refusal of a newer schema version")
	}
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected ErrUnsupportedVersion, got %T: %v", err, err)
	}
}

func TestParseEnvelopeValidation(t *testing.T) {
	id := strings.Repeat("a", 64)
	cases := []struct {
		name string
		line string
	}{
		{"missing v", `{"at":"2026-07-31T09:14:22Z","by":"u","ev":"node.add","id":"` + id + `","source":"asserted"}`},
		{"missing ev", `{"at":"2026-07-31T09:14:22Z","by":"u","id":"` + id + `","source":"asserted","v":1}`},
		{"short id", `{"at":"2026-07-31T09:14:22Z","by":"u","ev":"n","id":"abc","source":"asserted","v":1}`},
		{"uppercase id", `{"at":"2026-07-31T09:14:22Z","by":"u","ev":"n","id":"` + strings.Repeat("A", 64) + `","source":"asserted","v":1}`},
		{"missing by", `{"at":"2026-07-31T09:14:22Z","ev":"n","id":"` + id + `","source":"asserted","v":1}`},
		{"bad source", `{"at":"2026-07-31T09:14:22Z","by":"u","ev":"n","id":"` + id + `","source":"guessed","v":1}`},
		{"bad at", `{"at":"yesterday","by":"u","ev":"n","id":"` + id + `","source":"asserted","v":1}`},
		{"not an object", `["nope"]`},
		{"trailing content", `{"v":1} {"v":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.line)); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}
