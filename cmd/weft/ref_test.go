package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// appendRefAdd writes a second, independent assertion of a binding straight to
// the log. It cannot go through the CLI, which dedupes — but another writer
// leaves exactly this, and it is the shape `ref rm` has to handle.
func appendRefAdd(t *testing.T, dir, node, ref string) {
	t.Helper()
	node = fullID(t, node)
	e, err := event.RefAdd(node, ref, "agent:other", event.SourceObserved, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	line, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// SPEC §4 models refs as bindings "added over time as reality catches up", but
// only `add` and `import` could write one — so a node whose issue appeared a
// week later could not be bound without drop-and-re-add, which loses its edges.
func TestRefAddBindsAnExistingNode(t *testing.T) {
	scratch(t)
	id := addNode(t, "issue showed up later", "task")

	code, stdout, stderr := session(t, "ref", "add", id, "gh:acme/api#412")
	if code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "bound to gh:acme/api#412") {
		t.Errorf("ref add said %q", stdout)
	}

	// A binding that does not resolve is not really a binding.
	code, out, _ := session(t, "show", "gh:acme/api#412")
	if code != exitOK || !strings.HasPrefix(out, id) {
		t.Errorf("the new ref does not resolve to the node: exit %d\n%s", code, out)
	}
	if _, shown, _ := session(t, "show", id); !strings.Contains(shown, "gh:acme/api#412") {
		t.Errorf("show omits the new ref:\n%s", shown)
	}
}

func TestRefRmUnbinds(t *testing.T) {
	scratch(t)
	id := addNode(t, "bound then not", "task")
	if code, _, stderr := session(t, "ref", "add", id, "gh:acme/api#412"); code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}
	code, stdout, stderr := session(t, "ref", "rm", id, "gh:acme/api#412")
	if code != exitOK {
		t.Fatalf("ref rm: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "no longer bound") {
		t.Errorf("ref rm said %q", stdout)
	}
	if _, shown, _ := session(t, "show", id); strings.Contains(shown, "gh:acme/api#412") {
		t.Errorf("show still lists the retracted ref:\n%s", shown)
	}
	if code, _, _ := session(t, "show", "gh:acme/api#412"); code != exitNotFound {
		t.Errorf("the retracted ref still resolves: exit %d", code)
	}
}

// Both directions are no-ops when the desired state already holds, and both
// must append nothing. The fold tolerates a redundant ref.add by leaving the
// list unchanged, so a writer that re-emits blindly looks correct in every
// query while growing the log on every scheduled run (EVENTS.md §3.9).
func TestRefNoOpsWriteNothing(t *testing.T) {
	dir := scratch(t)
	id := addNode(t, "idempotent", "task")

	for _, tc := range []struct{ name, verb, want string }{
		{"rm before any binding", "rm", "was not bound"},
		{"add twice", "add", "was already bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.verb == "add" {
				if code, _, stderr := session(t, "ref", "add", id, "gh:o/r#1"); code != exitOK {
					t.Fatalf("setup: exit %d, %s", code, stderr)
				}
			}
			before := logLines(t, dir)
			code, stdout, stderr := session(t, "ref", tc.verb, id, "gh:o/r#1")
			if code != exitOK {
				t.Fatalf("exit %d, %s", code, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("said %q, want it to report the no-op", stdout)
			}
			if after := logLines(t, dir); after != before {
				t.Errorf("appended %d lines to the log; want 0", after-before)
			}
		})
	}
}

