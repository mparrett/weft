package graph

import (
	"errors"
	"testing"

	"github.com/mparrett/weft/internal/event"
)

// The live-preferring resolution rule, pinned at the layer it lives: ambiguity in a
// ref counts live nodes only. The CLI tests in cmd/weft cover the flows; these
// pin the boundary cases a refactor of Resolve could silently move.

func TestResolveRefPrefersTheLiveHolder(t *testing.T) {
	f := newFixture(t)
	dropped := f.node("a claim")
	f.ref(dropped, "key:x")
	f.state(dropped, event.StateDropped)
	live := f.node("a claim, re-asserted")
	f.ref(live, "key:x")

	n, err := f.g.Resolve("key:x")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.ID != live {
		t.Errorf("resolved %s, want the live holder %s", n.ID, live)
	}
}

// PreferLive's doc comment claims a node merged into a live survivor counts as
// live. That is true only because Resolve collapses to survivors *before* the
// state check — this is the test that keeps the ordering from being refactored
// away for free.
func TestResolveCountsANodeMergedIntoALiveSurvivorAsLive(t *testing.T) {
	f := newFixture(t)
	dead := f.node("dead claim")
	f.ref(dead, "key:x")
	f.state(dead, event.StateDropped)
	holder := f.node("merged holder")
	f.ref(holder, "key:x")
	survivor := f.node("survivor")
	f.merge(survivor, holder)

	n, err := f.g.Resolve("key:x")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.ID != survivor {
		t.Errorf("resolved %s, want the live survivor %s", n.ID, survivor)
	}
}

// Two dropped holders with no live one stay ambiguous: there is no live node
// to prefer, and choosing among retracted claims is merge's judgment. This is
// the documented boundary of "retraction never poisons a name" (SPEC §6).
func TestResolveTwoDroppedHoldersStayAmbiguous(t *testing.T) {
	f := newFixture(t)
	for _, title := range []string{"take one", "take two"} {
		id := f.node(title)
		f.ref(id, "key:x")
		f.state(id, event.StateDropped)
	}

	_, err := f.g.Resolve("key:x")
	var ambiguous *ErrAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("want ErrAmbiguous, got %v", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("want both dropped holders listed, got %d", len(ambiguous.Candidates))
	}
}

func TestResolveSingleDroppedHolderStillResolves(t *testing.T) {
	f := newFixture(t)
	id := f.node("rejected idea")
	f.ref(id, "key:x")
	f.state(id, event.StateDropped)

	n, err := f.g.Resolve("key:x")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.ID != id {
		t.Errorf("resolved %s, want %s", n.ID, id)
	}
}
