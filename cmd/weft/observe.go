package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/store"
)

// refObserve refreshes the cached facts behind a node's refs (SPEC.md §6.3):
// the artifact's own created/modified stamps and a content hash, recorded as
// ref.observe events. It speaks the schemes observable without a network —
// file: today — and leaves gh:/pr: to `weft import`, which stays the one place
// weft shells out.
//
// An observation is definitionally what was read from the world, so unlike the
// other ref verbs there is no --source flag: it is always "observed", and
// accepting "asserted" here would let the log claim a stat that never ran.
func refObserve(e *env, args []string) int {
	fs := flag.NewFlagSet("ref observe", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(e.stderr, "weft ref observe: want <node> [REF], e.g. weft ref observe a3f file:docs/plan.md")
		return exitUsage
	}
	nodeQuery, only := fs.Arg(0), fs.Arg(1)

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)

	// The stat and the hash run before the lock, the way import fetches before
	// its transaction: hashing a large file under the exclusive lock would
	// serialize every writer behind this command's IO. The world cannot race
	// the log — what is read here is a cache either way — and the write-side
	// diff still happens inside the transaction (EVENTS.md §3.9).
	pre, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}
	node, err := pre.Resolve(nodeQuery)
	if err != nil {
		return resolveOnStore(e, s, *asJSON, err)
	}
	refs := node.Refs
	if only != "" {
		if !slices.Contains(node.Refs, only) {
			// The fold keys observations under the node's refs, so observing
			// an unbound ref would land in a slot no query reads — written,
			// exit 0, and invisible. Refusing is the honest answer.
			fmt.Fprintf(e.stderr, "weft ref observe: %s is not bound to this node; bind it first: weft ref add %s %s\n",
				only, nodeQuery, only)
			return exitUsage
		}
		refs = []string{only}
	}

	// A relative file: path names a file in the node's own project, but the
	// store is global, so the only honest base is a checkout of that project.
	// Resolving against whatever directory the command ran in would hash some
	// other repo's README and record it as observed. The checkout root, not the
	// cwd, is the base so a run from a subdirectory reads the same file.
	cwd, cwdErr := os.Getwd()
	here := currentNS(dir)

	var res observeResult
	payloads := make(map[string]event.ObservePayload)
	for _, ref := range refs {
		path, isFile := strings.CutPrefix(ref, "file:")
		if !isFile {
			reason := "scheme is not observable"
			if strings.HasPrefix(ref, "gh:") || strings.HasPrefix(ref, "pr:") {
				reason = "weft import observes gh: and pr:"
			}
			res.skip(ref, reason)
			continue
		}
		// file:p#L10 names a place in the file; the artifact is the file.
		path, _, _ = strings.Cut(path, "#")
		if !filepath.IsAbs(path) {
			if cwdErr != nil || here != node.NS {
				res.skip(ref, fmt.Sprintf("relative path; run from a checkout of %s", node.NS))
				continue
			}
			path = filepath.Join(checkoutRoot(cwd), path)
		}
		p, err := observeFile(path)
		if err != nil {
			// A missing file is a normal state, not an error (SPEC.md §9.3):
			// the path may live on another machine or an unchecked-out branch.
			// Reported, so silence cannot read as observed.
			res.skip(ref, err.Error())
			continue
		}
		payloads[ref] = p
	}

	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		// Re-resolve by the id captured outside, never the original query: the
		// id survives a racing merge through the alias map, while the query
		// could land somewhere new (EVENTS.md §6).
		fresh, err := g.Resolve(node.ID)
		if err != nil {
			return nil, err
		}
		res.id = fresh.ID
		// One instant for the batch is safe because the records differ by ref
		// — unlike two same-titled adds, two observations in one run can never
		// hash identically.
		now := time.Now()
		var out []event.Event
		for _, ref := range refs {
			p, ok := payloads[ref]
			if !ok {
				continue
			}
			if !slices.Contains(fresh.Refs, ref) {
				// Unbound between the pre-lock read and the transaction; see
				// the unbound refusal above for why it must not be written.
				res.skip(ref, "no longer bound")
				continue
			}
			if !g.Observed(fresh.ID, ref).Changed(p) {
				res.unchanged = append(res.unchanged, ref)
				continue
			}
			ev, err := event.RefObserve(fresh.ID, ref, p, by, event.SourceObserved, now)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
			res.observed = append(res.observed, ref)
		}
		return out, nil
	})
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	if *asJSON {
		if err := res.renderJSON(e.stdout); err != nil {
			return degraded(e, "observations written", "rendering failed", err)
		}
		return exitOK
	}
	res.render(e.stdout)
	return exitOK
}

// observeResult records what one run did, so the report states facts rather
// than counting attempts — the same contract as import's summary.
type observeResult struct {
	id        string
	observed  []string
	unchanged []string
	skipped   []skippedRef
}

type skippedRef struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

func (r *observeResult) skip(ref, reason string) {
	r.skipped = append(r.skipped, skippedRef{Ref: ref, Reason: reason})
}

func (r *observeResult) render(w io.Writer) {
	if len(r.observed)+len(r.unchanged)+len(r.skipped) == 0 {
		fmt.Fprintln(w, "no refs to observe")
		return
	}
	for _, ref := range r.observed {
		fmt.Fprintf(w, "observed   %s\n", ref)
	}
	for _, ref := range r.unchanged {
		fmt.Fprintf(w, "unchanged  %s\n", ref)
	}
	for _, sk := range r.skipped {
		fmt.Fprintf(w, "skipped    %s  (%s)\n", sk.Ref, sk.Reason)
	}
}

func (r *observeResult) renderJSON(w io.Writer) error {
	out := struct {
		ID        string       `json:"id"`
		Observed  []string     `json:"observed,omitempty"`
		Unchanged []string     `json:"unchanged,omitempty"`
		Skipped   []skippedRef `json:"skipped,omitempty"`
	}{ID: r.id, Observed: r.observed, Unchanged: r.unchanged, Skipped: r.skipped}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// observeFile stats and hashes one file into an observation payload.
//
// The hash is why this reads the whole file: modification times lie — a touch,
// a checkout, a copy all move mtime without changing a byte — and the hash is
// also what makes a rename detectable later (SPEC.md §6.3). Created is present
// only where the platform reports a birth time; absence means "not reported",
// never "born at zero" (EVENTS.md §3.8).
func observeFile(path string) (event.ObservePayload, error) {
	var p event.ObservePayload
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("no such file")
	}
	if err != nil {
		// Permission and I/O errors are not absence: reporting them as missing
		// invites a rebind (ref rm + ref add) that records a false address change.
		return p, fmt.Errorf("unreadable: %v", err)
	}
	if fi.IsDir() {
		return p, fmt.Errorf("is a directory")
	}
	p.Modified = fi.ModTime().UTC().Format(time.RFC3339Nano)
	if bt, ok := birthtime(fi); ok {
		p.Created = bt.UTC().Format(time.RFC3339Nano)
	}
	f, err := os.Open(path)
	if err != nil {
		return p, fmt.Errorf("unreadable: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return p, fmt.Errorf("unreadable: %v", err)
	}
	p.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return p, nil
}

// checkoutRoot returns the nearest ancestor of dir holding a .git entry (a
// directory in a clone, a file in a worktree), or dir itself outside any
// checkout. It walks the filesystem rather than asking git, so observing costs
// no extra process (BUILD.md §1.1).
func checkoutRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}
