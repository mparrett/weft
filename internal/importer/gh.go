// Package importer binds weft nodes to external objects by shelling out.
//
// `gh` is shelled out to, never wrapped (BUILD.md §1.1). It already solves
// auth, enterprise hosts, and rate limiting, and every one of those solved
// again in-process is a dependency paid for on every invocation.
//
// Import is one-way and explicit (SPEC.md §10). Nothing here writes to GitHub.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Item is one issue or pull request as GitHub reported it at a moment.
//
// This is deliberately narrow. Everything weft stores about an external object
// has to be renderable and comparable, and a struct that grows to mirror the
// API is one that makes every observation differ from the last.
type Item struct {
	Number int
	Title  string
	State  string // open | closed | merged
	IsPR   bool
	// The artifact's own clock (EVENTS.md §3.8), verbatim from gh, which
	// reports RFC3339 UTC. Kept as strings end to end: the observation diff is
	// byte-exact, and a parse/format round trip that normalized precision
	// would report a change upstream never made.
	Created string
	Updated string
}

// Ref renders the item as a weft ref (SPEC.md §3).
func (i Item) Ref(repo string) string {
	scheme := "gh"
	if i.IsPR {
		scheme = "pr"
	}
	return fmt.Sprintf("%s:%s#%d", scheme, repo, i.Number)
}

// ErrUnavailable reports that gh could not be used at all — missing, not
// authenticated, or the API refused us. Distinct from "the issue does not
// exist", because one is worth fixing your environment over and the other is
// an answer.
type ErrUnavailable struct {
	Err    error
	Detail string
}

func (e *ErrUnavailable) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("gh unavailable: %s", strings.TrimSpace(e.Detail))
	}
	return fmt.Sprintf("gh unavailable: %v", e.Err)
}

func (e *ErrUnavailable) Unwrap() error { return e.Err }

// Runner executes gh and returns its stdout. Injected so tests never reach the
// network: the shell-out is the part that cannot be exercised hermetically, so
// it is the part with a seam.
type Runner func(args ...string) ([]byte, error)

// Exec is the production Runner.
func Exec(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || errors.Is(err, exec.ErrNotFound) {
			return nil, &ErrUnavailable{Err: err, Detail: stderr.String()}
		}
		return nil, &ErrUnavailable{Err: err}
	}
	return out, nil
}

// ghItem mirrors only the gh --json fields asked for.
type ghItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Query bounds a whole-repo fetch. Fetching one item by number takes no query:
// the fields here exist to bound a list, and a list of one is already bounded.
type Query struct {
	Limit int

	// MergedSince widens the fetch beyond open items to pull requests merged at
	// or after this instant. The zero value fetches open items only.
	//
	// Merged pull requests are what make the frontier mean anything: they import
	// as done, and until something is done `ls --ready` returns everything and a
	// real blocker is invisible in the noise. A repo with no open PRs is exactly
	// the one where an open-only import produces a graph that cannot explain
	// itself.
	MergedSince time.Time
}

// Truncated names the sub-fetches that hit Limit. The two are kept apart
// because their remedies differ: a truncated open list can only be answered
// with a bigger --limit, while a truncated merged window can also be narrowed.
type Truncated struct {
	Open   bool
	Merged bool
}

// fetchSpec is one `gh <sub> list` invocation.
type fetchSpec struct {
	sub       string
	isPR      bool
	filter    []string
	truncated *bool
}

