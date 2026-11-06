package graph

import (
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

func claimEvent(t *testing.T, node, holder, actor, source string, expires time.Time) event.Event {
	t.Helper()
	e, err := event.ClaimAdd(node, holder, "", actor, source, fixedTime(), expires)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestClaimLiveness(t *testing.T) {
	now := fixedTime()
	cases := []struct {
		name    string
		expires time.Time
		live    bool
	}{
		{"no expiry never ages", time.Time{}, true},
		{"future expiry is live", now.Add(time.Hour), true},
		{"past expiry is not", now.Add(-time.Hour), false},
		{"expiring exactly now is not live", now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Claim{Expires: tc.expires}
			if got := c.Live(now); got != tc.live {
				t.Errorf("Live = %v, want %v", got, tc.live)
			}
			if c.Expired(now) == tc.live {
				t.Error("Expired must be the negation of Live")
			}
		})
	}
}

// Claims are advisory. Two live claims on one node is a condition to report,
// never something the fold resolves (SPEC.md §4.3).
func TestTwoLiveClaimsCoexist(t *testing.T) {
	now := fixedTime()
	n := node(t, "contested")
	g := build(t, n,
		claimEvent(t, n.ID(), "user:matt", "user:matt", event.SourceAsserted, now.Add(time.Hour)),
		claimEvent(t, n.ID(), "user:colleague", "agent:scanner", event.SourceInferred, now.Add(time.Hour)),
	)
	if got := len(g.LiveClaims(n.ID(), now)); got != 2 {
		t.Fatalf("got %d live claims, want 2", got)
	}
	// Each is attributable to whoever asserted it, not to its subject.
	for _, c := range g.LiveClaims(n.ID(), now) {
		if c.Holder == "user:colleague" && c.By != "agent:scanner" {
			t.Errorf("inferred claim lost its asserter: by = %q", c.By)
		}
	}
}

func TestExpiredClaimStillVisible(t *testing.T) {
	now := fixedTime()
	n := node(t, "abandoned")
	g := build(t, n, claimEvent(t, n.ID(), "agent:gone", "agent:gone", event.SourceAsserted, now.Add(-time.Hour)))

	if got := len(g.LiveClaims(n.ID(), now)); got != 0 {
		t.Errorf("expired claim counted as live")
	}
	// Hiding it would make abandoned work look identical to unclaimed work.
	if got := len(g.ClaimsFor(n.ID())); got != 1 {
		t.Errorf("expired claim disappeared from the fold")
	}
}

func TestClaimReleaseRemovesOnlyThatClaim(t *testing.T) {
	now := fixedTime()
	n := node(t, "contested")
	mine := claimEvent(t, n.ID(), "user:matt", "user:matt", event.SourceAsserted, now.Add(time.Hour))
	theirs := claimEvent(t, n.ID(), "user:colleague", "user:colleague", event.SourceAsserted, now.Add(time.Hour))
	release, err := event.ClaimRelease(mine.ID(), "user:matt", event.SourceAsserted, now)
	if err != nil {
		t.Fatal(err)
	}

	g := build(t, n, mine, theirs, release)
	live := g.LiveClaims(n.ID(), now)
	if len(live) != 1 {
		t.Fatalf("got %d live claims, want 1", len(live))
	}
	if live[0].Holder != "user:colleague" {
		t.Errorf("wrong claim released: %s survived", live[0].Holder)
	}
}

// Releasing an unknown claim is a no-op, like every other retraction.
func TestClaimReleaseUnknownTargetIsNoOp(t *testing.T) {
	now := fixedTime()
	n := node(t, "x")
	release, err := event.ClaimRelease("0000000000000000000000000000000000000000000000000000000000000000",
		"user:matt", event.SourceAsserted, now)
	if err != nil {
		t.Fatal(err)
	}
	g := build(t, n, release)
	if len(g.Claims) != 0 {
		t.Errorf("unexpected claims: %d", len(g.Claims))
	}
}

// A claim on a node we have not seen asserted is retained, dangling.
func TestClaimOnUnknownNodeRetained(t *testing.T) {
	now := fixedTime()
	missing := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	g := build(t, claimEvent(t, missing, "user:matt", "user:matt", event.SourceAsserted, now.Add(time.Hour)))
	if got := len(g.LiveClaims(missing, now)); got != 1 {
		t.Errorf("claim on an unknown node was dropped")
	}
}

func TestClaimBy(t *testing.T) {
	now := fixedTime()
	n := node(t, "x")
	g := build(t, n, claimEvent(t, n.ID(), "user:matt", "user:matt", event.SourceAsserted, now.Add(time.Hour)))

	if g.ClaimBy(n.ID(), "user:matt", now) == nil {
		t.Error("holder's own claim not found")
	}
	if g.ClaimBy(n.ID(), "user:someone-else", now) != nil {
		t.Error("found a claim for the wrong holder")
	}
	// An expired claim is not held by anyone.
	if g.ClaimBy(n.ID(), "user:matt", now.Add(2*time.Hour)) != nil {
		t.Error("expired claim still reported as held")
	}
}
