package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gh:/pr: doubling bug: gh:owner/repo#N and pr:owner/repo#N are both first-class names
// for a pull request, and nothing related them — one PR could occupy two nodes
// with no warning and no ambiguity error. The cure is a note at add time:
// named, still written, exit 0. Equivalence never enters resolution, because
// the moment gh:x#1 resolves a node carrying only pr:x#1, refs have become
// identity — which SPEC §4.1 refuses.

func TestAddNamesTheLiveHolderOfADuplicateRef(t *testing.T) {
	scratch(t)
	addNodeFull(t, "first take", "note", "--ref", "key:shared")

	code, stdout, stderr := session(t, "add", "second take", "--kind", "note", "--ref", "key:shared")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	if stdout == "" {
		t.Error("the note swallowed the node line")
	}
	for _, want := range []string{"key:shared is already carried by", "first take", "weft merge"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("note missing %q:\n%s", want, stderr)
		}
	}
}

// The issue's exact repro: two schemes, two nodes, each resolving cleanly, and
// no moment at which the duplicate was visible.
func TestAddNamesTheGhPrTwin(t *testing.T) {
	scratch(t)
	addNodeFull(t, "708 first title", "pr", "--ref", "pr:nooga/let-go#708")

	code, _, stderr := session(t, "add", "708 second title", "--kind", "pr", "--ref", "gh:nooga/let-go#708")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stderr, "gh:nooga/let-go#708 names the same thing as pr:nooga/let-go#708") {
		t.Errorf("scheme twin not named:\n%s", stderr)
	}
	if !strings.Contains(stderr, "708 first title") {
		t.Errorf("note does not name the existing node:\n%s", stderr)
	}
}

// The pre-flight that bit: a script indexing the store by gh: reported nodes
// held under pr: as absent. --if-absent cannot catch it (titles differ, refs
// are ignored in its match), so the note has to fire on that path too.
func TestTheNoteFiresOnTheIfAbsentCreatePath(t *testing.T) {
	scratch(t)
	addNodeFull(t, "708 first title", "pr", "--ref", "pr:nooga/let-go#708")

	_, _, stderr := session(t, "add", "708 second title", "--kind", "pr",
		"--ref", "gh:nooga/let-go#708", "--if-absent")
	if !strings.Contains(stderr, "names the same thing as") {
		t.Errorf("--if-absent miss did not warn:\n%s", stderr)
	}
}

// The retry-after-drop pattern — drop, then re-add under the same ref — is the
// documented way to re-assert retracted work. A note about the dropped holder
// would teach writers to ignore notes.
func TestTheNoteIgnoresDroppedHolders(t *testing.T) {
	scratch(t)
	addNodeFull(t, "a claim", "note", "--ref", "key:claim1")
	if code, _, stderr := session(t, "drop", "key:claim1"); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}

	_, _, stderr := session(t, "add", "a claim", "--kind", "note", "--ref", "key:claim1")
	if strings.Contains(stderr, "note:") {
		t.Errorf("warned about a dropped holder:\n%s", stderr)
	}
}