// The fold records an entry per ref.add even when the node already carries the
// ref, so two writers leave two live assertions. Retracting one would drop the
// ref from the node while the log still asserted it.
func TestRefRmRetractsEveryAssertion(t *testing.T) {
	dir := scratch(t)
	id := addNode(t, "doubly asserted", "task")
	if code, _, stderr := session(t, "ref", "add", id, "gh:o/r#7"); code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}
	// A second, independent assertion of the same binding, as another writer
	// would leave. The CLI dedupes, so this has to be appended directly.
	appendRefAdd(t, dir, id, "gh:o/r#7")

	if code, _, stderr := session(t, "ref", "rm", id, "gh:o/r#7"); code != exitOK {
		t.Fatalf("ref rm: exit %d, %s", code, stderr)
	}
	if got := strings.Count(logText(t, dir), `"ev":"ref.retract"`); got != 2 {
		t.Errorf("%d ref.retract events, want one per assertion", got)
	}
	if _, shown, _ := session(t, "show", id); strings.Contains(shown, "gh:o/r#7") {
		t.Errorf("the ref survived:\n%s", shown)
	}
	// And the binding is really gone, not merely hidden: re-adding works.
	if code, _, stderr := session(t, "ref", "add", id, "gh:o/r#7"); code != exitOK {
		t.Fatalf("re-add after rm: exit %d, %s", code, stderr)
	}
	if _, shown, _ := session(t, "show", id); !strings.Contains(shown, "gh:o/r#7") {
		t.Errorf("re-added ref is missing:\n%s", shown)
	}
}

func TestRefJSONReportsWhetherItWrote(t *testing.T) {
	scratch(t)
	id := addNode(t, "json flags", "task")

	for _, tc := range []struct {
		name  string
		args  []string
		field string
		want  bool
	}{
		{"add binds", []string{"ref", "add", id, "gh:o/r#3", "--json"}, "bound", true},
		{"add again", []string{"ref", "add", id, "gh:o/r#3", "--json"}, "bound", false},
		{"rm unbinds", []string{"ref", "rm", id, "gh:o/r#3", "--json"}, "retracted", true},
		{"rm again", []string{"ref", "rm", id, "gh:o/r#3", "--json"}, "retracted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, _ := session(t, tc.args...)
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not valid JSON: %v\n%s", err, out)
			}
			if got[tc.field] != tc.want {
				t.Errorf("%s = %v, want %v\n%s", tc.field, got[tc.field], tc.want, out)
			}
			if _, ok := got["nodes"]; !ok {
				t.Errorf("the node is missing from the payload:\n%s", out)
			}
		})
	}
}

func TestRefUsageErrors(t *testing.T) {
	scratch(t)
	id := addNode(t, "usage", "task")

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no subcommand", []string{"ref"}, exitUsage},
		{"unknown subcommand", []string{"ref", "frobnicate", id, "x:1"}, exitUsage},
		{"add missing the ref", []string{"ref", "add", id}, exitUsage},
		{"rm missing the ref", []string{"ref", "rm", id}, exitUsage},
		{"empty ref", []string{"ref", "add", id, ""}, exitUsage},
		{"unknown source", []string{"ref", "add", id, "x:1", "--source", "vibes"}, exitUsage},
		{"unknown node", []string{"ref", "add", "ffffffffff", "x:1"}, exitNotFound},
	} {
		if code, _, _ := session(t, tc.args...); code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.want)
		}
	}
}

// BUILD.md §1.2: flags may follow positionals, and a sub-verb must not break
// the permutation — the flags belong to the sub-command's own FlagSet.
func TestRefParsesAFlagAfterPositionals(t *testing.T) {
	dir := scratch(t)
	id := addNode(t, "permuted", "task")
	if code, _, stderr := session(t, "ref", "add", id, "x:1", "--source", "observed"); code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	if !strings.Contains(logText(t, dir), `"source":"observed"`) {
		t.Error("--source after the positionals was ignored; provenance is wrong")
	}
}

// A ref bound to a node that was later merged has to reach the survivor, since
// after the merge the survivor's id is the one a caller holds.
func TestRefAfterAMerge(t *testing.T) {
	scratch(t)
	survivor := addNode(t, "the real one", "spec")
	merged := addNode(t, "the duplicate", "spec")
	if code, _, stderr := session(t, "merge", survivor, merged); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	// Binding by the merged id lands on the survivor (SPEC.md §6.2).
	if code, _, stderr := session(t, "ref", "add", merged, "gh:o/r#9"); code != exitOK {
		t.Fatalf("ref add by merged id: exit %d, %s", code, stderr)
	}
	if _, shown, _ := session(t, "show", survivor); !strings.Contains(shown, "gh:o/r#9") {
		t.Errorf("the survivor did not gain the ref:\n%s", shown)
	}
	// And it comes off again from either id.
	if code, _, stderr := session(t, "ref", "rm", survivor, "gh:o/r#9"); code != exitOK {
		t.Fatalf("ref rm: exit %d, %s", code, stderr)
	}
	if _, shown, _ := session(t, "show", survivor); strings.Contains(shown, "gh:o/r#9") {
		t.Errorf("the ref survived on the survivor:\n%s", shown)
	}
}

