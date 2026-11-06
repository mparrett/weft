package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// validKinds are the node kinds from SPEC.md §4.1.
var validKinds = []string{"bug", "task", "spec", "pr", "issue", "finding", "note", "workstream"}

// refList collects a repeatable --ref.
//
// A node commonly wants more than one identity: a code anchor like
// `file:pkg/vm/vm.go#L827`, which binds it to reality and is what a reader
// wants, and a stable synthetic key like `lgx:path.aot`, which survives the
// file moving. Taking one meant choosing, and the choice was permanent — refs
// are set at add time, and `drop` and re-`add` loses the node's edges.
type refList []string

func (r *refList) String() string { return strings.Join(*r, ",") }

func (r *refList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("a ref cannot be empty")
	}
	// Repeating a ref is tolerated rather than rejected (SPEC.md §9.3), but it
	// must not be written twice: two ref.add records naming the same node and
	// ref are byte-identical, so they hash to one id and the fold discards the
	// second as a crash-retry (EVENTS.md §2.5), leaving a duplicate id in a log
	// that is supposed to be the contract. Deduping here is also what lets the
	// whole transaction share one timestamp — the records differ by `ref`, so
	// nothing else has to make them unique.
	if slices.Contains(*r, v) {
		return nil
	}
	*r = append(*r, v)
	return nil
}

