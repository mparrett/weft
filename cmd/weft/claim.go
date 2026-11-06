package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

func cmdClaim(e *env, args []string) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	holder := fs.String("holder", "", "who is doing the work (default: you)")
	note := fs.String("note", "", "free text")
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	ttl := fs.Duration("ttl", event.DefaultTTL, "how long the claim stays live")
	noExpiry := fs.Bool("no-expiry", false, "claim never ages")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "weft claim: want exactly one node id, prefix, or ref")
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft claim: unknown source %q\n", *source)
		return exitUsage
	}
	if *ttl <= 0 && !*noExpiry {
		fmt.Fprintln(e.stderr, "weft claim: --ttl must be positive (or pass --no-expiry)")
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)
	// "me" is CLI sugar; the log only ever carries a concrete actor
	// (EVENTS.md §4).
	if *holder == "" || *holder == "me" {
		*holder = by
	}

	now := time.Now()
	var expires time.Time
	if !*noExpiry {
		expires = now.Add(*ttl)
	}

	var node *graph.Node
	var existing []*graph.Claim
	var resolveErr error
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if node, err = g.Resolve(fs.Arg(0)); err != nil {
			resolveErr = err
			return nil, err
		}
		// Recorded, never refused. Claims are advisory, and a collision is a
		// thing the humans involved need to see, not something the tool should
		// arbitrate (SPEC.md §4.3).
		existing = g.LiveClaims(node.ID, now)

		claim, err := event.ClaimAdd(node.ID, *holder, *note, by, *source, now, expires)
		if err != nil {
			return nil, err
		}
		return []event.Event{claim}, nil
	})
	if err != nil {
		if resolveErr != nil {
			return resolveOnStore(e, s, *asJSON, resolveErr)
		}
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}
	if *asJSON {
		if err := render.NodesJSON(e.stdout, []*graph.Node{node}); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}

	width := render.Width(g.IDs())
	fmt.Fprintf(e.stdout, "%s claimed by %s\n", render.Short(node.ID, width), *holder)
	for _, c := range existing {
		if c.Holder == *holder {
			continue
		}
		fmt.Fprintf(e.stderr, "note: %s is also claimed by %s (%s)\n",
			render.Short(node.ID, width), c.Holder, c.Source)
	}
	return exitOK
}