// The note is presentation: --json output stays valid on stdout, the note goes
// to stderr, and the exit code is untouched.
func TestTheNoteIsNotAnError(t *testing.T) {
	scratch(t)
	addNodeFull(t, "first take", "note", "--ref", "key:shared")

	code, stdout, stderr := session(t, "add", "second take", "--kind", "note", "--ref", "key:shared", "--json")
	if code != exitOK {
		t.Fatalf("add --json: exit %d, %s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("stdout is not JSON:\n%s", stdout)
	}
	if !strings.Contains(stderr, "already carried by") {
		t.Errorf("--json lost the note:\n%s", stderr)
	}
}

func TestSiblingSchemeRef(t *testing.T) {
	cases := map[string]string{
		"gh:acme/api#412": "pr:acme/api#412",
		"pr:acme/api#412": "gh:acme/api#412",
		// No issue number: gh:o/r names a repo, not a PR, and the narrow case
		// is deliberately schemes differing for the same owner/repo#N.
		"gh:acme/api":       "",
		"file:docs/spec.md": "",
		"jira:AB-12":        "",
		// Case is preserved, never folded: refs are opaque strings (SPEC
		// §4.1), so gh:Acme/API#1 and gh:acme/api#1 are different refs here
		// even though GitHub treats them as one repo. import always mints the
		// API's casing, so the mixed-case miss needs a hand-typed ref.
		"gh:Acme/API#12": "pr:Acme/API#12",
	}
	for in, want := range cases {
		if got := siblingSchemeRef(in); got != want {
			t.Errorf("siblingSchemeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// --even-dropped reuse gained a second ref-binding path, and it returned
// before the note computation — the automated, re-runnable path bound twins
// silently while the one-off path warned. The note now rides on what was
// actually written, whichever path wrote it.
func TestTheNoteFiresWhenEvenDroppedBindsARef(t *testing.T) {
	scratch(t)
	addNodeFull(t, "holder", "pr", "--ref", "pr:a/b#7")
	worker := addNode(t, "worker", "task")
	if code, _, _ := session(t, "drop", worker); code != exitOK {
		t.Fatal("drop failed")
	}

	_, _, stderr := session(t, "add", "worker", "--kind", "task",
		"--ref", "gh:a/b#7", "--if-absent", "--even-dropped")
	if !strings.Contains(stderr, "gh:a/b#7 names the same thing as pr:a/b#7") {
		t.Errorf("--even-dropped bound a twin silently:\n%s", stderr)
	}
}

// A node is not a duplicate of itself: binding the sibling scheme onto the
// node that already carries the other half — the exact state merge produces —
// is convergence, not collision.
func TestBindingTheSiblingOntoTheHolderItselfIsQuiet(t *testing.T) {
	scratch(t)
	addNodeFull(t, "the pr", "pr", "--ref", "pr:a/b#7")

	_, _, stderr := session(t, "add", "the pr", "--kind", "pr",
		"--ref", "gh:a/b#7", "--if-absent", "--even-dropped")
	if strings.Contains(stderr, "note:") {
		t.Errorf("the holder was reported as its own duplicate:\n%s", stderr)
	}
}

// ref add is the one command that can make a ref ambiguous between live
// holders — an exact duplicate means neither resolves — so it prints the same
// note add does, and still binds.
func TestRefAddNamesTheExistingHolder(t *testing.T) {
	scratch(t)
	addNodeFull(t, "holder", "pr", "--ref", "pr:a/b#7")
	other := addNode(t, "other", "task")

	code, _, stderr := session(t, "ref", "add", other, "pr:a/b#7")
	if code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}
	for _, want := range []string{"pr:a/b#7 is already carried by", "holder", "weft merge"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("ref add note missing %q:\n%s", want, stderr)
		}
	}
	// Still bound — the note is presentation, not refusal.
	if code, _, _ := session(t, "ref", "add", other, "pr:a/b#7"); code != exitOK {
		t.Errorf("re-adding should still be exit 0")
	}
}

// A holder that carries both schemes — what merge produces — is one duplicate,
// not two: one note, one hint, however many of its refs matched.
func TestOneNoteAndOneHintForAHolderCarryingBothSchemes(t *testing.T) {
	scratch(t)
	addNodeFull(t, "both", "pr", "--ref", "gh:a/b#1", "--ref", "pr:a/b#1")

	_, _, stderr := session(t, "add", "second", "--kind", "pr", "--ref", "gh:a/b#1")
	if got := strings.Count(stderr, "note:"); got != 1 {
		t.Errorf("want exactly 1 note, got %d:\n%s", got, stderr)
	}
	if got := strings.Count(stderr, "weft merge"); got != 1 {
		t.Errorf("want exactly 1 merge hint, got %d:\n%s", got, stderr)
	}
}

// A merged-away holder's refs live on its survivor, and the note must name
// the survivor — the id a merge command can actually act on.
func TestTheNoteNamesTheSurvivorAfterAMerge(t *testing.T) {
	scratch(t)
	from := addNodeFull(t, "original", "pr", "--ref", "gh:a/b#9")
	into := addNode(t, "survivor", "pr")
	if code, _, stderr := session(t, "merge", into, from); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}

	_, _, stderr := session(t, "add", "late duplicate", "--kind", "pr", "--ref", "gh:a/b#9")
	if !strings.Contains(stderr, "survivor") {
		t.Errorf("the note did not name the survivor:\n%s", stderr)
	}
	if strings.Contains(stderr, "original") {
		t.Errorf("the note named the merged-away node:\n%s", stderr)
	}
}

// The headline safety claim: a plain add --ref still lands when the log
// cannot be folded — the note is presentation, and failing the write for it
// would reopen the retry-duplicate trap BUILD.md §5 closed.
func TestPlainAddWithRefStillLandsOnACorruptLog(t *testing.T) {
	dir := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := session(t, "add", "survives", "--ref", "key:r")
	if code != exitOK {
		t.Fatalf("add --ref on a corrupt log: exit %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "survives") {
		t.Errorf("no confirmation printed:\n%s", stdout)
	}
	if !strings.Contains(logText(t, dir), `"title":"survives"`) {
		t.Errorf("the write did not land")
	}
}
