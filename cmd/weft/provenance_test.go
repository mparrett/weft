package main

import (
	"strings"
	"testing"
)

// The written-but-unreadable `source` bug: `add --source` was written to the log and then dropped by the
// fold, so an agent told to mark its guesses had no interface that could read
// the mark back. These tests pin the round trip: log → fold → show/export.

func TestNodeSourceSurvivesTheFold(t *testing.T) {
	scratch(t)
	id := addNodeFull(t, "a guess I made", "finding", "--source", "inferred")

	code, stdout, stderr := session(t, "show", id)
	if code != exitOK {
		t.Fatalf("show: exit %d, %s", code, stderr)
	}
	// Same vocabulary claims use, so one reading works for both.
	if !strings.Contains(stdout, "source inferred, per agent:test") {
		t.Errorf("show does not surface the node's provenance:\n%s", stdout)
	}

	_, stdout, _ = session(t, "show", id, "--json")
	for _, want := range []string{`"source": "inferred"`, `"by": "agent:test"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show --json missing %s:\n%s", want, stdout)
		}
	}
}

// The decision the by/source fold fix had to state: source belongs to the node.add and
// later events never revise it, the way Created never moves while Touched does.
func TestNodeSourceIsNotRevisedByLaterEvents(t *testing.T) {
	scratch(t)
	id := addNodeFull(t, "a record I read", "finding", "--source", "observed")
	if code, _, stderr := session(t, "done", id); code != exitOK {
		t.Fatalf("done: exit %d, %s", code, stderr)
	}
	if code, _, stderr := session(t, "ref", "add", id, "gh:acme/api#1"); code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}

	_, stdout, _ := session(t, "show", id)
	if !strings.Contains(stdout, "source observed, per agent:test") {
		t.Errorf("later events revised the node's provenance:\n%s", stdout)
	}
}

// The default export's bytes are pinned by golden files, so the node's
// provenance rides under the existing --include source, beside the edges'.
func TestExportCarriesNodeProvenanceOnlyOnRequest(t *testing.T) {
	scratch(t)
	addNodeFull(t, "a guess I made", "finding", "--source", "inferred")

	_, stdout, _ := session(t, "export", "--json")
	if strings.Contains(stdout, `"source"`) || strings.Contains(stdout, `"by"`) {
		t.Errorf("default export leaks provenance:\n%s", stdout)
	}

	_, stdout, _ = session(t, "export", "--json", "--include", "source")
	for _, want := range []string{`"source": "inferred"`, `"by": "agent:test"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("export --include source missing %s:\n%s", want, stdout)
		}
	}
}

// A merged node's provenance has exactly one reader: the survivor's
// merged_from. Resolution redirects the merged id and listings omit merged
// nodes, so without this the losing node's by/source would be written to the
// log and readable through nothing — the same shape, one level down.
func TestMergedFromCarriesTheMergedNodesProvenance(t *testing.T) {
	scratch(t)
	kept := addNodeFull(t, "kept", "task", "--source", "observed")
	t.Setenv("WEFT_ACTOR", "agent:bob")
	lost := addNodeFull(t, "lost", "task", "--source", "inferred")
	t.Setenv("WEFT_ACTOR", "agent:test")
	if code, _, stderr := session(t, "merge", kept, lost); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}

	_, stdout, _ := session(t, "show", kept, "--json")
	if !strings.Contains(stdout, `"merged_from"`) {
		t.Fatalf("no merged_from block:\n%s", stdout)
	}
	for _, want := range []string{`"by": "agent:bob"`, `"source": "inferred"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("merged_from missing the merged node's %s:\n%s", want, stdout)
		}
	}
	// And the survivor's own provenance is unrevised by the merge.
	if !strings.Contains(stdout, `"source": "observed"`) {
		t.Errorf("the survivor's provenance moved:\n%s", stdout)
	}
}
