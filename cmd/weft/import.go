package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/importer"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// ghRunner is swapped by tests. The shell-out is the one part that cannot be
// exercised hermetically, so it is the one part with a seam.
var ghRunner importer.Runner = importer.Exec

// cmdImport binds nodes to GitHub issues and pull requests.
//
// Import is idempotent in the strong sense: re-running it against unchanged
// upstream appends *nothing*, leaving the log byte-identical. That is stronger
// than "creates no duplicate node" and it is what makes running this on a
// schedule safe. It holds because the whole run is one diff against the folded
// graph, and store.Mutate does not append an empty event list.
//
// It is also one gh fetch, one fold, and one append for the whole batch. Per
// item, each of those would be a separate exclusive-lock transaction over the
// entire log, which is the bulk-write cost BUILD.md §1.4 names by name.
func cmdImport(e *env, args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	kind := fs.String("kind", "issue", "node kind for imported issues: "+strings.Join(validKinds, ", "))
	nsFlag := fs.String("ns", "", "namespace (default: the imported repo)")
	limit := fs.Int("limit", 200, "maximum items to fetch when importing a whole repo")
	mergedSince := fs.String("merged-since", "", "also import pull requests merged within a duration, e.g. 7d, 2w")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 || fs.Arg(0) != "gh" {
		fmt.Fprintln(e.stderr, "weft import: want `gh <owner/repo>` or `gh <owner/repo#123>`")
		return exitUsage
	}
	if !contains(validKinds, *kind) {
		fmt.Fprintf(e.stderr, "weft import: unknown kind %q (want one of: %s)\n", *kind, strings.Join(validKinds, ", "))
		return exitUsage
	}
	if *limit < 1 {
		fmt.Fprintln(e.stderr, "weft import: --limit must be at least 1")
		return exitUsage
	}
	repo, number, err := importer.ParseTarget(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(e.stderr, "weft import: %v\n", err)
		return exitUsage
	}

	q := importer.Query{Limit: *limit}
	if *mergedSince != "" {
		// One item is fetched by number, so there is no window to widen.
		// Accepting the flag and ignoring it would report a complete import of a
		// window that was never asked for.
		if number > 0 {
			fmt.Fprintln(e.stderr, "weft import: --merged-since takes a whole repo, not a single item")
			return exitUsage
		}
		d, err := parseSince("--merged-since", *mergedSince)
		if err != nil {
			fmt.Fprintf(e.stderr, "weft import: %v\n", err)
			return exitUsage
		}
		q.MergedSince = time.Now().Add(-d)
	}

	items, truncated, err := fetch(repo, number, q)
	if err != nil {
		var unavailable *importer.ErrUnavailable
		if errors.As(err, &unavailable) {
			return fail(e, exitExternal, *asJSON, err)
		}
		return fail(e, exitStore, *asJSON, err)
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)
	// The namespace comes from the repo being imported, not the working
	// directory. Importing acme/api while sitting in some other checkout should
	// not file the nodes under that checkout.
	namespace := *nsFlag
	if namespace == "" {
		namespace = repo
	}

	var result importResult
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		result = importResult{truncated: truncated}
		return plan(g, items, repo, namespace, *kind, by, time.Now(), &result)
	})
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}
	if *asJSON {
		if err := render.NodesJSON(e.stdout, result.nodes(g)); err != nil {
			return fail(e, exitStore, false, err)
		}
		// Truncation and collisions both have to reach a --json caller, or an
		// agent reads a clean exit and a short node list as a complete import.
		// Both go to stderr, so the payload on stdout stays parseable.
		warnTruncation(e, result.truncated, *limit)
		warnTwins(e, g, result.twins)
		if len(result.collisions) > 0 {
			for _, c := range result.collisions {
				fmt.Fprintf(e.stderr, "skipped %s: already on %d nodes — run `weft merge` to fold them\n",
					c.ref, len(c.nodes))
			}
			return exitAmbiguous
		}
		return exitOK
	}
	return report(e, g, result, *limit)
}

func fetch(repo string, number int, q importer.Query) ([]importer.Item, importer.Truncated, error) {
	if number > 0 {
		item, err := importer.One(ghRunner, repo, number)
		if err != nil {
			return nil, importer.Truncated{}, err
		}
		return []importer.Item{item}, importer.Truncated{}, nil
	}
	return importer.List(ghRunner, repo, q)
}

// importResult records what the run did, so the summary reports facts rather
// than a count of things attempted.
type importResult struct {
	added      []string // node ids
	observed   []string // refs whose upstream state changed
	collisions []collision
	twins      []twin
	seen       int
	truncated  importer.Truncated
}