// List fetches a repo's open issues and pull requests, plus the pull requests
// merged inside q.MergedSince when one is set.
//
// Two gh calls, not one per item — three with a merged window: process spawn
// dominates everything else here, so a hundred `gh issue view` invocations
// would cost more than the whole fold and append put together (BUILD.md §1.4).
func List(run Runner, repo string, q Query) ([]Item, Truncated, error) {
	var out []Item
	var cut Truncated
	if q.Limit < 1 {
		return nil, cut, fmt.Errorf("limit must be at least 1")
	}
	// Ask for one sentinel beyond the public limit. Receiving exactly Limit
	// items proves only that the window is full, not that anything was omitted;
	// the extra item is what makes the truncation warning a fact.
	fetchLimit := q.Limit
	if q.Limit < int(^uint(0)>>1) {
		fetchLimit++
	}
	fetches := []fetchSpec{
		{sub: "issue", filter: []string{"--state", "open"}, truncated: &cut.Open},
		{sub: "pr", isPR: true, filter: []string{"--state", "open"}, truncated: &cut.Open},
	}
	if !q.MergedSince.IsZero() {
		// The window is a search qualifier so the server does the filtering.
		// Fetching merged PRs unfiltered and dropping the old ones locally would
		// look equivalent and is not: `gh pr list` orders by *creation*, so a
		// long-lived branch merged yesterday sorts behind newer PRs and falls
		// off the end of --limit. That failure is silent and systematic, which
		// is worse than the search index being a moment behind.
		fetches = append(fetches, fetchSpec{
			sub: "pr", isPR: true, truncated: &cut.Merged,
			filter: []string{"--state", "merged",
				"--search", "merged:>=" + q.MergedSince.UTC().Format(time.RFC3339)},
		})
	}
	for _, spec := range fetches {
		args := append([]string{spec.sub, "list", "--repo", repo}, spec.filter...)
		args = append(args, "--limit", strconv.Itoa(fetchLimit), "--json", "number,title,state,createdAt,updatedAt")
		raw, err := run(args...)
		if err != nil {
			return nil, cut, err
		}
		items, err := decode(raw, spec.isPR)
		if err != nil {
			// Unreadable output from a gh that *ran* is still a gh problem, not
			// a weft one. Classified as unavailable so it exits 7: reporting a
			// JSON shape change as a store error sends you to inspect a log
			// that is perfectly fine (BUILD.md §5).
			return nil, cut, &ErrUnavailable{Err: err, Detail: "gh " + spec.sub + " list returned unreadable JSON"}
		}
		if len(items) > q.Limit {
			*spec.truncated = true
			items = items[:q.Limit]
		}
		out = append(out, items...)
	}
	return out, cut, nil
}

// apiItem is the REST issues payload. `pull_request` is present only on pull
// requests, which is what makes the distinction definitive.
type apiItem struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	PullRequest *struct {
		MergedAt *string `json:"merged_at"`
	} `json:"pull_request"`
}

// One fetches a single issue or pull request by number.
//
// Deliberately one `gh api` call rather than `issue view` plus `pr view`. The
// two-call form had to treat a failing `pr view` as "not a pull request", which
// is indistinguishable from a rate limit, an expired token, or a network blip —
// and guessing wrong mints the ref under the `gh:` scheme instead of `pr:`.
// Since those are different strings, the collision check cannot see it, and the
// next run creates a second node for the same PR. The REST payload carries
// `pull_request` or it does not, so nothing has to be inferred from a failure.
func One(run Runner, repo string, number int) (Item, error) {
	raw, err := run("api", fmt.Sprintf("repos/%s/issues/%d", repo, number))
	if err != nil {
		return Item{}, err
	}
	var it apiItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return Item{}, &ErrUnavailable{Err: err, Detail: "gh returned unreadable JSON for " + repo}
	}
	item := Item{
		Number:  it.Number,
		Title:   it.Title,
		State:   normalizeState(it.State),
		IsPR:    it.PullRequest != nil,
		Created: it.CreatedAt,
		Updated: it.UpdatedAt,
	}
	// The issues endpoint reports a merged PR as "closed". Both resolve to done
	// downstream, but the log should say which one actually happened.
	if it.PullRequest != nil && it.PullRequest.MergedAt != nil && *it.PullRequest.MergedAt != "" {
		item.State = "merged"
	}
	return item, nil
}

func decode(raw []byte, isPR bool) ([]Item, error) {
	var items []ghItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, Item{
			Number:  it.Number,
			Title:   it.Title,
			State:   normalizeState(it.State),
			IsPR:    isPR,
			Created: it.CreatedAt,
			Updated: it.UpdatedAt,
		})
	}
	return out, nil
}

// normalizeState lowercases gh's state, which it reports as OPEN/CLOSED/MERGED.
// Stored lowercase so an observation compares equal across gh versions that
// disagree about casing — otherwise every import writes a spurious change.
func normalizeState(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ParseTarget splits an import target into a repo and an optional issue number.
//
//	acme/api        -> every open issue and PR
//	acme/api#508    -> just that one
func ParseTarget(target string) (repo string, number int, err error) {
	repo, num, hasNum := strings.Cut(target, "#")
	repo = strings.TrimSpace(repo)
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", 0, fmt.Errorf("want owner/repo or owner/repo#123, got %q", target)
	}
	if !hasNum {
		return repo, 0, nil
	}
	number, err = strconv.Atoi(strings.TrimSpace(num))
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("%q is not an issue number", num)
	}
	return repo, number, nil
}
