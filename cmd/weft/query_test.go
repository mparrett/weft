package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// why must name the blocker and, when there is one, its holder. A bare status
// is not actionable (SPEC.md §6.1).
func TestWhyNeverPrintsBareBlocked(t *testing.T) {
	scratch(t)
	a := addNode(t, "downstream", "task")
	b := addNode(t, "the blocker", "task")
	session(t, "link", a, "depends-on", b)
	session(t, "claim", b, "--holder", "user:colleague", "--source", "inferred")

	code, stdout, stderr := session(t, "why", a)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"the blocker", "held by", "user:colleague", "inferred"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("why output missing %q:\n%s", want, stdout)
		}
	}
}

// `why` once printed blockers as peers when one gates the others; this pins
// the fix at the CLI level. Nearest blockers are a set, not an order, and a
// reader who starts on a gated one has the work re-done when its gate lands.
func TestWhyMarksTheBlockerThatGatesTheOthers(t *testing.T) {
	scratch(t)
	a := addNode(t, "A: the task you asked about", "task")
	// Added before the chokepoint, so file order alone would print it first.
	c := addNode(t, "C: waits on B", "task")
	b := addNode(t, "B: the chokepoint", "task")
	session(t, "link", a, "depends-on", b)
	session(t, "link", a, "depends-on", c)
	session(t, "link", c, "depends-on", b)

	_, stdout, _ := session(t, "why", a)
	if !strings.Contains(stdout, "1 other blocker here waits on this") {
		t.Errorf("why did not say which blocker gates the other:\n%s", stdout)
	}
	if strings.Index(stdout, "B: the chokepoint") > strings.Index(stdout, "C: waits on B") {
		t.Errorf("the chokepoint should be listed first:\n%s", stdout)
	}

	// The count is where an agent choosing what to start needs it.
	var got struct {
		Blockers []struct {
			Title string `json:"title"`
			Gates int    `json:"gates"`
		} `json:"blockers"`
	}
	_, out, _ := session(t, "why", a, "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("why --json is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Blockers) != 2 {
		t.Fatalf("got %d blockers, want 2", len(got.Blockers))
	}
	if got.Blockers[0].Gates != 1 || got.Blockers[1].Gates != 0 {
		t.Errorf("gates = %d, %d; want 1, 0", got.Blockers[0].Gates, got.Blockers[1].Gates)
	}
}

// A lone blocker gates nothing, and saying so would be noise on the common case.
func TestWhyIsSilentWhenNothingGates(t *testing.T) {
	scratch(t)
	a := addNode(t, "downstream", "task")
	b := addNode(t, "the blocker", "task")
	session(t, "link", a, "depends-on", b)

	if _, stdout, _ := session(t, "why", a); strings.Contains(stdout, "waits on this") {
		t.Errorf("a single blocker should carry no gating line:\n%s", stdout)
	}
}

// The wording matters: an incomplete graph can only say nothing recorded is in
// the way, never that the work is ready.
func TestWhyUnblockedIsOptimisticallyWorded(t *testing.T) {
	scratch(t)
	a := addNode(t, "free", "task")
	_, stdout, _ := session(t, "why", a)
	if !strings.Contains(stdout, "no known blocker") {
		t.Errorf("expected optimistic wording, got:\n%s", stdout)
	}
}

// The rule from review finding 6, at the CLI level.
func TestReadyHonoursCrossNamespaceBlockers(t *testing.T) {
	scratch(t)
	local := addNodeFull(t, "local work", "task", "--ns", "acme/api")
	upstream := addNodeFull(t, "upstream blocker", "task", "--ns", "other/fork")
	session(t, "link", local, "depends-on", upstream)

	// Listing is scoped to one namespace; readiness is not.
	_, ready, _ := session(t, "ls", "--ready", "--ns", "acme/api")
	if strings.Contains(ready, "local work") {
		t.Errorf("namespace filter hid a cross-namespace blocker:\n%s", ready)
	}
	_, blocked, _ := session(t, "ls", "--blocked", "--ns", "acme/api")
	if !strings.Contains(blocked, "local work") {
		t.Errorf("expected the node to be blocked:\n%s", blocked)
	}

	session(t, "done", upstream)
	_, ready2, _ := session(t, "ls", "--ready", "--ns", "acme/api")
	if !strings.Contains(ready2, "local work") {
		t.Errorf("node still blocked after the dependency was done:\n%s", ready2)
	}
	// The blocker itself stays out of the acme/api listing throughout.
	if strings.Contains(ready2, "upstream blocker") {
		t.Errorf("namespace filter leaked:\n%s", ready2)
	}
}

