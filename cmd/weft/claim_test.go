package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The milestone exit condition: two live claims on one node is reported, not
// refused. Claims are advisory, and arbitrating them is not the tool's job
// (SPEC.md §4.3).
func TestTwoLiveClaimsAreReportedNotRefused(t *testing.T) {
	scratch(t)
	a := addNode(t, "batch export", "task")

	if code, _, _ := session(t, "claim", a); code != exitOK {
		t.Fatal("first claim failed")
	}
	code, stdout, stderr := session(t, "claim", a, "--holder", "user:colleague", "--source", "inferred")
	if code != exitOK {
		t.Fatalf("second claim exit %d — collisions must not be refused", code)
	}
	if !strings.Contains(stdout, "claimed by user:colleague") {
		t.Errorf("second claim not recorded:\n%s", stdout)
	}
	// Reported, so a human can see the collision.
	if !strings.Contains(stderr, "also claimed by") {
		t.Errorf("collision not surfaced:\n%s", stderr)
	}

	// Both claims stand side by side, with the inferred one marked as such.
	_, shown, _ := session(t, "show", a)
	for _, want := range []string{"agent:test", "user:colleague", "inferred"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show missing %q:\n%s", want, shown)
		}
	}
}

// The other half of the exit condition: state is derived purely by folding.
// There is no materialized artifact besides the log, and re-folding reproduces
// everything.
func TestStateDerivesOnlyFromTheLog(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "bravo", "task")

	session(t, "claim", a, "--note", "mine")
	session(t, "done", b)
	session(t, "link", a, "depends-on", b)

	// Nothing but the log may exist in the store.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "log.jsonl" {
			t.Errorf("unexpected artifact in the store: %s", entry.Name())
		}
	}

	// Two independent folds of the same bytes agree, field for field.
	_, first, _ := session(t, "ls", "--json")
	_, second, _ := session(t, "ls", "--json")
	if first != second {
		t.Errorf("fold is not deterministic:\n%s\n---\n%s", first, second)
	}

	// And the derived state survives a round trip through the file alone.
	before, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), before, 0o644); err != nil {
		t.Fatal(err)
	}
	_, again, _ := session(t, "ls", "--json")
	if again != first {
		t.Errorf("state changed after rewriting the identical log:\n%s\n---\n%s", first, again)
	}
	if !strings.Contains(again, `"state": "done"`) {
		t.Errorf("node state not recovered from the log:\n%s", again)
	}
}

func TestClaimAndRelease(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")

	session(t, "claim", a)
	if _, mine, _ := session(t, "ls", "--mine"); !strings.Contains(mine, "alpha") {
		t.Errorf("--mine should list the claimed node:\n%s", mine)
	}

	if code, stdout, _ := session(t, "release", a); code != exitOK {
		t.Fatalf("release exit %d: %s", code, stdout)
	}
	if _, mine, _ := session(t, "ls", "--mine"); strings.Contains(mine, "alpha") {
		t.Errorf("--mine still lists a released node:\n%s", mine)
	}
}

// Releasing what is not held is not a failure: the desired state already holds.
func TestReleaseIsIdempotent(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	for i := 0; i < 3; i++ {
		if code, _, stderr := session(t, "release", a); code != exitOK {
			t.Fatalf("release %d: exit %d, %s", i, code, stderr)
		}
	}
}

// Idempotent is the right behaviour, but under --json it left a caller unable
// to tell a release that happened from one that had nothing to do — both being
// exit 0 on purpose. Same shape as unlink.
func TestReleaseJSONReportsWhetherItReleased(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	if code, _, stderr := session(t, "claim", a); code != exitOK {
		t.Fatalf("claim: exit %d, %s", code, stderr)
	}
	for _, want := range []bool{true, false} {
		_, out, _ := session(t, "release", a, "--json")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, out)
		}
		if got["released"] != want {
			t.Errorf("released = %v, want %v\n%s", got["released"], want, out)
		}
	}
}

// Releasing only touches your own claim, never someone else's.
func TestReleaseOnlyReleasesYourClaim(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	session(t, "claim", a)
	session(t, "claim", a, "--holder", "user:colleague", "--source", "inferred")

	session(t, "release", a)
	_, shown, _ := session(t, "show", a)
	if !strings.Contains(shown, "user:colleague") {
		t.Errorf("releasing your claim removed someone else's:\n%s", shown)
	}
	// Exactly one claim remains. Checking for the actor string alone would be
	// wrong: it still appears as the asserter of the inferred claim.
	if n := strings.Count(shown, "\n    "); n != 1 {
		t.Errorf("expected exactly one remaining claim:\n%s", shown)
	}
}