func cmdRelease(e *env, args []string) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	holder := fs.String("holder", "", "whose claim to release (default: yours)")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "weft release: want exactly one node id, prefix, or ref")
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)
	if *holder == "" || *holder == "me" {
		*holder = by
	}

	now := time.Now()
	var node *graph.Node
	var resolveErr error
	var released *graph.Claim
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if node, err = g.Resolve(fs.Arg(0)); err != nil {
			resolveErr = err
			return nil, err
		}
		released = g.ClaimBy(node.ID, *holder, now)
		if released == nil {
			// Nothing to release is not a failure — the desired state already
			// holds (SPEC.md §9.3).
			return nil, nil
		}
		ev, err := event.ClaimRelease(released.ID, by, event.SourceAsserted, now)
		if err != nil {
			return nil, err
		}
		return []event.Event{ev}, nil
	})
	if err != nil {
		if resolveErr != nil {
			return resolveOnStore(e, s, *asJSON, resolveErr)
		}
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	if *asJSON {
		// Same reason as unlink: nothing to release and a release that happened
		// are both exit 0, so the flag is the only thing telling them apart.
		if err := render.ChangedJSON(e.stdout, []*graph.Node{node}, "released", released != nil); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	g, _ := graph.Fold(s.Fold)
	width := render.Width(g.IDs())
	if released == nil {
		fmt.Fprintf(e.stdout, "%s had no live claim by %s\n", render.Short(node.ID, width), *holder)
		return exitOK
	}
	fmt.Fprintf(e.stdout, "%s released by %s\n", render.Short(node.ID, width), *holder)
	return exitOK
}

// cmdState backs both done and drop, and their --undo form. Reopening is a
// node.state back to open, not a retraction: the node was genuinely done for a
// while, and the log should say so (EVENTS.md §3.5).
func cmdState(target string) func(*env, []string) int {
	return func(e *env, args []string) int {
		name := "done"
		if target == event.StateDropped {
			name = "drop"
		}
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(e.stderr)
		undo := fs.Bool("undo", false, "return the node to open")
		ifPresent := fs.Bool("if-present", false, "append nothing when the node is already in the target state or does not exist (a writer that re-runs, not a second assertion)")
		asJSON := fs.Bool("json", false, "machine-readable output")

		if code, ok := parseFlags(e, fs, args); !ok {
			return code
		}
		if fs.NArg() != 1 {
			fmt.Fprintf(e.stderr, "weft %s: want exactly one node id, prefix, or ref\n", name)
			return exitUsage
		}
		// An empty query is a caller bug — the classic unset shell variable —
		// not a node that happens to be absent. It must stay a usage error even
		// under --if-present, which would otherwise turn it into a silent
		// exit-0 no-op, the same refusal add makes for an empty title.
		if strings.TrimSpace(fs.Arg(0)) == "" {
			fmt.Fprintf(e.stderr, "weft %s: the node to %s cannot be empty\n", name, name)
			return exitUsage
		}

		state := target
		if *undo {
			state = event.StateOpen
		}

		s, err := store.Open()
		if err != nil {
			return fail(e, exitStore, *asJSON, err)
		}
		dir, _ := store.Dir()
		by := actor.Resolve(dir)

		var node *graph.Node
		var resolveErr error
		// Whether anything was appended, for --if-present's degraded report:
		// "already there" and "no such node" are both the desired state holding.
		wrote := false
		err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
			wrote = false
			g, err := graph.Fold(fold)
			if err != nil {
				return nil, err
			}
			if node, err = g.Resolve(fs.Arg(0)); err != nil {
				var notFound *graph.ErrNotFound
				if *ifPresent && errors.As(err, &notFound) {
					// The node the writer would retract does not exist, so the
					// retraction already holds (SPEC.md §9.3). Ambiguity is not
					// tolerated the same way: the writer named something, and
					// picking one of several matches would retract on its behalf.
					node = nil
					return nil, nil
				}
				resolveErr = err
				return nil, err
			}
			if *ifPresent && node.State == state {
				return nil, nil
			}
			ev, err := event.NodeState(node.ID, state, by, event.SourceAsserted, time.Now())
			if err != nil {
				return nil, err
			}
			wrote = true
			return []event.Event{ev}, nil
		})
		if err != nil {
			if resolveErr != nil {
				return resolveOnStore(e, s, *asJSON, resolveErr)
			}
			return fail(e, storeExitCode(err), *asJSON, err)
		}
		if node == nil {
			// --if-present on a query that matched nothing.
			if *asJSON {
				if err := render.ChangedJSON(e.stdout, nil, state, false); err != nil {
					return fail(e, exitStore, false, err)
				}
				return exitOK
			}
			fmt.Fprintf(e.stdout, "nothing %s: no node matches %q\n", state, fs.Arg(0))
			return exitOK
		}

		g, err := graph.Fold(s.Fold)
		if err != nil {
			return fail(e, storeExitCode(err), *asJSON, err)
		}
		if *asJSON {
			// The changed field is emitted unconditionally, the way unlink and
			// release emit theirs — a schema that varies with a flag makes the
			// consumer track which flags the caller passed. Without --if-present
			// the field is always true, which is exactly what makes it safe to
			// rely on.
			if err := render.ChangedJSON(e.stdout, []*graph.Node{g.Nodes[node.ID]}, state, wrote); err != nil {
				return fail(e, exitStore, false, err)
			}
			return exitOK
		}
		if !wrote {
			fmt.Fprintf(e.stdout, "%s already %s\n", render.Short(node.ID, render.Width(g.IDs())), state)
			return exitOK
		}
		fmt.Fprintf(e.stdout, "%s %s\n", render.Short(node.ID, render.Width(g.IDs())), state)
		return exitOK
	}
}

// resolveOnStore re-folds so a resolution failure can be rendered with
// candidates. The transaction has already rolled back at this point.
func resolveOnStore(e *env, s *store.Store, asJSON bool, resolveErr error) int {
	g, foldErr := graph.Fold(s.Fold)
	if foldErr != nil {
		return fail(e, storeExitCode(foldErr), asJSON, foldErr)
	}
	return resolveFailure(e, g, asJSON, resolveErr)
}