func TestRdepsDepth(t *testing.T) {
	scratch(t)
	base := addNode(t, "base", "task")
	mid := addNode(t, "mid", "task")
	top := addNode(t, "top", "task")
	session(t, "link", mid, "depends-on", base)
	session(t, "link", top, "depends-on", mid)

	_, all, _ := session(t, "rdeps", base)
	for _, want := range []string{"mid", "top"} {
		if !strings.Contains(all, want) {
			t.Errorf("rdeps missing %q:\n%s", want, all)
		}
	}
	_, shallow, _ := session(t, "rdeps", base, "--depth", "1")
	if strings.Contains(shallow, "top") {
		t.Errorf("--depth 1 walked too far:\n%s", shallow)
	}
}

func TestPathAndVia(t *testing.T) {
	scratch(t)
	task := addNode(t, "task 7", "task")
	z := addNode(t, "review point Z", "finding")
	pr := addNode(t, "PR 77", "pr")
	session(t, "link", task, "depends-on", z)
	session(t, "link", z, "part-of", pr)

	_, route, _ := session(t, "path", task, pr)
	for _, want := range []string{"depends on", "part of", "PR 77"} {
		if !strings.Contains(route, want) {
			t.Errorf("route missing %q:\n%s", want, route)
		}
	}
	// Restricting the kind breaks the route, and that is an answer not an error.
	code, restricted, _ := session(t, "path", task, pr, "--via", "depends-on")
	if code != exitOK {
		t.Errorf("no route should exit 0, got %d", code)
	}
	if !strings.Contains(restricted, "no known route") {
		t.Errorf("expected no route via depends-on alone:\n%s", restricted)
	}
}

// One kind at a time cannot express "the directed family", which is the
// restriction a reader actually wants: a symmetric edge is a free bidirectional
// shortcut between nodes that exist to say they are alternatives, and excluding
// it was impossible while --via took a single kind.
func TestPathViaTakesSeveralKinds(t *testing.T) {
	scratch(t)
	task := addNode(t, "task 7", "task")
	z := addNode(t, "review point Z", "finding")
	pr := addNode(t, "PR 77", "pr")
	session(t, "link", task, "depends-on", z)
	session(t, "link", z, "part-of", pr)

	// Neither kind alone reaches PR 77; both together do.
	for _, one := range []string{"depends-on", "part-of"} {
		if _, out, _ := session(t, "path", task, pr, "--via", one); !strings.Contains(out, "no known route") {
			t.Errorf("--via %s alone should not reach the PR:\n%s", one, out)
		}
	}
	code, out, stderr := session(t, "path", task, pr, "--via", "depends-on,part-of")
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	if !strings.Contains(out, "PR 77") {
		t.Errorf("--via with both kinds did not find the route:\n%s", out)
	}

	// Spacing and repeats are what a human types, not an error to report back.
	if _, spaced, _ := session(t, "path", task, pr, "--via", " depends-on , part-of , depends-on "); !strings.Contains(spaced, "PR 77") {
		t.Errorf("--via did not tolerate spacing and repeats:\n%s", spaced)
	}
}

