package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mparrett/weft/internal/render"
)

// A node commonly wants two identities: a code anchor that binds it to reality,
// and a stable synthetic key that survives the file moving. One --ref meant
// choosing, permanently — refs are set at add time, and drop-and-re-add loses
// the node's edges.
func TestAddBindsSeveralRefs(t *testing.T) {
	scratch(t)
	code, stdout, stderr := session(t, "add", "Frame.Run: the VM entry point",
		"--kind", "spec",
		"--ref", "file:pkg/vm/vm.go#L827",
		"--ref", "lgx:vm.framerun")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	id := strings.Fields(stdout)[0]

	_, shown, _ := session(t, "show", id)
	for _, ref := range []string{"file:pkg/vm/vm.go#L827", "lgx:vm.framerun"} {
		if !strings.Contains(shown, ref) {
			t.Errorf("show omits %q:\n%s", ref, shown)
		}
	}

	// A ref that does not resolve is a ref that was not really bound.
	for _, ref := range []string{"file:pkg/vm/vm.go#L827", "lgx:vm.framerun"} {
		code, out, stderr := session(t, "show", ref)
		if code != exitOK {
			t.Errorf("show %s: exit %d, %s", ref, code, stderr)
			continue
		}
		if !strings.HasPrefix(out, id) {
			t.Errorf("show %s resolved to something else:\n%s", ref, out)
		}
	}
}

// Repeating a ref is tolerated rather than rejected (SPEC.md §9.3), but writing
// it twice would put two byte-identical ref.add records in the log. They hash to
// one id, so the fold discards the second as a crash-retry (EVENTS.md §2.5) and
// the log — which is the contract other tools read — carries a duplicate id that
// no query would ever reveal.
func TestAddDedupesARepeatedRef(t *testing.T) {
	dir := scratch(t)
	code, stdout, stderr := session(t, "add", "duplicated binding", "--kind", "note",
		"--ref", "same:one", "--ref", "same:one", "--ref", "same:two")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	id := strings.Fields(stdout)[0]

	if got := strings.Count(logText(t, dir), `"ev":"ref.add"`); got != 2 {
		t.Errorf("%d ref.add events in the log, want 2", got)
	}
	_, shown, _ := session(t, "show", id)
	if got := strings.Count(shown, "same:one"); got != 1 {
		t.Errorf("show lists same:one %d times, want 1:\n%s", got, shown)
	}
}

// Refs and their node commit together, so a crash cannot leave a node whose
// bindings were lost. Contiguity in the log is the observable form of that.
func TestAddWritesEveryRefInOneTransaction(t *testing.T) {
	dir := scratch(t)
	if code, _, stderr := session(t, "add", "atomic", "--kind", "task",
		"--ref", "a:one", "--ref", "b:two"); code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(logText(t, dir)), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d log lines, want node.add plus two ref.add", len(lines))
	}
	if !strings.Contains(lines[0], `"ev":"node.add"`) {
		t.Errorf("first line is not the node.add:\n%s", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, `"ev":"ref.add"`) {
			t.Errorf("expected a ref.add:\n%s", l)
		}
	}
}