func cmdAdd(e *env, args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	kind := fs.String("kind", "task", "node kind: "+strings.Join(validKinds, ", "))
	nsFlag := fs.String("ns", "", "namespace for the new node (default: $WEFT_NS, then ~/.weft/config, then the git remote)")
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	var refs refList
	fs.Var(&refs, "ref", "bind to an external object, e.g. gh:owner/repo#12; repeatable")
	ifAbsent := fs.Bool("if-absent", false, "reuse the earliest live node with the same exact title and namespace (a writer's assertion that it is the same work; dropped nodes are absent; refs are ignored)")
	evenDropped := fs.Bool("even-dropped", false, "with --if-absent: a dropped node also counts as present, and a reuse binds any --ref the node lacks, so a script that asserts and then drops a claim converges instead of minting duplicates")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}

	title := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if title == "" {
		fmt.Fprintln(e.stderr, `weft add: a title is required, e.g. weft add "N+1 in report gen" --kind bug`)
		return exitUsage
	}
	if !contains(validKinds, *kind) {
		fmt.Fprintf(e.stderr, "weft add: unknown kind %q (want one of: %s)\n", *kind, strings.Join(validKinds, ", "))
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft add: unknown source %q\n", *source)
		return exitUsage
	}
	if *evenDropped && !*ifAbsent {
		fmt.Fprintln(e.stderr, "weft add: --even-dropped only means something with --if-absent")
		return exitUsage
	}
	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	// Named to avoid shadowing the ns package if this file ever imports it.
	namespace := *nsFlag
	if namespace == "" {
		namespace = currentNS(dir)
	}
	// The asserting actor is resolved here and never passed in: --holder names
	// the subject of a claim, "by" is always whoever is running the command
	// (EVENTS.md §4).
	by := actor.Resolve(dir)

	var created *graph.Node
	// Whether anything was appended, so the degraded report below can say which
	// happened. --if-absent reuse returns a node without writing one — except
	// an --even-dropped reuse, which may bind missing refs (refsBound).
	reused := false
	refsBound := false
	// The refs this invocation actually wrote, for the duplicate-holder note
	// printed after the commit. The note is presentation, so it is computed
	// from a lock-free fold *after* the transaction rather than inside it —
	// plain add must not pay a full read under the write lock for a warning
	// (the cost the exit-6 fix removed), and a stale read costs at most a
	// missed or spurious note, never the write.
	var newRefs []string
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		reused = false
		refsBound = false
		newRefs = nil
		var g *graph.Graph
		if *ifAbsent {
			var foldErr error
			if g, foldErr = graph.Fold(fold); foldErr != nil {
				// --if-absent cannot decide "absent" against a log it cannot
				// read.
				return nil, foldErr
			}
		}
		if *ifAbsent {
			// A live match wins even under --even-dropped: when both exist, the
			// live node is the work, and the dropped one is history — reusing the
			// dropped one would hand back a node every query already discounts.
			var match, dropped *graph.Node
			for _, n := range g.Ordered() {
				if n.NS != namespace || n.Title != title {
					continue
				}
				if n.State == event.StateDropped {
					if dropped == nil {
						dropped = n
					}
					continue
				}
				match = n
				break
			}
			if match == nil && *evenDropped {
				match = dropped
			}
			if match != nil {
				// Match the shape returned by a newly created node. Folded nodes
				// carry derived timestamps, while add's immediate JSON response
				// deliberately does not; a retry should be byte-identical.
				created = &graph.Node{
					ID: match.ID, NS: match.NS, Kind: match.Kind, Title: match.Title, State: match.State,
					By: match.By, Source: match.Source,
					Refs: slices.Clone(match.Refs),
				}
				reused = true
				if !*evenDropped {
					return nil, nil
				}
				// --even-dropped exists for re-runnable scripts that address by
				// ref, so a reuse binds any --ref the node lacks. Plain
				// --if-absent keeps its contract (refs ignored, nothing
				// appended); without this, the assert line of an assert-then-
				// drop script can be absorbed by a same-titled node that never
				// carried the ref, and every later line naming the ref fails at
				// the addressing layer, the same symptom as re-creating a
				// dropped node.
				// Already-carried refs append nothing, so re-runs stay
				// byte-identical once the binding lands.
				now := time.Now()
				var events []event.Event
				for _, ref := range refs {
					if slices.Contains(match.Refs, ref) {
						continue
					}
					r := event.New(event.KindRefAdd, by, *source, now)
					r["node"] = match.ID
					r["ref"] = ref
					if err := r.Seal(); err != nil {
						return nil, err
					}
					events = append(events, r)
					created.Refs = append(created.Refs, ref)
					newRefs = append(newRefs, ref)
				}
				refsBound = len(events) > 0
				return events, nil
			}
		}

		// Stamped inside the closure so a retry restamps rather than replaying a
		// moment that has passed. One `now` for the whole batch is right here
		// because these events genuinely happened at once, and refList has
		// already made the ref.add records distinct from one another.
		now := time.Now()
		node, err := event.NodeAdd(namespace, *kind, title, by, *source, now)
		if err != nil {
			return nil, err
		}
		events := []event.Event{node}

		// Refs are committed in the same transaction as the node, so a crash
		// cannot leave a node whose bindings were lost.
		for _, ref := range refs {
			r := event.New(event.KindRefAdd, by, *source, now)
			r["node"] = node.ID()
			r["ref"] = ref
			if err := r.Seal(); err != nil {
				return nil, err
			}
			events = append(events, r)
		}

		created = &graph.Node{
			ID: node.ID(), NS: namespace, Kind: *kind, Title: title, State: event.StateOpen,
			By: by, Source: *source,
			Refs: refs,
		}
		newRefs = refs
		return events, nil
	})
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	// What the transaction did, for the degraded reports below.
	did := "the node was written"
	if reused {
		did = "an existing node was reused, so nothing was written"
		if refsBound {
			did = "an existing node was reused and its missing refs were bound"
		}
	}

	// Past here the write has committed, so nothing below may decide the exit
	// code. Exit 6 means the store could not be read or written, so a caller
	// that gets it concludes nothing landed and retries — and plain `add`
	// asserts a *new* node every time. One wrong 6 costs a silent duplicate that
	// somebody has to `merge` later. Both a failed read *and* a failed write to
	// stdout degrade to a warning: a full disk under `add > out` is the same
	// class, and by then there is nothing left to report the failure to anyway.
	if *asJSON {
		// --json folds only when refs were written, and only to warn: bulk
		// writers are the audience most likely to mint the duplicate the note
		// exists to catch. The read is lock-free and its failure is silent —
		// the write has landed, and a log that cannot be read back cannot
		// warn; the corruption surfaces on the next read command, matching
		// the deliberate --json silence of the exit-6 fix.
		if len(newRefs) > 0 {
			if g, foldErr := graph.Fold(s.Fold); foldErr == nil {
				printRefNotes(e, g, newRefs, created.ID)
			}
		}
		if err := render.NodesJSON(e.stdout, []*graph.Node{created}); err != nil {
			return degraded(e, did, "printing it failed", err)
		}
		return exitOK
	}

	// This fold contributes the id display width and the duplicate-ref note.
	// If the log cannot be read there is no id set to be distinct from, so
	// print the id whole: a MinWidth prefix guessed against an unreadable log
	// is exactly what the caller pastes into `weft show` once the log is
	// repaired.
	width := render.FullWidth
	if g, foldErr := graph.Fold(s.Fold); foldErr != nil {
		_ = degraded(e, did, "the log could not be read back", foldErr)
	} else {
		width = render.Width(g.IDs())
		if len(newRefs) > 0 {
			printRefNotes(e, g, newRefs, created.ID)
		}
	}
	if err := render.Added(e.stdout, created, width); err != nil {
		return degraded(e, did, "printing it failed", err)
	}
	return exitOK
}

// degraded reports a failure that happened after the transaction committed. It
// always returns exitOK: the caller asked for a write, the write either
// happened or was deliberately skipped, and saying otherwise makes a retry
// assert a duplicate node (BUILD.md §5). what names what failed, since by this
// point the interesting fact is that it did not cost the write.
func degraded(e *env, did, what string, err error) int {
	fmt.Fprintf(e.stderr, "warning: %s, but %s: %v\n", did, what, err)
	return exitOK
}