// A route that crosses a symmetric edge looks wrong and is not: the walk was
// allowed to. Naming the permitted set is what turns that from a puzzle into a
// diagnosis, the way `weft ns` names the rule and not just the answer.
func TestPathReportsWhichKindsItConsidered(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "beta", "task")
	session(t, "link", a, "depends-on", b)

	_, out, _ := session(t, "path", a, b)
	if !strings.Contains(out, "via: ") {
		t.Fatalf("path did not say which kinds it walked:\n%s", out)
	}
	// Unrestricted means every kind, including the symmetric ones that produce
	// the surprising routes — so the default has to name them, not summarize.
	for _, kind := range validEdgeKinds {
		if !strings.Contains(out, kind) {
			t.Errorf("default via line omits %q:\n%s", kind, out)
		}
	}
	if !strings.Contains(out, "--via") {
		t.Errorf("default via line does not say the walk can be restricted:\n%s", out)
	}

	_, restricted, _ := session(t, "path", a, b, "--via", "depends-on")
	if !strings.Contains(restricted, "via: depends-on (") {
		t.Errorf("restricted via line should name exactly the kinds asked for:\n%s", restricted)
	}
	if strings.Contains(restricted, "conflicts-with") {
		t.Errorf("restricted via line names a kind that was excluded:\n%s", restricted)
	}

	// An absent route and one excluded by the filter print the same first line,
	// so the via line is the only thing telling them apart.
	_, noRoute, _ := session(t, "path", a, b, "--via", "same-as")
	if !strings.Contains(noRoute, "no known route") || !strings.Contains(noRoute, "via: same-as") {
		t.Errorf("a filtered-out route must still report its filter:\n%s", noRoute)
	}
}

// The path --via direction repro (it restricted edge kind but not direction). Two nodes that depend on a third and on nothing else
// are connected through it, so the default walk finds a route — and a reader who
// asked `--via depends-on` reads that route as a dependency. --dir turns the same
// command into the reachability question the flag combination invites.
func TestPathDirRestrictsTheWalkToOneDirection(t *testing.T) {
	scratch(t)
	x := addNode(t, "X: independent of Y", "task")
	y := addNode(t, "Y: independent of X", "task")
	z := addNode(t, "Z: both depend on it", "task")
	session(t, "link", x, "depends-on", z)
	session(t, "link", y, "depends-on", z)

	if _, either, _ := session(t, "path", x, y, "--via", "depends-on"); !strings.Contains(either, "blocks") {
		t.Fatalf("the connectivity route through Z should still be found:\n%s", either)
	}
	code, out, stderr := session(t, "path", x, y, "--via", "depends-on", "--dir", "out")
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	if !strings.Contains(out, "no known route") {
		t.Errorf("X does not depend on Y, so a forwards walk must not reach it:\n%s", out)
	}
	if !strings.Contains(out, "forwards only") {
		t.Errorf("the via line must say the walk was directional:\n%s", out)
	}

	// The direction that does hold, from both ends.
	if _, fwd, _ := session(t, "path", x, z, "--dir", "out"); !strings.Contains(fwd, "Z: both depend on it") {
		t.Errorf("--dir out should walk X's own dependency:\n%s", fwd)
	}
	if _, back, _ := session(t, "path", z, x, "--dir", "in"); !strings.Contains(back, "X: independent of Y") {
		t.Errorf("--dir in should reach the dependent:\n%s", back)
	}
	if _, back, _ := session(t, "path", x, z, "--dir", "in"); !strings.Contains(back, "no known route") {
		t.Errorf("--dir in must not walk a dependency forwards:\n%s", back)
	}
}

// A symmetric kind has no direction to restrict — its stored orientation is the
// fold's normalization — so a directional walk still crosses it, and the via line
// has to admit that or "forwards only" overstates what was excluded.
func TestPathDirStillCrossesSymmetricKinds(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "beta", "task")
	session(t, "link", b, "conflicts-with", a)

	_, out, _ := session(t, "path", a, b, "--via", "conflicts-with", "--dir", "out")
	if !strings.Contains(out, "beta") {
		t.Errorf("a symmetric edge must stay crossable under --dir out:\n%s", out)
	}
	if !strings.Contains(out, "conflicts-with is symmetric") {
		t.Errorf("the via line must say which kinds ignored the direction:\n%s", out)
	}
}