// collision is a ref already carried by more than one node.
type collision struct {
	ref   string
	nodes []*graph.Node
}

// twin is a node import just created whose ref's gh:/pr: sibling is already
// carried by a live node — one PR on two nodes under two schemes, the doubling
// `add` warns about, minted by the bulk path that made it a 39-PR problem. Import
// cannot see the sibling in its collision check without making the schemes
// identity, so the duplicate is created and *named*, the same warn-only
// contract add's note keeps: exit 0, and deciding they are the same work
// stays `weft merge`.
type twin struct {
	ref     string
	sibling string
	holder  *graph.Node
	nodeID  string
}

func (r importResult) nodes(g *graph.Graph) []*graph.Node {
	out := make([]*graph.Node, 0, len(r.added))
	for _, id := range r.added {
		if n, ok := g.Nodes[id]; ok {
			out = append(out, n)
		}
	}
	return out
}

// plan diffs what GitHub reported against the folded graph and returns only the
// events needed to close the gap. An unchanged item contributes nothing.
//
// This runs inside the transaction, against the same fold the append lands on:
// two concurrent imports of the same issue would otherwise each see no node and
// each create one (EVENTS.md §6).
func plan(g *graph.Graph, items []importer.Item, repo, namespace, kind, by string,
	now time.Time, result *importResult) ([]event.Event, error) {

	byRef := g.ByRef()
	// The open and merged searches are separate snapshots. A PR that merges
	// between them can appear in both, and first-response-wins would be a
	// permanent state error: if the open copy creates the node, later imports
	// only update the observation because upstream is never authority over
	// weft's state. Coalesce before planning and prefer a resolved observation.
	// Identical repeats still collapse to the first one as before.
	result.seen += len(items)
	items = preferResolved(items, repo)
	var events []event.Event

	for i, item := range items {
		ref := item.Ref(repo)

		// Each item gets its own instant. A node id hashes the whole record and
		// carries no issue number, so one timestamp for the batch makes two
		// items with the same title — "flaky test", "update deps" — hash
		// identically, and the fold discards the second as a crash-retry
		// (EVENTS.md §2.5). That silently collapses two unrelated issues into
		// one node, and deciding two things are the same is `weft merge`.
		at := now.Add(time.Duration(i))

		// A ref on more than one node is not corruption, but import cannot pick
		// a winner — that judgment is `weft merge` (EVENTS.md §2.2). The item is
		// skipped and the rest of the batch proceeds: one collided ref must not
		// block a scheduled import of everything else (SPEC.md §9.3).
		// The count uses the same notion of present that resolution does: a
		// live holder wins over dropped ones, or a scheduled import
		// would skip an item forever — and exit 4 on every run — because a
		// dropped predecessor of a re-created node still carries the ref.
		matches := graph.PreferLive(byRef[ref])
		if len(matches) > 1 {
			result.collisions = append(result.collisions, collision{ref: ref, nodes: matches})
			continue
		}

		var nodeID string
		created := len(matches) == 0
		if created {
			itemKind := kind
			if item.IsPR {
				itemKind = "pr"
			}
			node, err := event.NodeAdd(namespace, itemKind, item.Title, by, event.SourceObserved, at)
			if err != nil {
				return nil, err
			}
			// The binding is committed in the same transaction as the node, so
			// a crash cannot leave a node whose ref was lost.
			bind, err := event.RefAdd(node.ID(), ref, by, event.SourceObserved, at)
			if err != nil {
				return nil, err
			}
			events = append(events, node, bind)

			// State is set once, here, from what was observed now, and is
			// weft's thereafter. Resolved states are named explicitly rather
			// than tested as "not open": an absent state field, or a value a
			// newer gh reports, would otherwise land the node done — and since
			// nothing ever revisits it, that drops real work out of `ls --ready`
			// permanently. Unknown fails towards open, which is recoverable.
			if item.State == "closed" || item.State == "merged" {
				st, err := event.NodeState(node.ID(), event.StateDone, by, event.SourceObserved, at)
				if err != nil {
					return nil, err
				}
				events = append(events, st)
			}
			nodeID = node.ID()
			result.added = append(result.added, nodeID)
			// Live holders only, like add's note: a dropped sibling is history,
			// and warning about it would teach readers to ignore notes.
			if sib := siblingSchemeRef(ref); sib != "" {
				for _, n := range byRef[sib] {
					if n.State == event.StateDropped {
						continue
					}
					result.twins = append(result.twins, twin{ref: ref, sibling: sib, holder: n, nodeID: nodeID})
				}
			}
		} else {
			nodeID = matches[0].ID
		}

		// The observation is a dated cache of what upstream says, never
		// authority over the node's own fields. Written only when it says
		// something new, which is what keeps a re-import of unchanged upstream
		// from appending anything at all.
		payload := event.ObservePayload{
			State: item.State, Title: item.Title,
			Created: item.Created, Modified: item.Updated,
		}
		if g.Observed(nodeID, ref).Changed(payload) {
			obs, err := event.RefObserve(nodeID, ref, payload, by, event.SourceObserved, at)
			if err != nil {
				return nil, err
			}
			events = append(events, obs)
			// A first observation is not a *change*. Reporting it as one on the
			// initial import makes every item look like it moved, which is the
			// fastest way to teach someone to stop reading the notes.
			if !created {
				result.observed = append(result.observed, ref)
			}
		}
	}
	return events, nil
}