// An expired claim means the work was taken and abandoned, which is different
// from never having been claimed.
func TestStaleIsAbandonedNotUnclaimed(t *testing.T) {
	scratch(t)
	abandoned := addNode(t, "abandoned work", "task")
	addNode(t, "untouched work", "task") // never claimed
	held := addNode(t, "held work", "task")

	session(t, "claim", abandoned, "--ttl", "1ns")
	session(t, "claim", held)

	_, stale, _ := session(t, "ls", "--stale")
	if !strings.Contains(stale, "abandoned work") {
		t.Errorf("expired claim should be stale:\n%s", stale)
	}
	if strings.Contains(stale, "untouched work") {
		t.Errorf("never-claimed node is unclaimed, not stale:\n%s", stale)
	}
	if strings.Contains(stale, "held work") {
		t.Errorf("live claim listed as stale:\n%s", stale)
	}
	// An expired claim is not a live one.
	if _, mine, _ := session(t, "ls", "--mine"); strings.Contains(mine, "abandoned") {
		t.Errorf("expired claim still counted as mine:\n%s", mine)
	}
}

func TestNoExpiryClaimNeverGoesStale(t *testing.T) {
	scratch(t)
	a := addNode(t, "forever", "task")
	session(t, "claim", a, "--no-expiry")

	if _, stale, _ := session(t, "ls", "--stale"); strings.Contains(stale, "forever") {
		t.Errorf("--no-expiry claim went stale:\n%s", stale)
	}
	if _, mine, _ := session(t, "ls", "--mine"); !strings.Contains(mine, "forever") {
		t.Errorf("--no-expiry claim should still be live:\n%s", mine)
	}
}

// holder is the subject, by is whoever asserted it. Collapsing them would lose
// the ability to tell an observation from a guess about a third party.
func TestHolderAndActorAreDistinct(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "alpha", "task")
	session(t, "claim", a, "--holder", "user:colleague", "--source", "inferred")

	data, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["ev"] != "claim.add" {
			continue
		}
		found = true
		if ev["holder"] != "user:colleague" {
			t.Errorf("holder = %v", ev["holder"])
		}
		if ev["by"] != "agent:test" {
			t.Errorf("by = %v, want the asserting actor", ev["by"])
		}
		if ev["source"] != "inferred" {
			t.Errorf("source = %v", ev["source"])
		}
	}
	if !found {
		t.Fatal("no claim.add event written")
	}
}

// "me" is CLI sugar and must be resolved before it reaches the log
// (EVENTS.md §4).
func TestMeNeverReachesTheLog(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "alpha", "task")
	session(t, "claim", a, "--holder", "me")

	data, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if strings.Contains(string(data), `"me"`) {
		t.Errorf("literal \"me\" was written to the log:\n%s", data)
	}
}

func TestDoneDropAndUndo(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")

	stateOf := func() string {
		_, out, _ := session(t, "ls", "--json")
		var res struct {
			Nodes []struct{ State string } `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Nodes) != 1 {
			t.Fatalf("unexpected ls output: %v\n%s", err, out)
		}
		return res.Nodes[0].State
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"done", a}, "done"},
		{[]string{"done", a, "--undo"}, "open"},
		{[]string{"drop", a}, "dropped"},
		{[]string{"drop", a, "--undo"}, "open"},
	} {
		if code, _, stderr := session(t, tc.args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", tc.args, code, stderr)
		}
		if got := stateOf(); got != tc.want {
			t.Errorf("%v: state = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestClaimUsageErrors(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"claim no target", []string{"claim"}, exitUsage},
		{"claim two targets", []string{"claim", a, a}, exitUsage},
		{"claim unknown node", []string{"claim", "ffffff"}, exitNotFound},
		{"claim bad source", []string{"claim", a, "--source", "vibes"}, exitUsage},
		{"claim zero ttl", []string{"claim", a, "--ttl", "0"}, exitUsage},
		{"release no target", []string{"release"}, exitUsage},
		{"release unknown node", []string{"release", "ffffff"}, exitNotFound},
		{"done no target", []string{"done"}, exitUsage},
		{"done unknown node", []string{"done", "ffffff"}, exitNotFound},
		{"drop two targets", []string{"drop", a, a}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := session(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

// Two distinct assertions made in the same second must both survive.
//
// Event ids hash the record including "at", and the fold treats a repeated id
// as a crashed-and-retried write. At second granularity these collided and the
// second was silently discarded (EVENTS.md §2.5).
func TestRapidIdenticalAssertionsAreNotDeduplicated(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "alpha", "task")

	// Four state changes, back to back, two of them identical in every field
	// but the timestamp.
	for _, args := range [][]string{
		{"done", a}, {"done", a, "--undo"}, {"drop", a}, {"drop", a, "--undo"},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	states := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["ev"] != "node.state" {
			continue
		}
		states++
		ids[ev["id"].(string)] = true
	}
	if states != 4 {
		t.Fatalf("wrote %d node.state events, want 4", states)
	}
	if len(ids) != 4 {
		t.Errorf("%d distinct ids for 4 events — identical timestamps collided", len(ids))
	}

	// And the last assertion is the one that wins.
	_, out, _ := session(t, "ls", "--json")
	if !strings.Contains(out, `"state": "open"`) {
		t.Errorf("final state lost to deduplication:\n%s", out)
	}
}