func TestPathJSONCarriesTheKindsWalked(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "beta", "task")
	session(t, "link", a, "depends-on", b)

	var got struct {
		Hops []struct {
			Kind string `json:"kind"`
		} `json:"hops"`
		Via []string `json:"via"`
		Dir string   `json:"dir"`
	}
	_, out, _ := session(t, "path", a, b, "--via", "depends-on,part-of", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("path --json is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Hops) != 1 || got.Hops[0].Kind != "depends-on" {
		t.Errorf("unexpected hops: %+v", got.Hops)
	}
	if len(got.Via) != 2 || got.Via[0] != "depends-on" || got.Via[1] != "part-of" {
		t.Errorf("via = %v, want the two kinds asked for", got.Via)
	}
	// Kind without direction is half the filter: an agent reading hops cannot
	// tell a reachability answer from a connectivity one without this.
	if got.Dir != "either" {
		t.Errorf("dir = %q, want the default", got.Dir)
	}
	_, forwards, _ := session(t, "path", a, b, "--dir", "out", "--json")
	if err := json.Unmarshal([]byte(forwards), &got); err != nil {
		t.Fatalf("path --dir out --json is not valid JSON: %v\n%s", err, forwards)
	}
	if got.Dir != "out" {
		t.Errorf("dir = %q, want out", got.Dir)
	}

	// The no-route case is where an agent most needs it: an empty hop list plus
	// the filter is a different fact from an empty hop list and no filter.
	_, none, _ := session(t, "path", a, b, "--via", "same-as", "--json")
	got.Via = nil
	if err := json.Unmarshal([]byte(none), &got); err != nil {
		t.Fatalf("no-route --json is not valid JSON: %v\n%s", err, none)
	}
	if len(got.Hops) != 0 {
		t.Errorf("expected no hops, got %+v", got.Hops)
	}
	if len(got.Via) != 1 || got.Via[0] != "same-as" {
		t.Errorf("via = %v, want [same-as]", got.Via)
	}
}

func TestQueryJSONShapes(t *testing.T) {
	scratch(t)
	a := addNode(t, "downstream", "task")
	b := addNode(t, "blocker", "task")
	session(t, "link", a, "depends-on", b)

	_, whyJSON, _ := session(t, "why", a, "--json")
	var why struct {
		Blockers []struct {
			ID    string   `json:"id"`
			Path  []string `json:"path"`
			Known bool     `json:"known"`
		} `json:"blockers"`
	}
	if err := json.Unmarshal([]byte(whyJSON), &why); err != nil {
		t.Fatalf("why --json: %v\n%s", err, whyJSON)
	}
	if len(why.Blockers) != 1 || !why.Blockers[0].Known {
		t.Fatalf("unexpected blockers: %+v", why.Blockers)
	}
	// Full ids everywhere in machine output.
	if len(why.Blockers[0].ID) != 64 {
		t.Errorf("blocker id is abbreviated: %q", why.Blockers[0].ID)
	}
	for _, id := range why.Blockers[0].Path {
		if len(id) != 64 {
			t.Errorf("path id is abbreviated: %q", id)
		}
	}

	_, rdepsJSON, _ := session(t, "rdeps", b, "--json")
	if !strings.Contains(rdepsJSON, `"depth": 1`) {
		t.Errorf("rdeps --json missing depth:\n%s", rdepsJSON)
	}
	_, pathJSON, _ := session(t, "path", a, b, "--json")
	if !strings.Contains(pathJSON, `"kind": "depends-on"`) {
		t.Errorf("path --json missing hop kind:\n%s", pathJSON)
	}
}

