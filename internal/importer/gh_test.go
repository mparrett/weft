package importer

import (
	"errors"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in     string
		repo   string
		number int
		bad    bool
	}{
		{in: "acme/api", repo: "acme/api"},
		{in: "acme/api#508", repo: "acme/api", number: 508},
		{in: " acme/api#508 ", repo: "acme/api", number: 508},
		{in: "acme", bad: true},
		{in: "acme/api/extra", bad: true},
		{in: "/api", bad: true},
		{in: "acme/", bad: true},
		{in: "acme/api#abc", bad: true},
		{in: "acme/api#0", bad: true},
		{in: "acme/api#-3", bad: true},
	}
	for _, tc := range cases {
		repo, number, err := ParseTarget(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: want an error, got %s#%d", tc.in, repo, number)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if repo != tc.repo || number != tc.number {
			t.Errorf("%q: got %s#%d, want %s#%d", tc.in, repo, number, tc.repo, tc.number)
		}
	}
}

func TestRefScheme(t *testing.T) {
	if got := (Item{Number: 412}).Ref("acme/api"); got != "gh:acme/api#412" {
		t.Errorf("issue ref = %q", got)
	}
	if got := (Item{Number: 77, IsPR: true}).Ref("acme/api"); got != "pr:acme/api#77" {
		t.Errorf("PR ref = %q", got)
	}
}

// gh reports OPEN/CLOSED/MERGED in some versions and lowercase in others.
// Normalizing at the boundary is what keeps a re-import from writing a
// spurious observation every time the gh version changes.
func TestStateNormalizes(t *testing.T) {
	items, err := decode([]byte(`[{"number":1,"title":"a","state":"OPEN"},
		{"number":2,"title":"b","state":"Closed"}]`), false)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].State != "open" || items[1].State != "closed" {
		t.Errorf("states not normalized: %+v", items)
	}
}

func TestListFetchesIssuesAndPRsInTwoCalls(t *testing.T) {
	var calls [][]string
	run := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pr" {
			return []byte(`[{"number":77,"title":"fix pagination","state":"open"}]`), nil
		}
		return []byte(`[{"number":412,"title":"N+1","state":"open"}]`), nil
	}

	items, _, err := List(run, "acme/api", Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Errorf("made %d gh calls, want 2", len(calls))
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].IsPR || !items[1].IsPR {
		t.Errorf("PR flag not set from which list it came from: %+v", items)
	}
}

// A merged window is a third fetch, and it has to be a *search* — filtering
// locally would silently drop a long-lived branch merged yesterday, because
// `gh pr list` orders by creation date.
func TestListFetchesMergedWindowAsASearch(t *testing.T) {
	var calls [][]string
	run := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pr" && contains(args, "merged") {
			return []byte(`[{"number":70,"title":"landed","state":"MERGED"}]`), nil
		}
		return []byte(`[]`), nil
	}

	since := time.Date(2026, 8, 4, 17, 30, 0, 0, time.FixedZone("PDT", -7*3600))
	items, _, err := List(run, "acme/api", Query{Limit: 200, MergedSince: since})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("made %d gh calls, want 3", len(calls))
	}
	// The qualifier is UTC, so the window does not move with the machine's zone.
	if got := calls[2]; !contains(got, "--search") || !contains(got, "merged:>=2026-08-05T00:30:00Z") {
		t.Errorf("merged window was not sent as a UTC search qualifier: %v", got)
	}
	if got := calls[2]; !contains(got, "201") {
		t.Errorf("merged window did not ask for the truncation sentinel: %v", got)
	}
	if len(items) != 1 || !items[0].IsPR || items[0].State != "merged" {
		t.Errorf("merged PR did not come back as a merged PR: %+v", items)
	}
}

