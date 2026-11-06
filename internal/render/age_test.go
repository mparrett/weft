package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/graph"
)

// dated returns the sample nodes with fixed timestamps, so age output is
// golden-safe: an age is relative to now, and only a fixed now makes it pin.
func dated(now time.Time) []*graph.Node {
	nodes := sample()
	// Created long ago, touched recently: the two-part line.
	nodes[0].Created = now.Add(-30 * 24 * time.Hour)
	nodes[0].Touched = now.Add(-3 * time.Hour)
	// Never revisited: Touched adds nothing and is dropped.
	nodes[1].Created = now.Add(-2 * 24 * time.Hour)
	nodes[1].Touched = nodes[1].Created
	// Hand-built, undated: no age line at all.
	return nodes
}

func TestNodeAgeGolden(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	for _, n := range dated(now) {
		if err := NodeObserved(&buf, n, MinWidth, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	golden(t, "node_age.txt", buf.Bytes())
}

// The undated case is what every hand-built graph looks like, and rendering a
// zero time as an age reads as "created 56 years ago" rather than "unknown".
func TestNodeAgeOmittedWhenUndated(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := NodeObserved(&buf, sample()[0], MinWidth, nil, now); err != nil {
		t.Fatal(err)
	}
	// Matched as the field label, not a bare substring — "agent:" in the
	// provenance line contains "age".
	if strings.Contains(buf.String(), "\n  age ") {
		t.Errorf("undated node rendered an age line:\n%s", buf.String())
	}
}

// Touched is dropped when it equals Created, or a node nobody has revisited
// prints the same age twice and the second copy reads as a second fact.
func TestNodeAgeDropsRedundantTouched(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	n := sample()[0]
	n.Created = now.Add(-2 * 24 * time.Hour)
	n.Touched = n.Created

	if got := nodeAge(n, now); strings.Contains(got, "touched") {
		t.Errorf("age = %q, want no touched clause when it equals created", got)
	}
	n.Touched = now.Add(-time.Hour)
	if got := nodeAge(n, now); !strings.Contains(got, "touched 1h ago") {
		t.Errorf("age = %q, want a touched clause once it differs", got)
	}
}

// Touched never decreases, so a machine with a fast clock leaves a permanently
// future timestamp in the log. The naive duration switch matches a negative
// duration against the "just now" arm and renders it as permanently fresh —
// worse than visibly broken, because nothing about it looks wrong.
func TestAgeNamesTheFuture(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, ahead := range []time.Duration{time.Second, time.Hour, 365 * 24 * time.Hour} {
		if got := Age(now.Add(ahead), now); got != "in the future" {
			t.Errorf("a stamp %v ahead rendered %q, want %q", ahead, got, "in the future")
		}
	}
	// The boundary still belongs to "just now", not to the future arm.
	if got := Age(now, now); got != "just now" {
		t.Errorf("now rendered %q, want %q", got, "just now")
	}
}

func TestSortRecentNewestFirst(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	nodes := sample()
	nodes[0].Touched = now.Add(-10 * 24 * time.Hour)
	nodes[1].Touched = now.Add(-time.Hour)
	nodes[2].Touched = now.Add(-3 * 24 * time.Hour)

	SortRecent(nodes)

	want := []string{"b", "c", "a"}
	for i, w := range want {
		if !strings.HasPrefix(nodes[i].ID, w) {
			t.Fatalf("position %d is %s..., want %s...", i, nodes[i].ID[:1], w)
		}
	}
}

// An undated node sorts last rather than first, which is where the zero time
// would otherwise put it — and last is where "we have no evidence it moved"
// belongs in a list of what moved.
func TestSortRecentPutsUndatedLast(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	nodes := sample()
	nodes[0].Touched = time.Time{}
	nodes[1].Touched = now.Add(-time.Hour)
	nodes[2].Touched = now.Add(-24 * time.Hour)

	SortRecent(nodes)

	if !strings.HasPrefix(nodes[2].ID, "a") {
		t.Errorf("undated node sorted to position %d, want last", indexOf(nodes, "a"))
	}
}

// Two nodes sharing a timestamp is ordinary: `at` is a wall clock with finite
// resolution, and a batch write stamps close together. File order breaks the
// tie, which is what keeps identical graphs printing identically (SPEC.md §9.5).
func TestSortRecentIsStableOnTies(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	nodes := sample()
	for _, n := range nodes {
		n.Touched = now
	}

	SortRecent(nodes)

	for i, want := range []string{"a", "b", "c"} {
		if !strings.HasPrefix(nodes[i].ID, want) {
			t.Fatalf("tied nodes reordered: position %d is %s..., want file order %s...",
				i, nodes[i].ID[:1], want)
		}
	}
}

func indexOf(nodes []*graph.Node, prefix string) int {
	for i, n := range nodes {
		if strings.HasPrefix(n.ID, prefix) {
			return i
		}
	}
	return -1
}