// refNote records that a live node already carries a ref the new node is being
// given, or the gh:/pr: sibling naming the same object.
type refNote struct {
	newRef      string
	existingRef string
	id          string
	title       string
}

// refNotes reports every live node already carrying one of refs or an
// equivalent, excluding the node the refs were just written to — it is not a
// duplicate of itself. This is presentation, not resolution: equivalence
// never enters Resolve, because the moment gh:x#1 resolves a node carrying
// only pr:x#1, refs have become identity — which SPEC.md §4.1 refuses.
//
// One note per (ref, holder), with the exact match winning over the sibling:
// a post-merge node carries both schemes, and naming it twice for one new ref
// is the noise that teaches writers to ignore notes. The dropped-holder skip
// is the same liveness rule resolution applies (graph.PreferLive), spelled
// here because this wants live-only, not live-preferred.
func refNotes(g *graph.Graph, refs []string, exclude string) []refNote {
	byRef := g.ByRef()
	seen := make(map[string]bool)
	var out []refNote
	for _, ref := range refs {
		for _, target := range []string{ref, siblingSchemeRef(ref)} {
			if target == "" {
				continue
			}
			for _, n := range byRef[target] {
				if n.ID == exclude || n.State == event.StateDropped || seen[ref+"\x00"+n.ID] {
					continue
				}
				seen[ref+"\x00"+n.ID] = true
				out = append(out, refNote{newRef: ref, existingRef: target, id: n.ID, title: n.Title})
			}
		}
	}
	return out
}

// printRefNotes surfaces the holders of freshly written refs, after the
// commit, to stderr: named, still written, exit 0. Refs are deliberately not
// identity (SPEC.md §4.1), so two nodes carrying the same ref — or its
// gh:/pr: twin — is a condition to surface at the moment it is created rather
// than several edges later, never an error, and deciding they are the same
// work stays `weft merge`. The hint is deduplicated per holder no matter how
// many refs matched it.
func printRefNotes(e *env, g *graph.Graph, newRefs []string, selfID string) {
	notes := refNotes(g, newRefs, selfID)
	if len(notes) == 0 {
		return
	}
	// The fold includes the node just written, so both ids in the hint render
	// at one width and the pasted merge command resolves.
	width := render.Width(g.IDs())
	for _, n := range notes {
		if n.existingRef == n.newRef {
			fmt.Fprintf(e.stderr, "note: %s is already carried by %s  %s\n",
				n.newRef, render.Short(n.id, width), n.title)
		} else {
			fmt.Fprintf(e.stderr, "note: %s names the same thing as %s on %s  %s\n",
				n.newRef, n.existingRef, render.Short(n.id, width), n.title)
		}
	}
	hinted := make(map[string]bool)
	for _, n := range notes {
		if hinted[n.id] {
			continue
		}
		hinted[n.id] = true
		fmt.Fprintf(e.stderr, "      if they are the same work: weft merge %s %s\n",
			render.Short(n.id, width), render.Short(selfID, width))
	}
}

// siblingSchemeRef maps gh:owner/repo#N to pr:owner/repo#N and back, or ""
// when ref is neither. Both schemes are first-class names for a pull request —
// they are the two `import` binds — and nothing else relates them, so one PR
// can silently occupy two nodes. The narrow case is deliberate: no other
// scheme pair claims to name the same object.
//
// The match is byte-exact past the scheme, so gh:Acme/API#1 and gh:acme/api#1
// get no note even though GitHub treats them as one repo. Deliberate: refs
// are opaque strings (SPEC.md §4.1), the exact-ref arm does not case-fold
// either, and a second equivalence rule folded in here would be the start of
// refs becoming identity. import always mints the case the API returns, so
// the mixed-case miss needs a hand-typed ref to arise.
func siblingSchemeRef(ref string) string {
	for _, p := range [2][2]string{{"gh:", "pr:"}, {"pr:", "gh:"}} {
		if rest, ok := strings.CutPrefix(ref, p[0]); ok && strings.Contains(rest, "#") {
			return p[1] + rest
		}
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// storeExitCode maps a store failure to the stable exit contract (BUILD.md §5).
func storeExitCode(err error) int {
	var corrupt *store.ErrCorrupt
	var unsupported *event.ErrUnsupportedVersion
	if errors.As(err, &corrupt) || errors.As(err, &unsupported) {
		return exitStore
	}
	return exitRejected
}

func fail(e *env, code int, asJSON bool, err error) int {
	if asJSON {
		_ = render.Error(e.stdout, code, err.Error(), nil)
		return code
	}
	fmt.Fprintf(e.stderr, "weft: %v\n", err)
	return code
}
