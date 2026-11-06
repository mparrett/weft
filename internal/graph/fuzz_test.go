package graph

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mparrett/weft/internal/event"
)

// Fuzzing the fold, because its failures are the quiet kind. A miscanonicalized
// reference does not crash anything: the command exits 0 and the assertion
// lands on a node nothing reads (CLAUDE.md). These targets assert the
// structural invariants that go wrong in that case.
//
// Note what is deliberately *not* asserted: acyclicity. depends-on is kept a DAG
// by CheckEdge at write time, but the fold reports what the log says (SPEC.md
// §9.3), so a hand-written log holding a cycle must fold without complaint.
// Asserting it here would encode a rule the fold does not have.

func foldFuzzSeeds(f *testing.F) {
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
		f.Add(data)
	}
}

// applyAll folds whatever parses and ignores the rest, which is what a reader
// facing a log written by another tool has to do.
func applyAll(t *testing.T, log []byte) *Graph {
	t.Helper()
	g := New()
	for _, line := range bytes.Split(log, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		e, err := event.Parse(line)
		if err != nil {
			continue
		}
		if err := g.Apply(e); err != nil {
			t.Fatalf("Apply rejected a parseable event: %v\n%q", err, line)
		}
	}
	return g
}

// FuzzFoldInvariants pins the structural properties every query depends on.
func FuzzFoldInvariants(f *testing.F) {
	foldFuzzSeeds(f)

	f.Fuzz(func(t *testing.T, log []byte) {
		g := applyAll(t, log)

		// Every edge names survivors. An edge left pointing at a merged node is
		// invisible to anything reading the survivor, and the command that
		// created it still exited 0.
		for id, e := range g.Edges {
			if c := g.Canonical(e.From); c != e.From {
				t.Fatalf("edge %.8s starts at %.8s, which merged into %.8s", id, e.From, c)
			}
			if c := g.Canonical(e.To); c != e.To {
				t.Fatalf("edge %.8s ends at %.8s, which merged into %.8s", id, e.To, c)
			}
			// The triple index and the edge map have to agree, or unlink reports
			// no edge while show still draws one.
			if found := g.FindEdge(e.Kind, e.From, e.To); found == nil {
				t.Fatalf("edge %.8s is in Edges but FindEdge cannot find it", id)
			}
		}

		// Ordered is what gets printed; IDs is what gets resolved. A merged node
		// must stay resolvable and stop being listed, so one is a subset of the
		// other and never the reverse (CLAUDE.md).
		all := make(map[string]bool, len(g.IDs()))
		for _, id := range g.IDs() {
			all[id] = true
		}
		for _, n := range g.Ordered() {
			if !all[n.ID] {
				t.Fatalf("node %.8s is listed by Ordered but not resolvable via IDs", n.ID)
			}
			if n.MergedInto != "" {
				t.Fatalf("node %.8s merged into %.8s but is still listed", n.ID, n.MergedInto)
			}
		}

		// The fold drops a repeated ref rather than appending it twice, so a
		// node's ref list is a set. Duplicates would show twice and be retracted
		// once.
		for _, n := range g.Nodes {
			seen := map[string]bool{}
			for _, r := range n.Refs {
				if seen[r] {
					t.Fatalf("node %.8s carries ref %q twice", n.ID, r)
				}
				seen[r] = true
			}
		}
	})
}

// FuzzFoldIsDeterministic pins that the same log folds to the same graph. Go
// randomizes map iteration, so anything that reads a map to decide output order
// produces a graph that renders differently run to run — which SPEC.md §9.5
// forbids and which nothing else fails on.
func FuzzFoldIsDeterministic(f *testing.F) {
	foldFuzzSeeds(f)

	f.Fuzz(func(t *testing.T, log []byte) {
		a, b := applyAll(t, log), applyAll(t, log)

		if !slices.Equal(a.IDs(), b.IDs()) {
			t.Fatalf("IDs differ between two folds of the same log:\n%v\n%v", a.IDs(), b.IDs())
		}
		oa, ob := a.Ordered(), b.Ordered()
		if len(oa) != len(ob) {
			t.Fatalf("Ordered returned %d nodes then %d", len(oa), len(ob))
		}
		for i := range oa {
			x, y := oa[i], ob[i]
			if x.ID != y.ID || x.Title != y.Title || x.State != y.State ||
				x.NS != y.NS || x.Kind != y.Kind || !slices.Equal(x.Refs, y.Refs) {
				t.Fatalf("node %d differs between folds:\n%+v\n%+v", i, x, y)
			}
		}
	})
}
