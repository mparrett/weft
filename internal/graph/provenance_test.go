package graph

import (
	"testing"

	"github.com/mparrett/weft/internal/event"
)

// The written-but-unreadable `source` fix at the layer it landed: the fold carries a node's own by and
// source, and no later event revises them (EVENTS.md §3.1). The CLI tests
// check the rendered surfaces; these pin the fold itself.

func TestApplyCarriesNodeProvenance(t *testing.T) {
	f := newFixture(t)
	id := f.apply(event.NodeAdd("ns", "finding", "a guess", "agent:scanner", event.SourceInferred, f.tick())).ID()

	n := f.g.Nodes[id]
	if n.By != "agent:scanner" || n.Source != event.SourceInferred {
		t.Errorf("fold dropped provenance: by=%q source=%q", n.By, n.Source)
	}
}

func TestNodeProvenanceIsNeverRevised(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(event.NodeAdd("ns", "finding", "a record", "agent:first", event.SourceObserved, f.tick()))
	id := ev.ID()

	// A crash-retry replays the identical record; same id, first wins.
	f.apply(ev, nil)
	// Later events about the node carry their own provenance, never the node's.
	f.state(id, event.StateDone)
	f.ref(id, "gh:acme/api#1")

	n := f.g.Nodes[id]
	if n.By != "agent:first" || n.Source != event.SourceObserved {
		t.Errorf("a later event revised the node's provenance: by=%q source=%q", n.By, n.Source)
	}
}

// A merge moves the index, never the assertions: the survivor keeps its own
// provenance — same rule as Created — and the merged node keeps what it said,
// which is what show --json's merged_from reads.
func TestMergeLeavesEachNodesProvenanceInPlace(t *testing.T) {
	f := newFixture(t)
	kept := f.apply(event.NodeAdd("ns", "task", "kept", "agent:alice", event.SourceObserved, f.tick())).ID()
	lost := f.apply(event.NodeAdd("ns", "task", "lost", "agent:bob", event.SourceInferred, f.tick())).ID()
	f.merge(kept, lost)

	if n := f.g.Nodes[kept]; n.By != "agent:alice" || n.Source != event.SourceObserved {
		t.Errorf("merge revised the survivor's provenance: by=%q source=%q", n.By, n.Source)
	}
	if n := f.g.Nodes[lost]; n.By != "agent:bob" || n.Source != event.SourceInferred {
		t.Errorf("merge disturbed the merged node's own provenance: by=%q source=%q", n.By, n.Source)
	}
}