func TestAddRefsReachJSON(t *testing.T) {
	scratch(t)
	var got struct {
		Nodes []struct {
			Refs []string `json:"refs"`
		} `json:"nodes"`
	}
	_, out, _ := session(t, "add", "json refs", "--kind", "note",
		"--ref", "x:one", "--ref", "y:two", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("add --json is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("expected one node, got %d", len(got.Nodes))
	}
	if len(got.Nodes[0].Refs) != 2 {
		t.Errorf("refs = %v, want both", got.Nodes[0].Refs)
	}
}

// An empty --ref is a mistake worth naming rather than silently dropping: a
// shell expanding an unset variable is the usual way it happens, and binding
// nothing while reporting success hides it.
func TestAddRejectsAnEmptyRef(t *testing.T) {
	scratch(t)
	if code, _, _ := session(t, "add", "empty binding", "--kind", "note", "--ref", ""); code != exitUsage {
		t.Errorf("empty --ref: exit %d, want %d", code, exitUsage)
	}
}

// BUILD.md §1.2: flags may follow positionals. A repeatable flag has to survive
// permutation as many times as it appears, not just once.
func TestAddParsesRepeatedRefsAfterTheTitle(t *testing.T) {
	scratch(t)
	code, stdout, stderr := session(t, "add", "trailing", "--kind", "note",
		"--ref", "p:one", "--ref", "q:two")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	id := strings.Fields(stdout)[0]
	_, shown, _ := session(t, "show", id)
	for _, ref := range []string{"p:one", "q:two"} {
		if !strings.Contains(shown, ref) {
			t.Errorf("show omits %q after permutation:\n%s", ref, shown)
		}
	}
	// The title must not have absorbed the flags.
	if !strings.Contains(shown, "trailing") || strings.Contains(shown, "--ref") {
		t.Errorf("title is wrong after permutation:\n%s", shown)
	}
}

func TestAddIfAbsentReusesLiveNodeWithoutWriting(t *testing.T) {
	dir := scratch(t)
	code, first, stderr := session(t, "add", "retry-safe", "--kind", "bug",
		"--ref", "first:binding", "--if-absent")
	if code != exitOK {
		t.Fatalf("first add: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	code, second, stderr := session(t, "add", "retry-safe", "--kind", "note",
		"--ref", "ignored:binding", "--source", "inferred", "--if-absent")
	if code != exitOK {
		t.Fatalf("retry: exit %d, %s", code, stderr)
	}
	if second != first {
		t.Errorf("retry output differs:\nfirst:  %ssecond: %s", first, second)
	}
	after := logText(t, dir)
	if after != before {
		t.Errorf("retry changed the log:\nbefore:\n%safter:\n%s", before, after)
	}
	if strings.Contains(after, "ignored:binding") {
		t.Error("a retry attached the new ref; refs should be ignored when matching")
	}
}

func TestAddIfAbsentReturnsExistingNodeAsJSON(t *testing.T) {
	scratch(t)
	code, first, stderr := session(t, "add", "json retry", "--ref", "stable:ref", "--json", "--if-absent")
	if code != exitOK {
		t.Fatalf("first add: exit %d, %s", code, stderr)
	}
	code, second, stderr := session(t, "add", "json retry", "--ref", "ignored:ref", "--json", "--if-absent")
	if code != exitOK {
		t.Fatalf("retry: exit %d, %s", code, stderr)
	}
	if second != first {
		t.Errorf("JSON retry output differs:\nfirst:  %ssecond: %s", first, second)
	}
}

func TestAddIfAbsentMatchesOnlyExactTitleInTheSameNamespace(t *testing.T) {
	dir := scratch(t)
	for _, args := range [][]string{
		{"add", "same title", "--if-absent"},
		{"add", "same title", "--ns", "other/repo", "--if-absent"},
		{"add", "Same Title", "--if-absent"},
		{"add", "same title"},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	if got := strings.Count(logText(t, dir), `"ev":"node.add"`); got != 4 {
		t.Errorf("got %d nodes, want 4; namespace and case differ, and plain add must still duplicate", got)
	}
}

func TestAddIfAbsentReusesTheEarliestMatchingNode(t *testing.T) {
	dir := scratch(t)
	_, first, _ := session(t, "add", "preexisting duplicate")
	session(t, "add", "preexisting duplicate")
	before := logText(t, dir)

	code, got, stderr := session(t, "add", "preexisting duplicate", "--if-absent")
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	if got != first {
		t.Errorf("reused later duplicate instead of earliest:\nwant: %sgot:  %s", first, got)
	}
	if after := logText(t, dir); after != before {
		t.Error("matching an existing duplicate changed the log")
	}
}

func TestAddIfAbsentRecreatesDroppedNode(t *testing.T) {
	dir := scratch(t)
	_, first, _ := session(t, "add", "needed again", "--if-absent")
	id := strings.Fields(first)[0]
	if code, _, stderr := session(t, "drop", id); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	code, second, stderr := session(t, "add", "needed again", "--if-absent")
	if code != exitOK {
		t.Fatalf("re-add: exit %d, %s", code, stderr)
	}
	if second == first {
		t.Errorf("dropped node was reused instead of recreated: %s", second)
	}
	if got := strings.Count(logText(t, dir), `"ev":"node.add"`); got != 2 {
		t.Errorf("got %d node.add events, want 2", got)
	}
}

// Done is still present: a retried script that already finished the work must
// not invent a second open copy. Dropped is the state that means absent.
func TestAddIfAbsentReusesDoneNode(t *testing.T) {
	dir := scratch(t)
	code, first, stderr := session(t, "add", "already finished", "--if-absent")
	if code != exitOK {
		t.Fatalf("first add: exit %d, %s", code, stderr)
	}
	id := strings.Fields(first)[0]
	if code, _, stderr := session(t, "done", id); code != exitOK {
		t.Fatalf("done: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	code, second, stderr := session(t, "add", "already finished", "--if-absent")
	if code != exitOK {
		t.Fatalf("retry: exit %d, %s", code, stderr)
	}
	if second != first {
		t.Errorf("done node was not reused:\nfirst:  %ssecond: %s", first, second)
	}
	if after := logText(t, dir); after != before {
		t.Errorf("retry of a done node changed the log:\nbefore:\n%safter:\n%s", before, after)
	}
	if got := strings.Count(logText(t, dir), `"ev":"node.add"`); got != 1 {
		t.Errorf("got %d node.add events, want 1", got)
	}
}

func TestConcurrentAddIfAbsentCreatesOneNode(t *testing.T) {
	dir := scratch(t)
	const writers = 12
	var wg sync.WaitGroup
	ids := make(chan string, writers)
	errs := make(chan string, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, stdout, stderr := session(t, "add", "one assertion", "--if-absent")
			if code != exitOK {
				errs <- stderr
				return
			}
			ids <- strings.Fields(stdout)[0]
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent add failed: %s", err)
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		} else if id != want {
			t.Errorf("concurrent retry returned %s, want %s", id, want)
		}
	}
	if got := strings.Count(logText(t, dir), `"ev":"node.add"`); got != 1 {
		t.Errorf("concurrent retries wrote %d nodes, want 1", got)
	}
}

// corruptLog prepends an unparseable line, the shape a log takes when its first
// bytes are lost. Line 1 rather than the tail because a truncated final record
// is tolerated by design, and this must not be.
func corruptLog(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, append([]byte("garbage\n"), logText(t, dir)...), 0o644); err != nil {
		t.Fatalf("corrupt log: %v", err)
	}
}

// repairLog drops the line corruptLog added.
func repairLog(t *testing.T, dir string) {
	t.Helper()
	_, rest, _ := strings.Cut(logText(t, dir), "\n")
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(rest), 0o644); err != nil {
		t.Fatalf("repair log: %v", err)
	}
}

// The exit code has to match what landed. Exit 6 means the store could not be
// read or written, so a caller that gets it retries — and plain add asserts a
// new node every time, making two nodes for one piece of work with no error
// anywhere. Before the fix add appended the node and then exited 6 from a fold
// whose only job was picking the id display width.
func TestAddOnACorruptLogReportsTheWriteThatLanded(t *testing.T) {
	dir := scratch(t)
	addNode(t, "written before corruption", "task")
	corruptLog(t, dir)
	before := logLines(t, dir)

	code, stdout, stderr := session(t, "add", "written during corruption", "--kind", "task")
	if code != exitOK {
		t.Fatalf("exit %d, want %d: %s", code, exitOK, stderr)
	}
	if logLines(t, dir) != before+1 {
		t.Fatalf("log went from %d to %d lines, want one more", before, logLines(t, dir))
	}
	if !strings.Contains(stdout, "written during corruption") {
		t.Errorf("the write was not reported:\n%s", stdout)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Errorf("a store that cannot be read should still be reported on stderr, got %q", stderr)
	}

	// Printed whole, not at MinWidth. There is no id set to be distinct from
	// when the log cannot be read, and a prefix guessed against a log nothing
	// can parse is exactly what the caller pastes into `show` after repairing.
	id := strings.Fields(stdout)[0]
	if len(id) != render.FullWidth {
		t.Errorf("id printed %d chars wide, want the whole %d: %q", len(id), render.FullWidth, id)
	}
	repairLog(t, dir)
	code, showed, stderr := session(t, "show", id)
	if code != exitOK {
		t.Fatalf("show %s: exit %d: %s", id, code, stderr)
	}
	if !strings.Contains(showed, "written during corruption") {
		t.Errorf("show %s returned the wrong node:\n%s", id, showed)
	}
}

// The --json path reported the same wrong 6, and worse: it printed an error
// object instead of the node, so the caller could not even name what it had
// just created.
func TestAddJSONOnACorruptLogReportsTheWriteThatLanded(t *testing.T) {
	dir := scratch(t)
	addNode(t, "written before corruption", "task")
	corruptLog(t, dir)
	before := logLines(t, dir)

	code, stdout, stderr := session(t, "add", "written during corruption", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, want %d: %s", code, exitOK, stderr)
	}
	if logLines(t, dir) != before+1 {
		t.Fatalf("log went from %d to %d lines, want one more", before, logLines(t, dir))
	}
	var out struct {
		Nodes []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json output was not a node list: %v\n%s", err, stdout)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].Title != "written during corruption" || out.Nodes[0].ID == "" {
		t.Fatalf("unexpected --json output:\n%s", stdout)
	}
	// Silence pins the decision, not an oversight: --json never folds, because
	// the ids it emits are whole and it has no width to compute. Warning here
	// would mean a full read on the path bulk writers use, which is the cost
	// this fix exists to avoid paying. The next read command reports the
	// corruption for real.
	if stderr != "" {
		t.Errorf("--json should not fold to warn, got stderr %q", stderr)
	}
}

// brokenWriter is a stdout that cannot be written to, e.g. a full disk under
// `weft add > out`.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// The fold is not the only post-commit step that can fail. Printing can too,
// and it is the same class: the transaction has committed, so reporting the
// write as lost still costs a duplicate on the retry. There is also nothing
// left to report a failure *to* by then, which is why this degrades rather than
// escalating.
func TestAddSurvivesAFailingStdout(t *testing.T) {
	for _, args := range [][]string{
		{"add", "text path", "--kind", "task"},
		{"add", "json path", "--json"},
	} {
		dir := scratch(t)
		var stderr strings.Builder
		code := run(args, brokenWriter{}, &stderr)
		if code != exitOK {
			t.Errorf("%v: exit %d, want %d", args, code, exitOK)
		}
		if logLines(t, dir) != 1 {
			t.Errorf("%v: %d events landed, want 1", args, logLines(t, dir))
		}
		if !strings.Contains(stderr.String(), "the node was written") {
			t.Errorf("%v: stderr did not report the write: %q", args, stderr.String())
		}
	}
}

// --if-absent reuse returns a node without writing one, so the degraded report
// must not claim a write that did not happen.
func TestAddIfAbsentDegradesWithoutClaimingAWrite(t *testing.T) {
	dir := scratch(t)
	if code, _, stderr := session(t, "add", "reused", "--if-absent"); code != exitOK {
		t.Fatalf("first add: exit %d, %s", code, stderr)
	}
	before := logLines(t, dir)

	var stderr strings.Builder
	if code := run([]string{"add", "reused", "--if-absent"}, brokenWriter{}, &stderr); code != exitOK {
		t.Fatalf("exit %d, want %d", code, exitOK)
	}
	if logLines(t, dir) != before {
		t.Errorf("--if-absent reuse wrote %d events, want 0", logLines(t, dir)-before)
	}
	if !strings.Contains(stderr.String(), "nothing was written") {
		t.Errorf("the warning claims a write that did not happen: %q", stderr.String())
	}
}

// The general invariant, pinned across the mutating commands: a command that
// reports failure must not have written. add is the only one that could reach
// the post-commit fold on a corrupt log — everything else resolves its
// arguments inside Mutate, which folds before the append — but the shape is
// what rots, so assert it rather than assert add alone.
func TestNoMutatingCommandReportsFailureForAWriteThatLanded(t *testing.T) {
	// want is asserted as well as the invariant, because the invariant alone
	// passes vacuously: rename a subcommand later and the subtest exits 1 having
	// written nothing, still "proving" a command it never ran.
	cases := []struct {
		name string
		want int
		args func(a, b string) []string
	}{
		{"add", exitOK, func(string, string) []string { return []string{"add", "new node", "--kind", "task"} }},
		{"add --if-absent", exitStore, func(string, string) []string { return []string{"add", "new node", "--if-absent"} }},
		{"link", exitStore, func(a, b string) []string { return []string{"link", a, "depends-on", b} }},
		{"done", exitStore, func(a, _ string) []string { return []string{"done", a} }},
		{"claim", exitStore, func(a, _ string) []string { return []string{"claim", a} }},
		{"ref add", exitStore, func(a, _ string) []string { return []string{"ref", "add", a, "gh:acme/api#1"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratch(t)
			a := addNode(t, "first", "task")
			b := addNode(t, "second", "task")
			corruptLog(t, dir)
			before := logLines(t, dir)

			code, _, _ := session(t, tc.args(a, b)...)
			appended := logLines(t, dir) - before
			if code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
			if code != exitOK && appended != 0 {
				t.Errorf("exit %d but %d events landed; a caller told to stop will retry a write it already made", code, appended)
			}
		})
	}
}

// The workstream-continuity tool writes this kind and filters on it. A literal
// rather than validKinds, so a kind list that stops deriving from validKinds
// fails here instead of dropping the kind silently.
func TestWorkstreamKindRoundTrips(t *testing.T) {
	scratch(t)
	code, stdout, stderr := session(t, "add", "continuity", "--kind", "workstream", "--ref", "claude:abc")
	if code != exitOK {
		t.Fatalf("add --kind workstream: exit %d, %s", code, stderr)
	}
	id := strings.Fields(stdout)[0]
	session(t, "add", "unrelated", "--kind", "task")

	code, listed, stderr := session(t, "ls", "--kind", "workstream")
	if code != exitOK {
		t.Fatalf("ls --kind workstream: exit %d, %s", code, stderr)
	}
	if !strings.Contains(listed, id) || strings.Contains(listed, "unrelated") {
		t.Errorf("ls --kind workstream should list only the workstream:\n%s", listed)
	}
}

// A workstream is a container, not work: it must never be offered from the
// frontier, whether it would read as ready or as blocked.
func TestWorkstreamsStayOffTheFrontier(t *testing.T) {
	scratch(t)
	_, out, _ := session(t, "add", "idle stream", "--kind", "workstream")
	idle := strings.Fields(out)[0]
	_, out, _ = session(t, "add", "waiting stream", "--kind", "workstream")
	waiting := strings.Fields(out)[0]
	_, out, _ = session(t, "add", "real work", "--kind", "task")
	work := strings.Fields(out)[0]
	if code, _, stderr := session(t, "link", waiting, "depends-on", work); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}

	_, ready, _ := session(t, "ls", "--ready")
	if !strings.Contains(ready, work) || strings.Contains(ready, idle) {
		t.Errorf("ls --ready should list the task and skip the workstream:\n%s", ready)
	}
	_, blocked, _ := session(t, "ls", "--blocked")
	if strings.Contains(blocked, waiting) {
		t.Errorf("ls --blocked should skip a workstream even with an open dependency:\n%s", blocked)
	}
	_, all, _ := session(t, "ls")
	if !strings.Contains(all, idle) || !strings.Contains(all, waiting) {
		t.Errorf("plain ls should still list workstreams:\n%s", all)
	}
}

// The state right after the continuity tool writes its first stream: the
// empty-frontier hint must not point at a blocker that does not exist.
func TestFrontierHintForAWorkstreamOnlyNamespace(t *testing.T) {
	scratch(t)
	session(t, "add", "first stream", "--kind", "workstream")
	for _, flag := range []string{"--ready", "--blocked"} {
		_, _, stderr := session(t, "ls", flag)
		if !strings.Contains(stderr, "only workstreams") || strings.Contains(stderr, "why <id>") {
			t.Errorf("ls %s hint should name the workstream-only state:\n%s", flag, stderr)
		}
	}
}
