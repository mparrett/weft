package main

import (
	"flag"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/query"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// parseSince reads a recency window. It takes the flag it is parsing for so the
// diagnostic names what the caller actually typed — `ls --since` and
// `import --merged-since` share the grammar, and an error naming the other flag
// sends someone to the wrong line.
//
// time.ParseDuration tops out at hours, but the question these flags answer is
// asked in days and weeks — "what moved this week" is the motivating case, and
// making someone write 168h to ask it is a tax on the one spelling everybody
// reaches for first. Days and weeks are fixed multiples here, not calendar
// units: no timezone is involved, and 7d means 168 hours rather than "the same
// clock time seven days ago".
func parseSince(name, s string) (time.Duration, error) {
	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	}
	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
		// NaN fails every comparison, so `n < 0` does not reject it and
		// "nanw" would parse to a zero window. Inf is rejected for the same
		// reason a caller cannot have meant it.
		span := n * float64(u.unit)
		// ParseFloat can return a finite number whose scaled duration is beyond
		// time.Duration. Converting that overflows to a negative window, turning
		// an old-history query into a future cutoff while the command exits 0.
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 ||
			span > float64(time.Duration(1<<63-1)) {
			return 0, fmt.Errorf("invalid %s %q: want a duration like 72h, 7d, 2w", name, s)
		}
		return time.Duration(span), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid %s %q: want a duration like 72h, 7d, 2w", name, s)
	}
	return d, nil
}