func TestQueryUsageErrors(t *testing.T) {
	scratch(t)
	a := addNode(t, "a", "task")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"why no target", []string{"why"}, exitUsage},
		{"why unknown", []string{"why", "ffffff"}, exitNotFound},
		{"rdeps two targets", []string{"rdeps", a, a}, exitUsage},
		{"path one target", []string{"path", a}, exitUsage},
		{"path unknown kind", []string{"path", a, a, "--via", "nonsense"}, exitUsage},
		{"path one bad kind among good", []string{"path", a, a, "--via", "depends-on,nope"}, exitUsage},
		// Separators alone are a typo, not a request to walk everything —
		// silently widening the walk is the opposite of what was asked.
		{"path via only separators", []string{"path", a, a, "--via", ",,"}, exitUsage},
		// A misspelled direction must not silently fall back to the widest walk,
		// which is the answer --dir exists to stop being mistaken for the narrow one.
		{"path unknown dir", []string{"path", a, a, "--dir", "forwards"}, exitUsage},
		{"ls ready and blocked", []string{"ls", "--ready", "--blocked"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := session(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

// The rdeps gloss bug: rdeps is named for dependencies and walks only depends-on, but on
// a real map most edges are part-of and derived-from — so an empty answer read
// as "nothing depends on this" when it meant "nothing through depends-on".
func TestRdepsSaysWhichKindsItWalked(t *testing.T) {
	scratch(t)
	design := addNode(t, "Design: keep the seams in step", "spec")
	task := addNode(t, "Task derived from the design", "task")
	dep := addNode(t, "Real dependent", "task")
	session(t, "link", task, "derived-from", design)
	session(t, "link", dep, "depends-on", design)

	_, out, _ := session(t, "rdeps", design)
	if !strings.Contains(out, "Real dependent") || strings.Contains(out, "derived from") {
		t.Errorf("the default walk should still be depends-on alone:\n%s", out)
	}
	if !strings.Contains(out, "via: depends-on (the default; --via walks other kinds)") {
		t.Errorf("rdeps did not say which kinds it walked:\n%s", out)
	}

	// The silence that started this: show lists the task under "led to", rdeps
	// printed nothing, and nothing said why.
	code, empty, stderr := session(t, "rdeps", task)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(empty, "no known dependent") {
		t.Errorf("an empty answer must say so rather than printing nothing:\n%s", empty)
	}
	if !strings.Contains(empty, "via: depends-on") {
		t.Errorf("an empty answer most needs the kinds it walked:\n%s", empty)
	}

	// And --via reaches the edge that was there all along.
	_, derived, _ := session(t, "rdeps", design, "--via", "derived-from")
	if !strings.Contains(derived, "Task derived from the design") {
		t.Errorf("--via derived-from did not find the task:\n%s", derived)
	}
	if strings.Contains(derived, "Real dependent") {
		t.Errorf("--via replaces the default rather than adding to it:\n%s", derived)
	}
	_, both, _ := session(t, "rdeps", design, "--via", "depends-on,derived-from")
	for _, want := range []string{"Real dependent", "Task derived from the design"} {
		if !strings.Contains(both, want) {
			t.Errorf("--via with both kinds missed %q:\n%s", want, both)
		}
	}
}

// A symmetric kind has no direction, so a reverse walk must find it from either
// endpoint — not from whichever one the fold's normalization made `to`.
func TestRdepsCrossesASymmetricKindFromEitherEnd(t *testing.T) {
	scratch(t)
	a := addNode(t, "Alpha", "task")
	b := addNode(t, "Beta", "task")
	session(t, "link", a, "conflicts-with", b)

	if _, out, _ := session(t, "rdeps", a, "--via", "conflicts-with"); !strings.Contains(out, "Beta") {
		t.Errorf("rdeps did not reach Beta from Alpha:\n%s", out)
	}
	if _, out, _ := session(t, "rdeps", b, "--via", "conflicts-with"); !strings.Contains(out, "Alpha") {
		t.Errorf("rdeps did not reach Alpha from Beta:\n%s", out)
	}
}

func TestRdepsJSONCarriesTheKindsWalked(t *testing.T) {
	scratch(t)
	a := addNode(t, "alone", "task")
	var got struct {
		Dependents []struct{ ID string } `json:"dependents"`
		Via        []string              `json:"via"`
	}
	_, out, _ := session(t, "rdeps", a, "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("rdeps --json is not valid JSON: %v\n%s", err, out)
	}
	// An empty list is where via matters most: no dependents at all reads the
	// same as none through the kinds actually walked.
	if len(got.Dependents) != 0 || len(got.Via) != 1 || got.Via[0] != "depends-on" {
		t.Errorf("dependents=%v via=%v", got.Dependents, got.Via)
	}
}

// The CLI half of the review finding on rdeps's walked kinds: --via is the only way to reach a
// symmetric kind from rdeps, so this shape was unreachable until the flag.
func TestRdepsDoesNotChainConflictsWith(t *testing.T) {
	scratch(t)
	a := addNode(t, "Alpha", "task")
	b := addNode(t, "Beta", "task")
	c := addNode(t, "Gamma", "task")
	session(t, "link", a, "conflicts-with", b)
	session(t, "link", b, "conflicts-with", c)

	_, out, _ := session(t, "rdeps", a, "--via", "conflicts-with")
	if !strings.Contains(out, "Beta") {
		t.Errorf("the asserted conflict is missing:\n%s", out)
	}
	if strings.Contains(out, "Gamma") {
		t.Errorf("A conflicts B and B conflicts C says nothing about A and C:\n%s", out)
	}
}

// The rdeps --via widening bug: a widened walk lists what is reachable, not what waits, and an
// indented tree under a command named for dependencies asserted otherwise —
// rdeps --via part-of framed children as dependents. Once the walk crosses any
// kind beyond depends-on, each row names the relation it was reached through.
func TestRdepsWidenedRowsNameTheirRelation(t *testing.T) {
	scratch(t)
	epic := addNode(t, "Epic: reports", "spec")
	child := addNode(t, "Child one", "task")
	dep := addNode(t, "Real dependent", "task")
	session(t, "link", child, "part-of", epic)
	session(t, "link", dep, "depends-on", epic)

	_, out, _ := session(t, "rdeps", epic, "--via", "part-of,depends-on")
	// The mixed walk is the case that bit: without per-row relations, "is part
	// of a thing that depends on it" and "depends on it" are one tree.
	for _, want := range []string{"Child one  (part of)", "Real dependent  (depends on)"} {
		if !strings.Contains(out, want) {
			t.Errorf("widened walk missing %q:\n%s", want, out)
		}
	}

	// The pure depends-on walk stays bare — there the tree really is the wait,
	// and a label on every row would say the command's own name.
	_, def, _ := session(t, "rdeps", epic)
	if strings.Contains(def, "(depends on)") {
		t.Errorf("default walk grew annotations:\n%s", def)
	}
	_, pure, _ := session(t, "rdeps", epic, "--via", "depends-on")
	if strings.Contains(pure, "(depends on)") {
		t.Errorf("--via depends-on alone is still the pure walk:\n%s", pure)
	}
}

// An empty answer crossed nothing, so it reads "no known dependent" whatever
// --via asked for; the via: line is what says which kinds were open to it.
func TestRdepsEmptyAnswerAlwaysSaysNoKnownDependent(t *testing.T) {
	scratch(t)
	leaf := addNode(t, "leaf", "task")

	for _, args := range [][]string{
		{"rdeps", leaf},
		{"rdeps", leaf, "--via", "part-of"},
	} {
		_, out, _ := session(t, args...)
		if !strings.Contains(out, "no known dependent") {
			t.Errorf("%v: empty answer lost its wording:\n%s", args, out)
		}
	}
}

func TestRdepsJSONCarriesPerRowViaAndParent(t *testing.T) {
	scratch(t)
	epic := addNode(t, "Epic: reports", "spec")
	child := addNode(t, "Child one", "task")
	session(t, "link", child, "part-of", epic)

	// via is a list on every walk, pure ones included, and parent names what
	// the row was reached from — a consumer should not have to know which
	// flags produced the output.
	_, out, _ := session(t, "rdeps", epic, "--json", "--via", "part-of")
	for _, want := range []string{`"via"`, `"part-of"`, `"parent"`} {
		if !strings.Contains(out, want) {
			t.Errorf("--json rows missing %s:\n%s", want, out)
		}
	}
	_, pure, _ := session(t, "rdeps", child, "--json")
	if strings.Contains(pure, `"dependents": [`) && !strings.Contains(pure, `"via"`) {
		t.Errorf("pure-walk rows lost via:\n%s", pure)
	}
}

// Every kind's annotation, pinned individually. The symmetric pair matters
// most: their labels are correct only because edgeLabels holds the same
// reading in both slots, and an asymmetric inverse added later would silently
// mislabel every symmetric row with nothing else failing.
func TestRdepsAnnotatesEveryKindHonestly(t *testing.T) {
	scratch(t)
	root := addNode(t, "the root", "spec")
	cases := []struct{ kind, want string }{
		{"part-of", "(part of)"},
		{"derived-from", "(derived from)"},
		{"supersedes", "(supersedes)"},
		{"conflicts-with", "(conflicts with)"},
		{"same-as", "(same as)"},
	}
	for i, c := range cases {
		n := addNode(t, "row "+string(rune('a'+i)), "task")
		if code, _, stderr := session(t, "link", n, c.kind, root); code != exitOK {
			t.Fatalf("link %s: %s", c.kind, stderr)
		}
		_, out, _ := session(t, "rdeps", root, "--via", c.kind)
		if !strings.Contains(out, c.want) {
			t.Errorf("--via %s row missing %s:\n%s", c.kind, c.want, out)
		}
	}
}

// Level-order output does not place a deep row under its parent — with two
// depth-1 rows the indentation points at the wrong one — so a deeper
// annotation names the node it relates to. The claim becomes checkable
// instead of a relation to a node the reader cannot identify (review of the
// widening fix: both available readings of the bare form were false).
func TestRdepsDeepRowsNameTheirParent(t *testing.T) {
	scratch(t)
	root := addNode(t, "the root", "spec")
	a := addNode(t, "A child of root", "task")
	x := addNode(t, "X both kinds", "task")
	b := addNode(t, "B waits on A", "task")
	session(t, "link", a, "part-of", root)
	session(t, "link", x, "depends-on", root)
	session(t, "link", x, "part-of", root)
	session(t, "link", b, "depends-on", a)

	_, out, _ := session(t, "rdeps", root, "--via", "part-of,depends-on")
	if !strings.Contains(out, "B waits on A  (depends on "+a+")") {
		t.Errorf("the deep row does not name its parent:\n%s", out)
	}
	// Depth-1 rows relate to the queried node and stay bare of a parent id.
	if strings.Contains(out, "A child of root  (part of "+root+")") {
		t.Errorf("a depth-1 row grew a parent id:\n%s", out)
	}
}

// When two kinds both connect a row to its parent, the row names both — one
// alone would be asserted as *the* relation — and the answer cannot vary with
// the order the caller typed --via.
func TestRdepsNamesEveryRelationToTheParent(t *testing.T) {
	scratch(t)
	root := addNode(t, "the root", "spec")
	x := addNode(t, "X both kinds", "task")
	session(t, "link", x, "depends-on", root)
	session(t, "link", x, "part-of", root)

	_, first, _ := session(t, "rdeps", root, "--via", "depends-on,part-of")
	if !strings.Contains(first, "(depends on, part of)") {
		t.Errorf("row does not carry both relations:\n%s", first)
	}
	_, second, _ := session(t, "rdeps", root, "--via", "part-of,depends-on")
	if !strings.Contains(second, "(depends on, part of)") {
		t.Errorf("row content varies with --via order:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// Annotation keys on what the walk crossed, not on what --via requested: a
// wide --via over a store whose edges are all depends-on is still the pure
// closure, and labelling it would say the command's own name on every row.
func TestRdepsWideViaOverAPureGraphStaysBare(t *testing.T) {
	scratch(t)
	root := addNode(t, "the root", "spec")
	dep := addNode(t, "A dep", "task")
	session(t, "link", dep, "depends-on", root)

	_, out, _ := session(t, "rdeps", root, "--via", "depends-on,part-of")
	if strings.Contains(out, "(depends on)") {
		t.Errorf("a walk that crossed only depends-on grew annotations:\n%s", out)
	}
	// And a duplicated --via is still the pure walk — parseVia dedupes, and
	// this is what notices if that ever relaxes.
	_, dup, _ := session(t, "rdeps", root, "--via", "depends-on,depends-on")
	if strings.Contains(dup, "(depends on)") {
		t.Errorf("--via depends-on,depends-on annotated the pure walk:\n%s", dup)
	}
}