// `ref` is the one command with a subcommand grammar to discover, which makes
// it the one where --help failing costs the most. It used to answer "unknown
// subcommand", sending anyone asking what the verbs are away with the fact that
// they had not named one.
func TestRefHelpPrintsTheGrammarAndTheFlags(t *testing.T) {
	scratch(t)
	// An explicit help request is a success, not a usage error (help used to exit 1):
	// exit 0 and on stdout, across every form so asking for help is
	// distinguishable from a typo. Bare `weft ref` is the different case — a
	// missing subcommand — and stays exit 1 on stderr below.
	for _, args := range [][]string{
		{"ref", "--help"}, {"ref", "-h"}, {"ref", "help"},
	} {
		code, stdout, stderr := session(t, args...)
		if code != exitOK {
			t.Errorf("%v: exit %d, want %d", args, code, exitOK)
		}
		for _, want := range []string{"weft ref add", "weft ref rm", "-source", "-json"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%v: usage omits %q on stdout:\n%s", args, want, stdout)
			}
		}
		if stderr != "" {
			t.Errorf("%v: help wrote to stderr:\n%s", args, stderr)
		}
	}

	// Bare `weft ref` named no subcommand, which is a usage error — exit 1 on
	// stderr — but it still answers the question the reader had by printing the
	// grammar.
	code, _, stderr := session(t, "ref")
	if code != exitUsage {
		t.Errorf("bare ref: exit %d, want %d", code, exitUsage)
	}
	for _, want := range []string{"weft ref add", "weft ref rm"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("bare ref: usage omits %q:\n%s", want, stderr)
		}
	}
}

// A real typo still says so — and now also says what the verbs are, which is
// the question the reader had.
func TestRefUnknownSubcommandStillNamesIt(t *testing.T) {
	scratch(t)
	code, _, stderr := session(t, "ref", "bogus")
	if code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) {
		t.Errorf("typo not named:\n%s", stderr)
	}
	if !strings.Contains(stderr, "weft ref add") {
		t.Errorf("typo message does not go on to say what the verbs are:\n%s", stderr)
	}
}

// Live-preferring resolution (the dropped-node-resurrection fix) made a state reachable that the
// post-write re-resolve mishandled: rm removes the ref from the only live
// holder, the original query then resolves to a dropped holder the command
// never touched, and that node was reported as the one that changed. The
// report must name the node the transaction actually modified.
func TestRefRmReportsTheNodeItActuallyChanged(t *testing.T) {
	scratch(t)
	dropped := addNodeFull(t, "old", "note", "--ref", "key:r")
	if code, _, _ := session(t, "drop", dropped); code != exitOK {
		t.Fatal("drop failed")
	}
	live := addNodeFull(t, "new", "note", "--ref", "key:r")

	code, stdout, stderr := session(t, "ref", "rm", "key:r", "key:r", "--json")
	if code != exitOK {
		t.Fatalf("ref rm: exit %d, %s", code, stderr)
	}
	var out struct {
		Retracted bool `json:"retracted"`
		Nodes     []struct {
			ID    string   `json:"id"`
			Refs  []string `json:"refs"`
			State string   `json:"state"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Retracted || len(out.Nodes) != 1 {
		t.Fatalf("unexpected shape:\n%s", stdout)
	}
	if !strings.HasPrefix(out.Nodes[0].ID, live) {
		t.Errorf("reported %s, want the live node %s that was retracted from:\n%s",
			out.Nodes[0].ID, live, stdout)
	}
	for _, r := range out.Nodes[0].Refs {
		if r == "key:r" {
			t.Errorf("the reported node still carries the ref it was told no longer holds:\n%s", stdout)
		}
	}
}