func cmdLs(e *env, args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	kind := fs.String("kind", "", "filter by node kind")
	nsFlag := fs.String("ns", "", "namespace to list (default: $WEFT_NS, then ~/.weft/config, then the git remote)")
	allNS := fs.Bool("all-ns", false, "list across every namespace")
	byTitle := fs.Bool("sort", false, "sort by title instead of assertion order")
	recent := fs.Bool("recent", false, "sort by most recently touched")
	since := fs.String("since", "", "only nodes touched within a duration, e.g. 72h, 7d, 2w")
	mine := fs.Bool("mine", false, "only nodes you hold a live claim on")
	ready := fs.Bool("ready", false, "only nodes with no known blocker")
	blocked := fs.Bool("blocked", false, "only nodes with an unresolved dependency")
	stale := fs.Bool("stale", false, "only nodes whose every claim has expired")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "weft ls: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *ready && *blocked {
		fmt.Fprintln(e.stderr, "weft ls: --ready and --blocked are mutually exclusive")
		return exitUsage
	}
	if *byTitle && *recent {
		fmt.Fprintln(e.stderr, "weft ls: --sort and --recent are mutually exclusive")
		return exitUsage
	}
	var window time.Duration
	if *since != "" {
		d, err := parseSince("--since", *since)
		if err != nil {
			fmt.Fprintf(e.stderr, "weft ls: %v\n", err)
			return exitUsage
		}
		window = d
	}
	if *kind != "" && !contains(validKinds, *kind) {
		fmt.Fprintf(e.stderr, "weft ls: unknown kind %q (want one of: %s)\n", *kind, strings.Join(validKinds, ", "))
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	scope := *nsFlag
	if scope == "" {
		scope = currentNS(dir)
	}
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	// Namespace selects which nodes are *listed*; it never limits traversal.
	// Filtering before traversal is what would let ls --ready hide an external
	// blocker (SPEC.md §5), so the distinction is established here, before any
	// query depends on it.
	now := time.Now()
	me := actor.Resolve(dir)

	// Readiness and blocking are computed over the *whole* graph before any
	// namespace filter applies. Filtering first would hide a cross-namespace
	// blocker and report a node ready when it is not (SPEC.md §5) — and the
	// motivating story crosses repos, so that is the ordinary case.
	var candidates []*graph.Node
	switch {
	case *ready:
		candidates = query.Ready(g, me, now)
	case *blocked:
		candidates = query.Blocked(g, now)
	default:
		candidates = g.Ordered()
	}

	// The namespace filter is applied last, and separately, so that what it
	// excluded can be counted. Everything else answers "does this node match?";
	// only the namespace can be widened by a flag, so only it is worth a hint.
	var nodes []*graph.Node
	elsewhere := 0
	for _, n := range candidates {
		if *kind != "" && n.Kind != *kind {
			continue
		}
		if *mine && g.ClaimBy(n.ID, me, now) == nil {
			continue
		}
		// Stale means abandoned: someone took the work and never came back.
		// A node nobody ever claimed is not stale, it is simply unclaimed.
		if *stale {
			claims := g.ClaimsFor(n.ID)
			if len(claims) == 0 || len(g.LiveClaims(n.ID, now)) > 0 {
				continue
			}
		}
		// A node with no Touched is excluded rather than kept: the zero time
		// means nothing in the log dated this node, which is not evidence that
		// it moved. Only a hand-built graph gets here — every folded node
		// carries the timestamp of its own node.add.
		//
		// Guarded on the flag rather than on a positive window, or `--since 0d`
		// would silently list everything: the filter would switch itself off at
		// exactly the value that asks for the narrowest possible window. A
		// future-dated node is excluded too — now.Sub is negative there, so it
		// would otherwise fall inside every window including --since 1h.
		if *since != "" {
			age := now.Sub(n.Touched)
			if n.Touched.IsZero() || age < 0 || age > window {
				continue
			}
		}
		if !*allNS && n.NS != scope {
			elsewhere++
			continue
		}
		nodes = append(nodes, n)
	}
	if *recent {
		render.SortRecent(nodes)
	} else {
		render.SortNodes(nodes, *byTitle)
	}

	if *asJSON {
		if err := render.NodesJSON(e.stdout, nodes); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	// Width spans every id in the store, not just the filtered set, so a
	// pasted prefix stays unambiguous outside this listing.
	if err := render.NodeList(e.stdout, nodes, render.Width(g.IDs()), *allNS); err != nil {
		return fail(e, exitStore, false, err)
	}
	// An empty listing has two causes that call for opposite advice, and saying
	// the wrong one steers the reader away from the answer (it once did, when a
	// namespace had nodes but none ready). The hint stays on stderr so stdout is
	// byte-identical for anything reading the listing (SPEC §9.5), and is
	// suppressed under --all-ns, where "namespace" is not the frame.
	if len(nodes) == 0 && !*allNS {
		// Does the namespace hold anything at all, filter aside? Computed over
		// the whole graph, not the filtered set, precisely so the two states can
		// be told apart.
		scopeHasNodes, scopeHasWork := false, false
		for _, n := range g.Ordered() {
			if n.NS == scope {
				scopeHasNodes = true
				if n.Kind != query.Container {
					scopeHasWork = true
					break
				}
			}
		}

		selecting := *ready || *blocked || *mine || *stale || *kind != "" || *since != ""

		switch {
		case scopeHasNodes && !scopeHasWork && (*ready || *blocked):
			// The frontier skips containers, so a namespace of nothing but
			// workstreams has nothing blocked either; pointing at --blocked or
			// `why` would send the reader after a blocker that does not exist.
			fmt.Fprintf(e.stderr, "no work nodes in namespace %q — it holds only workstreams, which the frontier skips; try `weft ls --kind workstream`\n", scope)
		case scopeHasNodes && selecting:
			// The namespace holds work; this filter excludes all of it. Saying
			// "no nodes" here is a false fact, and --all-ns / `weft ns` are
			// remedies for a namespace *resolution* mistake, which this is not.
			// Name the predicate and point back into the graph. The predicate is
			// specific only for the two headline filters standing alone, where
			// it cannot over-claim.
			noun := "matching nodes"
			narrowed := *mine || *stale || *kind != "" || *since != ""
			switch {
			case *ready && !narrowed:
				noun = "ready nodes"
			case *blocked && !narrowed:
				noun = "blocked nodes"
			}
			// Only --ready earns the `why` suggestion. Under --kind or --stale
			// nothing is waiting on anything, so offering to explain a blocker
			// sends the reader after a question they did not ask; dropping the
			// filter is the whole remedy there.
			next := "`weft ls` without the filter"
			if *ready {
				next = "`weft ls --blocked` or `weft why <id>` for what is holding it"
			}
			fmt.Fprintf(e.stderr, "no %s in namespace %q — it holds other work; try %s\n", noun, scope, next)
		case elsewhere > 0:
			// The namespace holds nothing here but matches exist elsewhere:
			// likely the wrong namespace. The count is of what the namespace
			// filter excluded, so --all-ns and `weft ns` are the right calls.
			fmt.Fprintf(e.stderr, "no nodes in namespace %q (%d elsewhere — try --all-ns, or `weft ns` to see why this namespace)\n",
				scope, elsewhere)
		}
		// Empty everywhere and unfiltered gets nothing: there is simply nothing
		// to find.
	}
	return exitOK
}