func preferResolved(items []importer.Item, repo string) []importer.Item {
	out := make([]importer.Item, 0, len(items))
	positions := make(map[string]int, len(items))
	for _, item := range items {
		ref := item.Ref(repo)
		if i, ok := positions[ref]; ok {
			if upstreamResolved(item.State) && !upstreamResolved(out[i].State) {
				out[i] = item
			}
			continue
		}
		positions[ref] = len(out)
		out = append(out, item)
	}
	return out
}

func upstreamResolved(state string) bool { return state == "closed" || state == "merged" }

// warnTruncation says which fetches were cut short. Silence here would make a
// partial import indistinguishable from a complete one, while `ls --ready`
// still reads as authoritative.
func warnTruncation(e *env, t importer.Truncated, limit int) {
	if t.Open {
		// gh lists newest first, so the window is not even stable across runs:
		// new issues push the oldest permanently out of every scheduled import.
		fmt.Fprintf(e.stderr,
			"warning: stopped at --limit %d; gh lists newest first, so older items were not seen\n", limit)
	}
	if t.Merged {
		// Deliberately does not say *which* ones were missed. The merged window
		// is a search query, and GitHub's search does not order by merge time —
		// so unlike the open lists, the ones that fell off the end are not the
		// oldest, and claiming they were would send someone looking in the
		// wrong place.
		fmt.Fprintf(e.stderr,
			"warning: more than --limit %d pull requests were merged in that window, "+
				"so some were not seen; raise --limit or shorten --merged-since\n", limit)
	}
}

// warnTwins names each created node whose ref's gh:/pr: sibling a live node
// already carries, in the same shape as add's note. Stderr, exit unchanged.
func warnTwins(e *env, g *graph.Graph, twins []twin) {
	if len(twins) == 0 {
		return
	}
	width := render.Width(g.IDs())
	for _, tw := range twins {
		fmt.Fprintf(e.stderr, "note: %s names the same thing as %s on %s  %s\n",
			tw.ref, tw.sibling, render.Short(tw.holder.ID, width), tw.holder.Title)
		fmt.Fprintf(e.stderr, "      if they are the same work: weft merge %s %s\n",
			render.Short(tw.holder.ID, width), render.Short(tw.nodeID, width))
	}
}

func report(e *env, g *graph.Graph, r importResult, limit int) int {
	width := render.Width(g.IDs())
	for _, id := range r.added {
		if n, ok := g.Nodes[id]; ok {
			fmt.Fprintf(e.stdout, "%s  %s\n", render.Short(n.ID, width), n.Title)
		}
	}
	// Say what changed, including when nothing did. "Up to date" is the answer
	// a scheduled import gives most of the time, and silence reads as failure.
	changed := append([]string(nil), r.observed...)
	sort.Strings(changed)
	switch {
	case len(r.added) == 0 && len(changed) == 0 && len(r.collisions) == 0:
		fmt.Fprintf(e.stdout, "%s up to date, nothing written\n", render.Plural(r.seen, "item"))
	default:
		fmt.Fprintf(e.stdout, "%s: %d added, %d with upstream changes\n",
			render.Plural(r.seen, "item"), len(r.added), len(changed))
	}
	for _, ref := range changed {
		fmt.Fprintf(e.stderr, "note: %s changed upstream\n", ref)
	}
	warnTruncation(e, r.truncated, limit)
	warnTwins(e, g, r.twins)

	// Each collision names the remedy. Reporting that two nodes claim one ref
	// without saying what to do about it leaves the caller no way forward.
	sort.Slice(r.collisions, func(i, j int) bool { return r.collisions[i].ref < r.collisions[j].ref })
	for _, c := range r.collisions {
		fmt.Fprintf(e.stderr, "skipped %s: already on %d nodes — run `weft merge` to fold them\n",
			c.ref, len(c.nodes))
		_ = render.NodeList(e.stderr, c.nodes, width, false)
	}
	if len(r.collisions) > 0 {
		return exitAmbiguous
	}
	return exitOK
}
