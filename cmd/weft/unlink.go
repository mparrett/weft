package main

import (
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

// cmdUnlink retracts an edge.
//
// Modeling is iterative: you link, you read the source, and you find you had
// the direction or the lineage backwards. Until this existed the only recourse
// was to add a correction node saying the edge above it is wrong, which leaves
// the graph asserting something false with a footnote nearby — and every query
// reads the assertion, not the footnote.
//
// Retraction is not deletion. Both events stay in the log, so who asserted the
// edge and who took it back are both still answerable (EVENTS.md §3.3). That is
// also why this targets the edge.add event's id rather than the triple: the
// retraction names a specific assertion someone made.
func cmdUnlink(e *env, args []string) int {
	fs := flag.NewFlagSet("unlink", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 3 {
		fmt.Fprintf(e.stderr, "weft unlink: want <from> <kind> <to>, e.g. weft unlink a3f depends-on 9b0\n"+
			"kinds: %s\n", strings.Join(validEdgeKinds, ", "))
		return exitUsage
	}
	fromQuery, kind, toQuery := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	if !contains(validEdgeKinds, kind) {
		fmt.Fprintf(e.stderr, "weft unlink: unknown edge kind %q (want one of: %s)\n",
			kind, strings.Join(validEdgeKinds, ", "))
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft unlink: unknown source %q\n", *source)
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)

	var from, to *graph.Node
	var resolveErr error
	var found *graph.Edge
	// Resolution and lookup both happen inside the transaction, against the same
	// fold the append lands on. An edge asserted by a concurrent process between
	// a lookup out here and the append would otherwise survive an unlink that
	// reported success (EVENTS.md §6). cmd/weft/link.go is the same shape.
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if from, err = g.Resolve(fromQuery); err != nil {
			resolveErr = err
			return nil, err
		}
		if to, err = g.Resolve(toQuery); err != nil {
			resolveErr = err
			return nil, err
		}
		found = g.FindEdge(kind, from.ID, to.ID)
		if found == nil {
			// No such edge means the desired state already holds, which is not a
			// failure (EVENTS.md §3.3, SPEC.md §9.3). Appending a retract that
			// names nothing would fold to this same graph while growing the log on
			// every run — tolerance on the read side is not licence to write
			// (EVENTS.md §3.9).
			return nil, nil
		}
		ev, err := event.EdgeRetract(found.ID, by, *source, time.Now())
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
		// `retracted` is the whole answer for a caller that cannot read the text
		// form: exit 0 covers both a retraction and a no-op, on purpose.
		if err := render.ChangedJSON(e.stdout, []*graph.Node{from, to}, "retracted", found != nil); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), false, err)
	}
	width := render.Width(g.IDs())
	if found == nil {
		fmt.Fprintf(e.stdout, "no %s edge from %s to %s\n",
			kind, render.Short(from.ID, width), render.Short(to.ID, width))
		return exitOK
	}
	fmt.Fprintf(e.stdout, "unlinked %s %s %s\n",
		render.Short(from.ID, width), kind, render.Short(to.ID, width))
	return exitOK
}
