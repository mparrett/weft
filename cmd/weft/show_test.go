package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedCollision writes a log by hand containing two nodes whose ids share a
// 7-character prefix. Waiting for sha256 to produce one would take an
// impractical corpus, and the format check on ids does not require the hash to
// verify, so the collision can be constructed exactly (EVENTS.md §2.4).
func seedCollision(t *testing.T, dir string) (idA, idB string) {
	t.Helper()
	idA = "aaaaaaa1" + strings.Repeat("0", 56)
	idB = "aaaaaaa2" + strings.Repeat("0", 56)
	lines := ""
	for _, pair := range [][2]string{{idA, "first colliding node"}, {idB, "second colliding node"}} {
		lines += `{"at":"2026-07-31T09:14:22Z","body":null,"by":"user:test","ev":"node.add","id":"` +
			pair[0] + `","kind":"bug","ns":"default","source":"asserted","title":"` + pair[1] + `","v":1}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return idA, idB
}

func TestShowResolvesUniquePrefix(t *testing.T) {
	scratch(t)
	if code, _, stderr := session(t, "add", "findable", "--kind", "bug"); code != exitOK {
		t.Fatalf("add: %s", stderr)
	}
	_, listing, _ := session(t, "ls")
	short := strings.Fields(listing)[0]

	// Every prefix length from 1 up to the displayed width must resolve.
	for i := 1; i <= len(short); i++ {
		code, stdout, stderr := session(t, "show", short[:i])
		if code != exitOK {
			t.Errorf("prefix %q: exit %d, stderr %s", short[:i], code, stderr)
			continue
		}
		if !strings.Contains(stdout, "findable") {
			t.Errorf("prefix %q resolved to the wrong node:\n%s", short[:i], stdout)
		}
	}
}

// The exit condition: an ambiguous prefix exits nonzero listing every candidate.
func TestShowAmbiguousPrefixListsCandidates(t *testing.T) {
	dir := scratch(t)
	idA, idB := seedCollision(t, dir)

	code, _, stderr := session(t, "show", "aaaaaaa")
	if code != exitAmbiguous {
		t.Fatalf("exit %d, want %d", code, exitAmbiguous)
	}
	// A bare count leaves the user no better off — both ids and both titles
	// have to appear.
	for _, want := range []string{"first colliding node", "second colliding node"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("candidate %q missing from:\n%s", want, stderr)
		}
	}
	if !strings.Contains(stderr, idA[:8]) || !strings.Contains(stderr, idB[:8]) {
		t.Errorf("candidate ids not shown at a distinguishing width:\n%s", stderr)
	}
}

// Display width expands past 7 on a corpus seeded with a 7-char collision.
func TestDisplayWidthExpandsOnCollision(t *testing.T) {
	dir := scratch(t)
	idA, idB := seedCollision(t, dir)

	_, stdout, _ := session(t, "ls")
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		shown := strings.Fields(line)[0]
		if len(shown) <= 7 {
			t.Errorf("id shown at width %d, must expand past 7 on a collision: %q", len(shown), line)
		}
	}
	// And what is shown must actually be unambiguous — that is the point.
	for _, id := range []string{idA, idB} {
		shown := ""
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			f := strings.Fields(line)[0]
			if strings.HasPrefix(id, f) {
				shown = f
			}
		}
		if shown == "" {
			t.Fatalf("no displayed prefix for %s", id[:8])
		}
		if code, _, _ := session(t, "show", shown); code != exitOK {
			t.Errorf("displayed prefix %q did not resolve uniquely", shown)
		}
	}
}

func TestShowNotFoundIsDistinctFromAmbiguous(t *testing.T) {
	dir := scratch(t)
	seedCollision(t, dir)

	if code, _, _ := session(t, "show", "ffffff"); code != exitNotFound {
		t.Errorf("missing node: exit %d, want %d", code, exitNotFound)
	}
	if code, _, _ := session(t, "show", "aaaaaaa"); code != exitAmbiguous {
		t.Errorf("ambiguous prefix: exit %d, want %d", code, exitAmbiguous)
	}
}

func TestShowByRef(t *testing.T) {
	scratch(t)
	session(t, "add", "bound node", "--kind", "issue", "--ref", "gh:acme/api#412")

	code, stdout, stderr := session(t, "show", "gh:acme/api#412")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "bound node") {
		t.Errorf("ref did not resolve:\n%s", stdout)
	}
}

func TestShowJSONAmbiguousCarriesCandidates(t *testing.T) {
	dir := scratch(t)
	seedCollision(t, dir)

	code, stdout, _ := session(t, "show", "aaaaaaa", "--json")
	if code != exitAmbiguous {
		t.Fatalf("exit %d", code)
	}
	var out struct {
		Code       int      `json:"code"`
		Candidates []string `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if out.Code != exitAmbiguous {
		t.Errorf("code = %d", out.Code)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(out.Candidates))
	}
	// Full ids, never abbreviated: a stored short id can become ambiguous later.
	for _, c := range out.Candidates {
		if len(c) != 64 {
			t.Errorf("candidate %q is not a full id", c)
		}
	}
}

func TestShowUsageErrors(t *testing.T) {
	scratch(t)
	for _, args := range [][]string{{"show"}, {"show", "a", "b"}} {
		if code, _, _ := session(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
