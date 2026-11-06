package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// cmdRef binds an external object to a node that already exists, and takes the
// binding back.
//
// SPEC §4 models refs as bindings "added over time as reality catches up", and
// EVENTS.md §3.4 calls ref.add "the general attach-a-ref affordance the CLI was
// missing" — but until now the only writers were `add`, which acts at creation,
// and `import`, which only speaks gh. So a node whose GitHub issue appeared a
// week later, or whose file moved, could not be rebound: the escape hatch was
// `drop` and re-`add`, which loses the node's edges.
//
// Sub-verbs rather than two top-level commands. `import gh` already establishes
// the shape, the pair belongs together under the noun people look for, and the
// CLI surface is a deliberately small documented thing (SPEC.md §6).
func cmdRef(e *env, args []string) int {
	if len(args) == 0 {
		// Named no subcommand — a missing argument, so a usage error. Still
		// print the grammar, since that is the question the reader had.
		refUsage(e, e.stderr)
		return exitUsage
	}
	switch args[0] {
	case "add":
		return refMutate(e, "add", args[1:])
	case "rm":
		return refMutate(e, "rm", args[1:])
	case "observe":
		return refObserve(e, args[1:])
	case "-h", "--help", "help":
		// The subcommand grammar is the thing a reader cannot discover, which
		// makes this the one command where -h failing costs the most. Every
		// other command answers it by printing its flags; this one used to
		// answer "unknown subcommand", sending anyone asking what the verbs are
		// away with the fact that they had not named one.
		//
		// Asking for help is a success, not a usage error: exit 0
		// on stdout, distinguishable from the typo below which stays exit 1 on
		// stderr.
		refUsage(e, e.stdout)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "weft ref: unknown subcommand %q (want add, rm or observe)\n", args[0])
		refUsage(e, e.stderr)
		return exitUsage
	}
}

// refUsage prints the grammar and the shared flags together to w. The flags
// come from the same FlagSet the subcommands parse with, so a flag added there
// cannot go unmentioned here. The writer varies because a help request prints
// to stdout while a usage error prints to stderr.
func refUsage(e *env, w io.Writer) {
	fmt.Fprint(w, `Usage of ref:
  weft ref add <node> <ref>        bind an external object to a node that exists
  weft ref rm  <node> <ref>        take the binding back
  weft ref observe <node> [REF]    refresh the cached facts behind the node's
                                   file: refs — stamps and a content hash — or
                                   just REF; unchanged files write nothing

add and rm are exit 0 when the binding is already there or already gone, so
they are safe to run when you are unsure. <ref> is any scheme:value —
gh:acme/api#12, pr:acme/api#45, branch:fix/n-plus-1, file:docs/export.md,
session:<uuid>. gh: and pr: are the ones `+"`weft import`"+` binds to and
observes; observe takes --json but not --source, because an observation is
definitionally observed.

`)
	fs, _, _ := refFlags(e, "ref")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// refFlags is the flag set both subcommands share, with its bindings. One
// definition, so the usage above cannot drift from what the subcommands parse.
func refFlags(e *env, name string) (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	asJSON := fs.Bool("json", false, "machine-readable output")
	return fs, source, asJSON
}

func refMutate(e *env, verb string, args []string) int {
	fs, source, asJSON := refFlags(e, "ref "+verb)

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(e.stderr, "weft ref %s: want <node> <ref>, e.g. weft ref %s a3f gh:acme/api#12\n", verb, verb)
		return exitUsage
	}
	nodeQuery, ref := fs.Arg(0), fs.Arg(1)
	if ref == "" {
		fmt.Fprintf(e.stderr, "weft ref %s: a ref cannot be empty\n", verb)
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft ref %s: unknown source %q\n", verb, *source)
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)

	var node *graph.Node
	var resolveErr error
	var changed bool
	// Resolution and the has-it-already check both run inside the transaction,
	// against the same fold the append lands on. Checking outside would let two
	// processes each decide the ref was absent and both write it (EVENTS.md §6).
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if node, err = g.Resolve(nodeQuery); err != nil {
			resolveErr = err
			return nil, err
		}
		now := time.Now()

		if verb == "add" {
			// Already bound means the desired state holds. Re-emitting would leave
			// the ref list unchanged and grow the log on every run — the fold's
			// tolerance for a redundant ref.add is not licence to write one
			// (EVENTS.md §3.9).
			if slices.Contains(node.Refs, ref) {
				return nil, nil
			}
			r := event.New(event.KindRefAdd, by, *source, now)
			r["node"] = node.ID
			r["ref"] = ref
			if err := r.Seal(); err != nil {
				return nil, err
			}
			changed = true
			return []event.Event{r}, nil
		}

		// Retract every assertion of this binding, not just one: leaving another
		// live would drop the ref from the node while the log still says it holds.
		ids := g.FindRefs(node.ID, ref)
		if len(ids) == 0 {
			return nil, nil
		}
		out := make([]event.Event, 0, len(ids))
		for _, id := range ids {
			// One `now` for the batch is safe because the records differ by target.
			r := event.New(event.KindRefRetract, by, *source, now)
			r["target"] = id
			if err := r.Seal(); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		changed = true
		return out, nil
	})
	if err != nil {
		if resolveErr != nil {
			return resolveOnStore(e, s, *asJSON, resolveErr)
		}
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	// Re-read so the reported node carries the binding as it now stands, rather
	// than as it was when resolved inside the transaction. Refresh by the id
	// captured there, never by the original query: `rm` may have just removed
	// this ref from the only live holder, and re-resolving the ref would then
	// land on a *different* node — a dropped holder the command never touched —
	// and report it as the one that changed.
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}
	if fresh, err := g.Resolve(node.ID); err == nil {
		node = fresh
	}

	// A duplicate binding gets the same note add prints, off the same fold the
	// width uses. ref add is the one command that can make a ref *ambiguous*
	// between live holders — an exact duplicate means neither resolves — so
	// staying silent here while add warns would put the silence on the worse
	// case.
	if verb == "add" && changed {
		printRefNotes(e, g, []string{ref}, node.ID)
	}

	if *asJSON {
		field := "bound"
		if verb == "rm" {
			field = "retracted"
		}
		if err := render.ChangedJSON(e.stdout, []*graph.Node{node}, field, changed); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	short := render.Short(node.ID, render.Width(g.IDs()))
	switch {
	case verb == "add" && changed:
		fmt.Fprintf(e.stdout, "%s bound to %s\n", short, ref)
	case verb == "add":
		fmt.Fprintf(e.stdout, "%s was already bound to %s\n", short, ref)
	case changed:
		fmt.Fprintf(e.stdout, "%s no longer bound to %s\n", short, ref)
	default:
		fmt.Fprintf(e.stdout, "%s was not bound to %s\n", short, ref)
	}
	return exitOK
}
