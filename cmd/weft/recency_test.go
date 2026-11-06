package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "90m", want: 90 * time.Minute},
		{in: "72h", want: 72 * time.Hour},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		// Fractional, because "half a day" is a thing people mean.
		{in: "1.5d", want: 36 * time.Hour},
		{in: "0d", want: 0},
		{in: "", bad: true},
		{in: "yesterday", bad: true},
		{in: "7", bad: true},
		{in: "-3d", bad: true},
		{in: "-2h", bad: true},
		{in: "d", bad: true},
		// ParseFloat accepts these, and NaN fails every comparison, so `n < 0`
		// does not reject it — "nanw" would silently become a zero window.
		{in: "nanw", bad: true},
		{in: "infd", bad: true},
		{in: "-infd", bad: true},
		// Finite as a float, but not as a time.Duration. Accepting this wraps to
		// a negative window and moves --merged-since into the future.
		{in: "1e100d", bad: true},
	}
	for _, tc := range cases {
		got, err := parseSince("--since", tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseSince(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSince(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// backdate appends a node.add stamped in the past, which is what a backfilled
// import or a machine with a skewed clock writes. Nothing the CLI can do
// produces one, and recency is untestable without it.
func backdate(t *testing.T, dir, title string, at time.Time) string {
	t.Helper()
	e, err := event.NodeAdd("default", "task", title, "user:test", event.SourceAsserted, at)
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
	return e.ID()
}

func TestLsSinceFiltersByRecency(t *testing.T) {
	dir := scratch(t)
	backdate(t, dir, "stale work", time.Now().Add(-30*24*time.Hour))
	if code, _, stderr := session(t, "add", "fresh work"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}

	code, stdout, stderr := session(t, "ls", "--since", "7d")
	if code != exitOK {
		t.Fatalf("ls --since: exit %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "fresh work") {
		t.Errorf("--since 7d dropped a node touched today:\n%s", stdout)
	}
	if strings.Contains(stdout, "stale work") {
		t.Errorf("--since 7d listed a node last touched 30d ago:\n%s", stdout)
	}

	// A window wide enough for both is the control: without it, a filter that
	// excluded everything would pass the assertions above.
	if _, stdout, _ = session(t, "ls", "--since", "90d"); !strings.Contains(stdout, "stale work") {
		t.Errorf("--since 90d should list both nodes:\n%s", stdout)
	}
}

// The narrowest possible window must not be the one that disables the filter.
// Guarding on a positive duration made `--since 0d` list the whole namespace —
// the failure this repo cares about, where the command succeeds and does the
// wrong thing.
func TestZeroWindowMatchesNothing(t *testing.T) {
	scratch(t)
	if code, _, stderr := session(t, "add", "fresh work"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}
	for _, w := range []string{"0d", "0h", "0s"} {
		code, stdout, _ := session(t, "ls", "--since", w)
		if code != exitOK {
			t.Errorf("--since %s: exit %d", w, code)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("--since %s listed nodes, want an empty listing:\n%s", w, stdout)
		}
	}
}

// A future-dated node has a negative age, which falls inside every window
// including the narrowest — so the one node whose recency cannot be trusted
// would appear in every recency query.
func TestFutureDatedNodeIsOutsideEveryWindow(t *testing.T) {
	dir := scratch(t)
	skewed := backdate(t, dir, "clock is fast", time.Now().Add(365*24*time.Hour))
	if code, _, stderr := session(t, "add", "fresh work"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}

	for _, w := range []string{"1h", "7d", "9999d"} {
		_, stdout, _ := session(t, "ls", "--since", w)
		if strings.Contains(stdout, "clock is fast") {
			t.Errorf("--since %s listed a future-dated node:\n%s", w, stdout)
		}
	}
	// It is still a node, still listed, and still shown by an unfiltered ls.
	if _, stdout, _ := session(t, "ls"); !strings.Contains(stdout, "clock is fast") {
		t.Errorf("a future-dated node vanished from an unfiltered listing:\n%s", stdout)
	}
	// And show names the condition rather than calling it fresh. Asserted on
	// the id, not a title — Resolve takes an id, prefix, or ref, so a title
	// would resolve to nothing and the assertion would pass vacuously.
	code, stdout, stderr := session(t, "show", skewed)
	if code != exitOK {
		t.Fatalf("show %s: exit %d, stderr %s", skewed, code, stderr)
	}
	if !strings.Contains(stdout, "in the future") {
		t.Errorf("show did not name the future timestamp:\n%s", stdout)
	}
	if strings.Contains(stdout, "just now") {
		t.Errorf("show rendered a future stamp as fresh:\n%s", stdout)
	}
}

// Recency is what the node has *done*, not when it was asserted. A node created
// long ago and worked on today belongs at the top.
func TestLsSinceTracksWorkNotCreation(t *testing.T) {
	dir := scratch(t)
	old := backdate(t, dir, "long-running work", time.Now().Add(-30*24*time.Hour))

	if _, stdout, _ := session(t, "ls", "--since", "7d"); strings.Contains(stdout, "long-running") {
		t.Fatalf("precondition: node should start outside the window:\n%s", stdout)
	}

	if code, _, stderr := session(t, "done", old); code != exitOK {
		t.Fatalf("done: exit %d, stderr %s", code, stderr)
	}

	if _, stdout, _ := session(t, "ls", "--since", "7d"); !strings.Contains(stdout, "long-running") {
		t.Errorf("work done today did not bring an old node into the window:\n%s", stdout)
	}
}

func TestLsRecentSortsNewestFirst(t *testing.T) {
	dir := scratch(t)
	backdate(t, dir, "oldest", time.Now().Add(-30*24*time.Hour))
	backdate(t, dir, "middle", time.Now().Add(-2*24*time.Hour))
	if code, _, stderr := session(t, "add", "newest"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}

	code, stdout, stderr := session(t, "ls", "--recent")
	if code != exitOK {
		t.Fatalf("ls --recent: exit %d, stderr %s", code, stderr)
	}
	want := []string{"newest", "middle", "oldest"}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		for _, w := range want {
			if strings.Contains(line, w) {
				got = append(got, w)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("--recent ordered %v, want %v:\n%s", got, want, stdout)
	}
}

// Two orderings cannot both apply, and silently honouring one would make the
// other flag look broken rather than rejected.
func TestLsRejectsConflictingSorts(t *testing.T) {
	scratch(t)
	if code, _, _ := session(t, "ls", "--recent", "--sort"); code != exitUsage {
		t.Errorf("--recent with --sort: exit %d, want %d", code, exitUsage)
	}
}

func TestLsRejectsBadSince(t *testing.T) {
	scratch(t)
	code, _, stderr := session(t, "ls", "--since", "yesterday")
	if code != exitUsage {
		t.Errorf("--since yesterday: exit %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "7d") {
		t.Errorf("error should show an accepted spelling, got: %s", stderr)
	}
}

// Flags may follow positionals (BUILD.md §1.2), and a value-taking flag is the
// case where getting that wrong is silent: --since would be dropped and the
// listing would quietly cover everything.
func TestSinceParsesAfterPositionals(t *testing.T) {
	dir := scratch(t)
	backdate(t, dir, "stale work", time.Now().Add(-30*24*time.Hour))
	if code, _, stderr := session(t, "add", "fresh work"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}

	_, stdout, _ := session(t, "ls", "--kind", "task", "--since", "7d")
	if strings.Contains(stdout, "stale work") {
		t.Errorf("--since ignored when it follows another flag's value:\n%s", stdout)
	}
}

// Machine output carries absolute timestamps, never the relative form: a
// consumer stores what it reads, and "3d ago" is only true when it was printed.
func TestJSONCarriesAbsoluteTimestamps(t *testing.T) {
	scratch(t)
	if code, _, stderr := session(t, "add", "dated work"); code != exitOK {
		t.Fatalf("add: exit %d, stderr %s", code, stderr)
	}
	for _, args := range [][]string{{"ls", "--json"}, {"export", "--json"}} {
		_, stdout, _ := session(t, args...)
		if !strings.Contains(stdout, `"created"`) || !strings.Contains(stdout, `"touched"`) {
			t.Errorf("%v output lacks timestamps:\n%s", args, stdout)
		}
		if strings.Contains(stdout, "ago") {
			t.Errorf("%v output carries a relative age:\n%s", args, stdout)
		}
	}
}
