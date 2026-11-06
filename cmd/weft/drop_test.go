package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The retry-after-drop bug: the documented retry pattern is `add --if-absent` plus refs, and
// one `drop` used to break it permanently — the re-add bound the ref to a
// second node, and every later command naming the ref exited 4. These tests
// pin the repaired contract: ambiguity in a ref counts live nodes only, and
// drop has a form a re-runnable writer can use.

func TestARefResolvesToTheLiveNodeAfterDropAndReadd(t *testing.T) {
	scratch(t)

	// The issue's exact sequence: assert, retract, re-run the same script line.
	addNodeFull(t, "a claim", "note", "--ref", "key:claim1", "--if-absent")
	if code, _, stderr := session(t, "drop", "key:claim1"); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	readd := addNodeFull(t, "a claim", "note", "--ref", "key:claim1", "--if-absent")

	// The ref now names two nodes, one dropped. It must resolve to the live one,
	// not report ambiguity — that is what let one drop poison a whole script.
	code, stdout, stderr := session(t, "show", "key:claim1")
	if code != exitOK {
		t.Fatalf("show key:claim1: exit %d, %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, readd) {
		t.Errorf("show resolved to something other than the live node %s:\n%s", readd, stdout)
	}
	if strings.Contains(stdout, "state  dropped") {
		t.Errorf("show resolved to the dropped node:\n%s", stdout)
	}

	// The later lines of the same script — links naming the ref — must work too.
	other := addNode(t, "downstream", "task")
	if code, _, stderr := session(t, "link", other, "depends-on", "key:claim1"); code != exitOK {
		t.Errorf("link by ref: exit %d, %s", code, stderr)
	}
}

func TestARefCarriedOnlyByDroppedNodesStillResolves(t *testing.T) {
	scratch(t)
	addNodeFull(t, "rejected idea", "note", "--ref", "plan:undo")
	if code, _, stderr := session(t, "drop", "plan:undo"); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	// Dropped is a state, not an erasure: with no live holder the ref still
	// names the dropped node, so "why was this rejected" stays answerable.
	code, stdout, stderr := session(t, "show", "plan:undo")
	if code != exitOK {
		t.Fatalf("show: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "dropped") {
		t.Errorf("show did not surface the dropped state:\n%s", stdout)
	}
}

func TestARefSharedByTwoLiveNodesIsStillAmbiguous(t *testing.T) {
	scratch(t)
	addNodeFull(t, "first take", "note", "--ref", "key:shared")
	addNodeFull(t, "second take", "note", "--ref", "key:shared")
	if code, _, _ := session(t, "show", "key:shared"); code != exitAmbiguous {
		t.Errorf("two live holders: exit %d, want %d", code, exitAmbiguous)
	}
}

// A prefix is an id, not a name: a dropped node must stay addressable by it,
// so preferring live nodes there would make prefix resolution lossy. Only the
// ref path discounts dropped nodes.
func TestAPrefixCollisionIsNotHiddenByDrop(t *testing.T) {
	scratch(t)

	// Add nodes until two ids share a first hex character — at most 17 by
	// pigeonhole — then drop one of the pair.
	byFirst := make(map[byte]string)
	var prefix string
	var droppable string
	for i := 0; i < 20; i++ {
		title := "filler " + string(rune('a'+i))
		code, stdout, stderr := session(t, "add", title, "--json")
		if code != exitOK {
			t.Fatalf("add: exit %d, %s", code, stderr)
		}
		var out struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatal(err)
		}
		id := out.Nodes[0].ID
		if earlier, ok := byFirst[id[0]]; ok {
			prefix, droppable = string(id[0]), earlier
			break
		}
		byFirst[id[0]] = id
	}
	if prefix == "" {
		t.Fatal("no two of 20 ids shared a first hex character")
	}
	if code, _, stderr := session(t, "drop", droppable); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	if code, _, _ := session(t, "show", prefix); code != exitAmbiguous {
		t.Errorf("prefix matching a live and a dropped node: exit %d, want %d", code, exitAmbiguous)
	}
}

func TestDropIfPresentAppendsNothingWhenAlreadyDropped(t *testing.T) {
	dir := scratch(t)
	addNodeFull(t, "retracted", "note", "--ref", "key:r")
	if code, _, stderr := session(t, "drop", "key:r"); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	code, stdout, stderr := session(t, "drop", "key:r", "--if-present")
	if code != exitOK {
		t.Fatalf("drop --if-present: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "already dropped") {
		t.Errorf("no-op drop did not say so:\n%s", stdout)
	}
	if after := logText(t, dir); after != before {
		t.Errorf("drop --if-present grew the log:\n%s", after[len(before):])
	}

	// --json carries the no-op explicitly, the way unlink and release do.
	_, stdout, _ = session(t, "drop", "key:r", "--if-present", "--json")
	if !strings.Contains(stdout, `"dropped": false`) {
		t.Errorf("--json did not report the no-op:\n%s", stdout)
	}
}

func TestDropIfPresentToleratesANodeThatNeverExisted(t *testing.T) {
	dir := scratch(t)

	if code, _, _ := session(t, "drop", "key:ghost"); code != exitNotFound {
		t.Errorf("plain drop of a missing node: exit %d, want %d", code, exitNotFound)
	}
	code, stdout, stderr := session(t, "drop", "key:ghost", "--if-present")
	if code != exitOK {
		t.Fatalf("drop --if-present: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "no node matches") {
		t.Errorf("no-op drop did not say why:\n%s", stdout)
	}
	// The lock path opens the log with O_CREATE, so an empty file may exist
	// even after a pure no-op — what must not exist is a byte in it.
	if got, err := os.ReadFile(filepath.Join(dir, "log.jsonl")); err == nil && len(got) > 0 {
		t.Errorf("drop --if-present wrote to an empty store:\n%s", got)
	}
}

func TestAddIfAbsentEvenDroppedReusesTheDroppedNode(t *testing.T) {
	dir := scratch(t)
	id := addNodeFull(t, "rejected idea", "note", "--ref", "plan:x", "--if-absent")
	if code, _, stderr := session(t, "drop", "plan:x"); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	got := addNodeFull(t, "rejected idea", "note", "--ref", "plan:x", "--if-absent", "--even-dropped")
	if got != id {
		t.Errorf("reused %s, want the dropped node %s", got, id)
	}
	if after := logText(t, dir); after != before {
		t.Errorf("--even-dropped reuse grew the log:\n%s", after[len(before):])
	}
}

func TestEvenDroppedWithoutIfAbsentIsAUsageError(t *testing.T) {
	scratch(t)
	if code, _, _ := session(t, "add", "x", "--even-dropped"); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
}

func TestEvenDroppedPrefersTheLiveNode(t *testing.T) {
	scratch(t)
	first := addNodeFull(t, "the work", "task")
	if code, _, stderr := session(t, "drop", first); code != exitOK {
		t.Fatalf("drop: exit %d, %s", code, stderr)
	}
	live := addNodeFull(t, "the work", "task") // genuinely re-asserted

	got := addNodeFull(t, "the work", "task", "--if-absent", "--even-dropped")
	if got != live {
		t.Errorf("reused %s, want the live node %s — a live match wins even under --even-dropped", got, live)
	}
}

// The property the whole change exists for: a script that asserts a claim and
// then retracts it — the natural shape for "considered and rejected" — appends
// a byte-identical log on every run after the first (EVENTS.md §3.9).
func TestAnAssertThenDropScriptAppendsNothingOnRerun(t *testing.T) {
	dir := scratch(t)

	script := func() {
		for _, args := range [][]string{
			{"add", "merge --undo: needs replay", "--kind", "note", "--ref", "plan:merge-undo", "--if-absent", "--even-dropped"},
			{"drop", "plan:merge-undo", "--if-present"},
		} {
			if code, _, stderr := session(t, args...); code != exitOK {
				t.Fatalf("%v: exit %d, %s", args, code, stderr)
			}
		}
	}

	script()
	first := logText(t, dir)
	if first == "" {
		t.Fatal("first run wrote nothing")
	}
	script()
	if second := logText(t, dir); second != first {
		t.Errorf("re-run grew the log:\n%s", second[len(first):])
	}
}

// The boundary of "retraction never poisons a name": two dropped holders with
// no live one are still ambiguous — there is no live node to prefer, and
// choosing among retracted claims is merge's judgment. merge is the cure.
func TestTwoDroppedHoldersStayAmbiguousUntilMerged(t *testing.T) {
	scratch(t)
	first := addNodeFull(t, "a claim", "note", "--ref", "key:c")
	if code, _, _ := session(t, "drop", first); code != exitOK {
		t.Fatal("drop failed")
	}
	second := addNodeFull(t, "a claim", "note", "--ref", "key:c")
	if code, _, _ := session(t, "drop", second); code != exitOK {
		t.Fatal("drop failed")
	}

	if code, _, _ := session(t, "show", "key:c"); code != exitAmbiguous {
		t.Errorf("two dropped holders: exit %d, want %d", code, exitAmbiguous)
	}
	// --if-present does not paper over ambiguity — the writer named something.
	if code, _, _ := session(t, "drop", "key:c", "--if-present"); code != exitAmbiguous {
		t.Errorf("drop --if-present on an ambiguous ref: want exit %d", exitAmbiguous)
	}
	if code, _, stderr := session(t, "merge", first, second); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	if code, _, _ := session(t, "show", "key:c"); code != exitOK {
		t.Errorf("merge did not cure the ambiguity")
	}
}

func TestDoneIfPresentAppendsNothingWhenAlreadyDone(t *testing.T) {
	dir := scratch(t)
	id := addNode(t, "shipped", "task")
	if code, _, _ := session(t, "done", id); code != exitOK {
		t.Fatal("done failed")
	}
	before := logText(t, dir)

	code, stdout, stderr := session(t, "done", id, "--if-present")
	if code != exitOK {
		t.Fatalf("done --if-present: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "already done") {
		t.Errorf("no-op done did not say so:\n%s", stdout)
	}
	if after := logText(t, dir); after != before {
		t.Errorf("done --if-present grew the log")
	}
}

// --if-present reads as "act only if present", so under --undo it means
// reopen-if-present: a missing node is a no-op, a dropped one reopens.
func TestUndoIfPresentReopensOnlyWhatExists(t *testing.T) {
	scratch(t)
	if code, _, _ := session(t, "drop", "key:ghost", "--undo", "--if-present"); code != exitOK {
		t.Errorf("reopen-if-present of a missing node should be exit 0")
	}
	id := addNode(t, "retracted", "note")
	session(t, "drop", id)
	if code, stdout, _ := session(t, "drop", id, "--undo", "--if-present"); code != exitOK || !strings.Contains(stdout, "open") {
		t.Errorf("reopen-if-present of a dropped node should reopen it:\n%s", stdout)
	}
}

// An empty query is the unset shell variable — a caller bug, not a node that
// happens to be absent. It must not become a silent exit-0 under --if-present.
func TestStateCommandsRejectAnEmptyQuery(t *testing.T) {
	scratch(t)
	for _, args := range [][]string{
		{"drop", ""},
		{"drop", "", "--if-present"},
		{"done", "", "--if-present"},
	} {
		if code, _, _ := session(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// The changed field is emitted whether or not --if-present was passed, so the
// output schema does not vary with a flag the consumer cannot see.
func TestStateJSONAlwaysCarriesTheChangedField(t *testing.T) {
	scratch(t)
	id := addNode(t, "work", "task")
	_, stdout, _ := session(t, "drop", id, "--json")
	if !strings.Contains(stdout, `"dropped": true`) {
		t.Errorf("plain drop --json lost the changed field:\n%s", stdout)
	}
	_, stdout, _ = session(t, "drop", id, "--if-present", "--json")
	if !strings.Contains(stdout, `"dropped": false`) {
		t.Errorf("no-op drop --json should report false:\n%s", stdout)
	}
	id2 := addNode(t, "more work", "task")
	_, stdout, _ = session(t, "drop", id2, "--if-present", "--json")
	if !strings.Contains(stdout, `"dropped": true`) {
		t.Errorf("a write under --if-present should report true:\n%s", stdout)
	}
}

// --even-dropped's audience is scripts that address by ref, so a reuse binds
// any --ref the node lacks — otherwise a same-titled node that never carried
// the ref absorbs the assertion and every later line naming the ref fails at
// the addressing layer, the same symptom as re-creating a
// dropped node.
func TestEvenDroppedReuseBindsTheMissingRef(t *testing.T) {
	dir := scratch(t)
	id := addNode(t, "merge --undo: needs replay", "note") // no ref
	if code, _, _ := session(t, "drop", id); code != exitOK {
		t.Fatal("drop failed")
	}

	got := addNodeFull(t, "merge --undo: needs replay", "note",
		"--ref", "plan:merge-undo", "--if-absent", "--even-dropped")
	if !strings.HasPrefix(id, got) && !strings.HasPrefix(got, id) {
		t.Fatalf("reused %s, want %s", got, id)
	}
	if code, _, _ := session(t, "show", "plan:merge-undo"); code != exitOK {
		t.Errorf("the reuse did not bind the ref")
	}
	if code, _, _ := session(t, "drop", "plan:merge-undo", "--if-present"); code != exitOK {
		t.Errorf("the bound ref should be droppable")
	}

	// Once the binding lands, re-runs are byte-identical again.
	before := logText(t, dir)
	addNodeFull(t, "merge --undo: needs replay", "note",
		"--ref", "plan:merge-undo", "--if-absent", "--even-dropped")
	if code, _, _ := session(t, "drop", "plan:merge-undo", "--if-present"); code != exitOK {
		t.Fatal("drop --if-present failed")
	}
	if after := logText(t, dir); after != before {
		t.Errorf("converged script still grew the log:\n%s", after[len(before):])
	}
}

// A reuse of a non-open node says so in the confirmation — a dropped node
// handed back in the same shape as a fresh write is one every readiness query
// discounts, with nothing telling the writer.
func TestReusingADroppedNodeSaysSo(t *testing.T) {
	scratch(t)
	id := addNode(t, "rejected idea", "note")
	session(t, "drop", id)

	_, stdout, _ := session(t, "add", "rejected idea", "--kind", "note", "--if-absent", "--even-dropped")
	if !strings.Contains(stdout, "(dropped)") {
		t.Errorf("reuse of a dropped node is indistinguishable from a fresh add:\n%s", stdout)
	}
}