// The two windows are truncated independently: which one hit the limit decides
// which remedy the caller is told about.
func TestListReportsWhichFetchWasTruncated(t *testing.T) {
	run := func(args ...string) ([]byte, error) {
		if args[0] == "pr" && contains(args, "merged") {
			return []byte(`[{"number":70,"title":"a","state":"MERGED"},
				{"number":71,"title":"b","state":"MERGED"},
				{"number":72,"title":"c","state":"MERGED"}]`), nil
		}
		return []byte(`[]`), nil
	}
	items, cut, err := List(run, "acme/api", Query{Limit: 2, MergedSince: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if cut.Open || !cut.Merged {
		t.Errorf("truncation = %+v, want the merged window alone", cut)
	}
	if len(items) != 2 {
		t.Errorf("returned %d items past the public limit, want 2", len(items))
	}
}

func TestListNeedsASentinelToCallAWindowTruncated(t *testing.T) {
	run := func(args ...string) ([]byte, error) {
		if args[0] == "pr" && contains(args, "merged") {
			return []byte(`[{"number":70,"title":"a","state":"MERGED"},
				{"number":71,"title":"b","state":"MERGED"}]`), nil
		}
		return []byte(`[]`), nil
	}
	_, cut, err := List(run, "acme/api", Query{Limit: 2, MergedSince: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if cut.Merged {
		t.Error("exactly Limit items do not prove that the window was truncated")
	}
}

func TestListRejectsANonPositiveLimitBeforeCallingGH(t *testing.T) {
	called := false
	_, _, err := List(func(args ...string) ([]byte, error) {
		called = true
		return []byte(`[]`), nil
	}, "acme/api", Query{Limit: 0})
	if err == nil {
		t.Fatal("zero limit was accepted")
	}
	if called {
		t.Error("gh was called before the query was validated")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestListPropagatesUnavailable(t *testing.T) {
	run := func(args ...string) ([]byte, error) {
		return nil, &ErrUnavailable{Err: errors.New("exit status 4"), Detail: "gh auth login"}
	}
	_, _, err := List(run, "acme/api", Query{Limit: 200})
	var unavailable *ErrUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("want ErrUnavailable, got %T: %v", err, err)
	}
	if got := unavailable.Error(); got != "gh unavailable: gh auth login" {
		t.Errorf("gh's own diagnostic was lost: %q", got)
	}
}

// PR-ness comes from the payload carrying a pull_request key, never from a
// second call having failed. One `gh api` call, one unambiguous answer.
func TestOneDistinguishesIssueFromPR(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		isPR  bool
		state string
	}{
		{"plain issue", `{"number":412,"title":"N+1","state":"open"}`, false, "open"},
		{"open pr", `{"number":77,"title":"fix","state":"open","pull_request":{"merged_at":null}}`, true, "open"},
		{"merged pr", `{"number":78,"title":"fix","state":"closed","pull_request":{"merged_at":"2026-07-31T09:00:00Z"}}`, true, "merged"},
		{"closed unmerged pr", `{"number":79,"title":"fix","state":"closed","pull_request":{"merged_at":null}}`, true, "closed"},
	}
	for _, tc := range cases {
		run := func(args ...string) ([]byte, error) { return []byte(tc.body), nil }
		item, err := One(run, "acme/api", 1)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if item.IsPR != tc.isPR || item.State != tc.state {
			t.Errorf("%s: got IsPR=%v state=%q, want %v %q", tc.name, item.IsPR, item.State, tc.isPR, tc.state)
		}
	}

	// A single call means an infrastructure failure surfaces as one, instead of
	// being mistaken for "not a pull request" and minting the wrong ref scheme.
	failing := func(args ...string) ([]byte, error) {
		return nil, &ErrUnavailable{Err: errors.New("exit 1"), Detail: "API rate limit exceeded"}
	}
	if _, err := One(failing, "acme/api", 42); err == nil {
		t.Fatal("a rate limit was swallowed; it must not be read as \"not a PR\"")
	}

	// Unreadable JSON from a gh that ran is a gh problem, so it must classify
	// as unavailable (exit 7), not fall through to a store error (exit 6).
	garbage := func(args ...string) ([]byte, error) { return []byte("not json"), nil }
	var unavailable *ErrUnavailable
	if _, err := One(garbage, "acme/api", 1); !errors.As(err, &unavailable) {
		t.Errorf("malformed JSON gave %T, want *ErrUnavailable", err)
	}
}
