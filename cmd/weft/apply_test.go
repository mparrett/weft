package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// applyIn runs `weft apply` with body on stdin and decodes the reply.
func applyIn(t *testing.T, body any, args ...string) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runWithStdin(append([]string{"apply"}, args...), bytes.NewReader(raw), &stdout, &stderr)
	var reply map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("apply reply is not JSON (exit %d): %v\nstdout: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	return code, reply
}

func readLog(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

// --- the shared fixtures ----------------------------------------------------

// The fixtures in testdata/apply are the contract both sides test against: weft
// here, and the workstream tool against the same files. Changing one changes
// the interface, so it is a change to make on purpose, in both places.
type applyFixture struct {
	Name    string         `json:"name"`
	Setup   map[string]any `json:"setup"`
	Args    []string       `json:"args"`
	Request any            `json:"request"`
	Repeat  bool           `json:"repeat"`
	Exit    int            `json:"exit"`
	Reply   any            `json:"reply"`
}

var (
	hexID     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revForm   = regexp.MustCompile(`^[0-9]+:[0-9a-f]{0,12}$`)
	setupName = regexp.MustCompile(`^<([A-Z][A-Za-z0-9]*)>$`)
	setupRes  = regexp.MustCompile(`^<setup\.([0-9]+)\.(claim|node|edge)>$`)
	tmpName   = regexp.MustCompile(`^<tmp\.(\$[A-Za-z0-9_]+)>$`)
)

// bindSetup replaces the placeholders that name what the setup created.
func bindSetup(t *testing.T, v any, setup map[string]any) any {
	switch x := v.(type) {
	case string:
		if m := setupName.FindStringSubmatch(x); m != nil {
			tmp, _ := setup["tmp"].(map[string]any)
			id, ok := tmp["$"+m[1]].(string)
			if !ok {
				t.Fatalf("placeholder %s: setup defined no $%s", x, m[1])
			}
			return id
		}
		if m := setupRes.FindStringSubmatch(x); m != nil {
			results, _ := setup["results"].([]any)
			var n int
			fmt.Sscan(m[1], &n)
			if n >= len(results) {
				t.Fatalf("placeholder %s: setup has %d results", x, len(results))
			}
			id, ok := results[n].(map[string]any)[m[2]].(string)
			if !ok {
				t.Fatalf("placeholder %s: setup result %d has no %s", x, n, m[2])
			}
			return id
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = bindSetup(t, x[i], setup)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k := range x {
			out[k] = bindSetup(t, x[k], setup)
		}
		return out
	}
	return v
}

// matchReply compares structurally, with wildcards for values no fixture can
// pin: fresh ids, revs, timestamps, generated keys.
func matchReply(path string, want, got any, reply map[string]any) []string {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		if !ok {
			return []string{fmt.Sprintf("%s: want string %q, got %v", path, w, got)}
		}
		switch {
		case w == "<id>" && hexID.MatchString(g),
			w == "<rev>" && revForm.MatchString(g),
			w == "<key>" && g != "":
			return nil
		case w == "<at>":
			if _, err := time.Parse(time.RFC3339Nano, g); err == nil {
				return nil
			}
		}
		if m := tmpName.FindStringSubmatch(w); m != nil {
			tmp, _ := reply["tmp"].(map[string]any)
			if id, _ := tmp[m[1]].(string); id != "" && id == g {
				return nil
			}
		}
		if w == g {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %q, got %q", path, w, g)}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %d items, got %v", path, len(w), got)}
		}
		var errs []string
		for i := range w {
			errs = append(errs, matchReply(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], reply)...)
		}
		return errs
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object, got %v", path, got)}
		}
		var errs []string
		for k := range w {
			if _, ok := g[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: missing", path, k))
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: unexpected (%v)", path, k, g[k]))
				continue
			}
			errs = append(errs, matchReply(path+"."+k, w[k], g[k], reply)...)
		}
		return errs
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) {
			return []string{fmt.Sprintf("%s: want %v, got %v", path, want, got)}
		}
	}
	return nil
}

func TestApplyFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/apply/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no apply fixtures: %v", err)
	}
	for _, p := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(p), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var fx applyFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			dir := scratch(t)

			setup := map[string]any{}
			if ops, _ := fx.Setup["ops"].([]any); len(ops) > 0 {
				code, reply := applyIn(t, fx.Setup)
				if code != exitOK {
					t.Fatalf("setup: exit %d: %v", code, reply)
				}
				setup = reply
			}
			req := bindSetup(t, fx.Request, setup)
			want := bindSetup(t, fx.Reply, setup)

			before := readLog(t, dir)
			code, reply := applyIn(t, req, fx.Args...)
			if fx.Repeat {
				if code != exitOK {
					t.Fatalf("first run: exit %d: %v", code, reply)
				}
				before = readLog(t, dir)
				code, reply = applyIn(t, req, fx.Args...)
			}
			if code != fx.Exit {
				t.Fatalf("exit %d, want %d: %v", code, fx.Exit, reply)
			}
			for _, e := range matchReply("reply", want, reply, reply) {
				t.Error(e)
			}
			// Nothing is written by a refusal, a dry run, or a replay. Compared as
			// bytes, not as a fold: a rejected batch that left a torn prefix
			// would fold identically and still be wrong.
			if fx.Exit != exitOK || fx.Repeat || contains(fx.Args, "--dry-run") {
				if after := readLog(t, dir); !bytes.Equal(before, after) {
					t.Errorf("log changed:\nbefore %d bytes, after %d bytes", len(before), len(after))
				}
			}
		})
	}
}

// --- concurrency ------------------------------------------------------------

func moveReq(holder, from, to string) map[string]any {
	return map[string]any{
		"expect": []any{
			map[string]any{"holder": holder, "live_on": []string{from}, "ns": "workstreams", "kind": "workstream"},
			map[string]any{"node": to, "state": "open"},
		},
		"ops": []any{
			map[string]any{"op": "release", "node": from, "holder": holder},
			map[string]any{"op": "claim", "node": to, "holder": holder},
		},
	}
}

func liveOn(t *testing.T, holder string) []string {
	t.Helper()
	_, stdout, _ := session(t, "export", "--json", "--include", "claims,live")
	var out struct {
		Nodes []struct {
			ID     string `json:"id"`
			Claims []struct {
				Holder string `json:"holder"`
				Live   bool   `json:"live"`
			} `json:"claims"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	var on []string
	for _, n := range out.Nodes {
		for _, c := range n.Claims {
			if c.Holder == holder && c.Live {
				on = append(on, n.ID)
			}
		}
	}
	return on
}

func setupWorkstreams(t *testing.T, names ...string) map[string]string {
	t.Helper()
	var ops []any
	for _, n := range names {
		ops = append(ops, map[string]any{"op": "add", "as": "$" + n, "title": "ws " + n, "kind": "workstream", "ns": "workstreams"})
	}
	ops = append(ops, map[string]any{"op": "claim", "node": "$" + names[0], "holder": "agent:S1"})
	code, reply := applyIn(t, map[string]any{"ops": ops})
	if code != exitOK {
		t.Fatalf("setup: %v", reply)
	}
	ids := map[string]string{}
	for k, v := range reply["tmp"].(map[string]any) {
		ids[strings.TrimPrefix(k, "$")] = v.(string)
	}
	return ids
}

// Two moves of one holder from A, racing to B and to C. The lock serializes
// them and the second sees the first's result, so exactly one lands and the
// holder ends with exactly one live claim. Repeated because a race that is
// lost once in a while is the bug this exists to catch.
func TestApplyConcurrentMovesLeaveOneClaim(t *testing.T) {
	for round := 0; round < 20; round++ {
		scratch(t)
		ids := setupWorkstreams(t, "A", "B", "C")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for k, to := range []string{ids["B"], ids["C"]} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				raw, _ := json.Marshal(moveReq("agent:S1", ids["A"], to))
				var out, errb bytes.Buffer
				codes[k] = runWithStdin([]string{"apply"}, bytes.NewReader(raw), &out, &errb)
			}()
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case exitOK:
				ok++
			case exitConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: exits %v, want one 0 and one 8", round, codes)
		}
		if on := liveOn(t, "agent:S1"); len(on) != 1 {
			t.Fatalf("round %d: holder is live on %d nodes, want 1", round, len(on))
		}
	}
}

// An attach racing a close: whichever order the lock picks, a closed node must
// never be left holding a live claim. Normal close conflicts on a claim it did
// not see; forced close releases it.
func TestApplyAttachRacingClose(t *testing.T) {
	for _, force := range []bool{false, true} {
		for round := 0; round < 10; round++ {
			scratch(t)
			code, reply := applyIn(t, map[string]any{"ops": []any{
				map[string]any{"op": "add", "as": "$A", "title": "ws", "kind": "workstream", "ns": "workstreams"},
			}})
			if code != exitOK {
				t.Fatal(reply)
			}
			a := reply["tmp"].(map[string]any)["$A"].(string)
			attach := map[string]any{
				"expect": []any{map[string]any{"node": a, "state": "open"}},
				"ops":    []any{map[string]any{"op": "claim", "node": a, "holder": "agent:S2"}},
			}
			closeReq := map[string]any{
				"expect": []any{map[string]any{"node": a, "claims": []string{}}, map[string]any{"node": a, "state": "open"}},
				"ops":    []any{map[string]any{"op": "done", "node": a}},
			}
			if force {
				closeReq = map[string]any{
					"expect": []any{map[string]any{"node": a, "state": "open"}},
					"ops":    []any{map[string]any{"op": "release", "node": a, "all": true}, map[string]any{"op": "done", "node": a}},
				}
			}
			var wg sync.WaitGroup
			codes := make([]int, 2)
			for k, req := range []any{attach, closeReq} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					raw, _ := json.Marshal(req)
					var out, errb bytes.Buffer
					codes[k] = runWithStdin([]string{"apply"}, bytes.NewReader(raw), &out, &errb)
				}()
			}
			wg.Wait()
			for _, c := range codes {
				if c != exitOK && c != exitConflict {
					t.Fatalf("force=%v round %d: exits %v", force, round, codes)
				}
			}
			_, stdout, _ := session(t, "show", a, "--json")
			closed := strings.Contains(stdout, `"state": "done"`)
			live := len(liveOn(t, "agent:S2")) > 0
			if closed && live {
				t.Fatalf("force=%v round %d: closed with a live claim (exits %v)", force, round, codes)
			}
			if force && codes[1] != exitOK {
				t.Fatalf("forced close failed: exits %v", codes)
			}
		}
	}
}

// The generic use: one task, reassigned by two writers at once from the same
// read. One wins; the task ends with exactly one holder.
func TestApplyConcurrentReassignment(t *testing.T) {
	scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$T", "title": "fix the parser"},
		map[string]any{"op": "claim", "node": "$T", "holder": "person:P"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	task := reply["tmp"].(map[string]any)["$T"].(string)
	claim := reply["results"].([]any)[1].(map[string]any)["claim"].(string)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for k, to := range []string{"person:Q", "person:R"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"expect": []any{map[string]any{"node": task, "claims": []string{claim}}},
				"ops": []any{
					map[string]any{"op": "release", "claim": claim},
					map[string]any{"op": "claim", "node": task, "holder": to},
				},
			})
			var out, errb bytes.Buffer
			codes[k] = runWithStdin([]string{"apply"}, bytes.NewReader(raw), &out, &errb)
		}()
	}
	wg.Wait()
	if !(codes[0] == exitOK && codes[1] == exitConflict) && !(codes[0] == exitConflict && codes[1] == exitOK) {
		t.Fatalf("exits %v, want one 0 and one 8", codes)
	}
	holders := len(liveOn(t, "person:Q")) + len(liveOn(t, "person:R")) + len(liveOn(t, "person:P"))
	if holders != 1 {
		t.Fatalf("task has %d live holders, want 1", holders)
	}
}

// --- atomicity --------------------------------------------------------------

// A batch torn after its first complete event — a crash, ENOSPC, power loss —
// must be invisible to readers and gone after the next write. The move here
// would otherwise keep its release and lose its claim.
func TestApplyTornBatchIsNotCommitted(t *testing.T) {
	dir := scratch(t)
	ids := setupWorkstreams(t, "A", "B")
	committed := readLog(t, dir)

	code, reply := applyIn(t, moveReq("agent:S1", ids["A"], ids["B"]))
	if code != exitOK {
		t.Fatal(reply)
	}
	full := readLog(t, dir)
	batch := strings.SplitAfter(string(full[len(committed):]), "\n")
	if len(batch) < 3 { // release, claim, commit, and the empty tail
		t.Fatalf("batch has %d lines", len(batch))
	}
	// Keep only the first complete event: the release landed, the rest did not.
	torn := append(append([]byte{}, committed...), batch[0]...)
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), torn, 0o644); err != nil {
		t.Fatal(err)
	}
	if on := liveOn(t, "agent:S1"); len(on) != 1 || on[0] != ids["A"] {
		t.Fatalf("after a torn move the holder is on %v, want only A", on)
	}

	// The next writer truncates the torn run before appending.
	if code, _, stderr := session(t, "add", "unrelated"); code != exitOK {
		t.Fatal(stderr)
	}
	after := readLog(t, dir)
	if !bytes.HasPrefix(after, committed) || bytes.Contains(after[len(committed):], []byte(batch[0])) {
		t.Fatal("the torn batch survived the next append")
	}
	if n := bytes.Count(after[len(committed):], []byte("\n")); n != 1 {
		t.Fatalf("after recovery, %d lines follow the committed prefix, want 1 (the add)", n)
	}
}

// A dry run and a real apply agree on everything but committed and the ids of
// events that were never written.
func TestApplyKeyReusedForDifferentRequest(t *testing.T) {
	scratch(t)
	first := map[string]any{"key": "k1", "ops": []any{map[string]any{"op": "add", "title": "one"}}}
	if code, reply := applyIn(t, first); code != exitOK {
		t.Fatal(reply)
	}
	other := map[string]any{"key": "k1", "ops": []any{map[string]any{"op": "done", "node": "x"}}}
	if code, reply := applyIn(t, other); code != exitUsage {
		t.Fatalf("exit %d, want 1: %v", code, reply)
	}
}

func TestApplyUsageErrors(t *testing.T) {
	scratch(t)
	for name, req := range map[string]any{
		"no ops":            map[string]any{"ops": []any{}},
		"unknown op":        map[string]any{"ops": []any{map[string]any{"op": "frobnicate"}}},
		"field of another":  map[string]any{"ops": []any{map[string]any{"op": "done", "node": "x", "holder": "h"}}},
		"undefined name":    map[string]any{"ops": []any{map[string]any{"op": "done", "node": "$nope"}}},
		"name in expect":    map[string]any{"expect": []any{map[string]any{"node": "$a", "state": "open"}}, "ops": []any{map[string]any{"op": "add", "as": "$a", "title": "t"}}},
		"bad expectation":   map[string]any{"expect": []any{map[string]any{"node": "x"}}, "ops": []any{map[string]any{"op": "add", "title": "t"}}},
		"children $name":    map[string]any{"expect": []any{map[string]any{"node": "x", "children": []any{"$a"}}}, "ops": []any{map[string]any{"op": "add", "as": "$a", "title": "t"}}},
		"children bad kind": map[string]any{"expect": []any{map[string]any{"node": "x", "children": []any{}, "kind": "parent-of"}}, "ops": []any{map[string]any{"op": "add", "title": "t"}}},
		"children extra":    map[string]any{"expect": []any{map[string]any{"node": "x", "children": []any{}, "ns": "n"}}, "ops": []any{map[string]any{"op": "add", "title": "t"}}},
		"two release forms": map[string]any{"ops": []any{map[string]any{"op": "release", "node": "x", "holder": "h", "all": true}}},
	} {
		if code, reply := applyIn(t, req); code != exitUsage {
			t.Errorf("%s: exit %d, want 1: %v", name, code, reply)
		}
	}
}

func TestApplyOpErrorsNameTheOp(t *testing.T) {
	scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$a", "title": "a"},
		map[string]any{"op": "done", "node": "no-such-node"},
	}})
	if code != exitNotFound || reply["i"] != float64(1) {
		t.Fatalf("exit %d, reply %v; want 2 naming op 1", code, reply)
	}

	code, reply = applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$a", "title": "a"},
		map[string]any{"op": "add", "as": "$b", "title": "b"},
		map[string]any{"op": "link", "from": "$a", "kind": "depends-on", "to": "$b"},
		map[string]any{"op": "link", "from": "$b", "kind": "depends-on", "to": "$a"},
	}})
	if code != exitRejected || reply["i"] != float64(3) {
		t.Fatalf("exit %d, reply %v; want 5 naming op 3", code, reply)
	}
	if _, stdout, _ := session(t, "ls"); strings.TrimSpace(stdout) != "" {
		t.Fatalf("a rejected batch left nodes behind:\n%s", stdout)
	}
}

// Retried ops append nothing: an already-bound ref, an existing edge, a node
// already done, a holder already claiming with unless_held.
func TestApplyRetriedOpsAreNoOps(t *testing.T) {
	dir := scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$a", "title": "a", "refs": []string{"x:1"}},
		map[string]any{"op": "add", "as": "$b", "title": "b"},
		map[string]any{"op": "claim", "node": "$a", "holder": "h"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	tmp := reply["tmp"].(map[string]any)
	a, b := tmp["$a"].(string), tmp["$b"].(string)
	link := map[string]any{"ops": []any{
		map[string]any{"op": "link", "from": a, "kind": "depends-on", "to": b},
		map[string]any{"op": "done", "node": b},
	}}
	if code, reply := applyIn(t, link); code != exitOK {
		t.Fatal(reply)
	}
	before := readLog(t, dir)
	code, reply = applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "ref.add", "node": a, "ref": "x:1"},
		map[string]any{"op": "link", "from": a, "kind": "depends-on", "to": b},
		map[string]any{"op": "done", "node": b},
		map[string]any{"op": "claim", "node": a, "holder": "h", "unless_held": true},
		map[string]any{"op": "release", "node": a, "holder": "nobody"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	for _, r := range reply["results"].([]any) {
		if r.(map[string]any)["changed"] != false {
			t.Errorf("op reported a change: %v", r)
		}
	}
	if !bytes.Equal(before, readLog(t, dir)) {
		t.Error("a batch of no-ops grew the log")
	}
}

// A stacked holder releases fully: two claims by one holder, one release op.
func TestApplyReleaseByHolderReleasesEveryClaim(t *testing.T) {
	scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$a", "title": "a"},
		map[string]any{"op": "claim", "node": "$a", "holder": "h"},
		map[string]any{"op": "claim", "node": "$a", "holder": "h"},
		map[string]any{"op": "claim", "node": "$a", "holder": "other"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	a := reply["tmp"].(map[string]any)["$a"].(string)
	code, reply = applyIn(t, map[string]any{"ops": []any{map[string]any{"op": "release", "node": a, "holder": "h"}}})
	if code != exitOK {
		t.Fatal(reply)
	}
	if got := reply["results"].([]any)[0].(map[string]any)["claims"].([]any); len(got) != 2 {
		t.Fatalf("released %d claims, want 2", len(got))
	}
	if len(liveOn(t, "h")) != 0 || len(liveOn(t, "other")) != 1 {
		t.Fatal("release by holder touched the wrong claims")
	}
}

func TestApplyGlobalRev(t *testing.T) {
	scratch(t)
	_, stdout, _ := session(t, "export", "--json", "--include", "rev")
	var snap struct{ Rev string }
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil || snap.Rev != "0:" {
		t.Fatalf("empty store rev = %q (%v)", snap.Rev, err)
	}
	code, reply := applyIn(t, map[string]any{"rev": snap.Rev, "ops": []any{map[string]any{"op": "add", "title": "a"}}})
	if code != exitOK {
		t.Fatal(reply)
	}
	newRev := reply["rev"].(string)
	_, stdout, _ = session(t, "export", "--json", "--include", "rev")
	_ = json.Unmarshal([]byte(stdout), &snap)
	if snap.Rev != newRev {
		t.Fatalf("export rev %q, apply reported %q", snap.Rev, newRev)
	}
	// The same stale rev now conflicts.
	code, reply = applyIn(t, map[string]any{"rev": "0:", "ops": []any{map[string]any{"op": "add", "title": "b"}}})
	if code != exitConflict {
		t.Fatalf("exit %d, want 8: %v", code, reply)
	}
}

func TestExportMachineSections(t *testing.T) {
	scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$a", "title": "a", "refs": []string{"x:1"}, "source": "observed"},
		map[string]any{"op": "add", "as": "$b", "title": "b"},
		map[string]any{"op": "claim", "node": "$a", "holder": "h"},
		map[string]any{"op": "link", "from": "$a", "kind": "depends-on", "to": "$b"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	plain := func(include string) string {
		args := []string{"export", "--json"}
		if include != "" {
			args = append(args, "--include", include)
		}
		_, out, _ := session(t, args...)
		return out
	}
	// The default and the presentation sections are untouched by the new ones.
	for _, in := range []string{"", "claims", "refs", "all"} {
		if out := plain(in); strings.Contains(out, `"rev"`) || strings.Contains(out, `"live"`) || strings.Contains(out, `"bindings"`) || strings.Contains(out, `"aliases"`) {
			t.Errorf("--include %q carries a machine section:\n%s", in, out)
		}
	}
	var snap struct {
		Rev   string `json:"rev"`
		Nodes []struct {
			Title string `json:"title"`
			Refs  []struct {
				Ref      string `json:"ref"`
				Bindings []struct {
					ID, By, Source, At string
				} `json:"bindings"`
			} `json:"refs"`
			Claims []struct {
				ID   string `json:"id"`
				Live *bool  `json:"live"`
			} `json:"claims"`
		} `json:"nodes"`
		Edges   []struct{ ID string } `json:"edges"`
		Aliases *map[string]string    `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(plain("claims,rev,ids,live,bindings,aliases")), &snap); err != nil {
		t.Fatal(err)
	}
	if !revForm.MatchString(snap.Rev) || snap.Rev != reply["rev"] {
		t.Errorf("rev %q, apply said %v", snap.Rev, reply["rev"])
	}
	a := snap.Nodes[0]
	if len(a.Claims) != 1 || !hexID.MatchString(a.Claims[0].ID) || a.Claims[0].Live == nil || !*a.Claims[0].Live {
		t.Errorf("claim = %+v", a.Claims)
	}
	if len(a.Refs) != 1 || len(a.Refs[0].Bindings) != 1 || a.Refs[0].Bindings[0].Source != "observed" || a.Refs[0].Bindings[0].By != "agent:test" {
		t.Errorf("refs = %+v", a.Refs)
	}
	if len(snap.Edges) != 1 || !hexID.MatchString(snap.Edges[0].ID) {
		t.Errorf("edges = %+v", snap.Edges)
	}
	if snap.Aliases == nil || len(*snap.Aliases) != 0 {
		t.Errorf("aliases = %v, want an empty object", snap.Aliases)
	}
}

// A null where a value belongs is refused. Read as absent, live_on: null — what
// a client marshalling a nil slice sends — would commit with the at-most-one
// guard off.
func TestApplyRefusesNullValues(t *testing.T) {
	dir := scratch(t)
	setupWorkstreams(t, "A", "B")
	before := readLog(t, dir)
	for name, x := range map[string]string{
		"live_on":  `{"holder":"agent:S1","live_on":null}`,
		"state":    `{"node":"x","state":null}`,
		"claims":   `{"node":"x","claims":null}`,
		"bound_to": `{"ref":"r","bound_to":null}`,
		"children": `{"node":"x","children":null}`,
		"node":     `{"node":null,"state":"open"}`,
		"holder":   `{"holder":null,"live_on":[]}`,
	} {
		raw := `{"expect":[` + x + `],"ops":[{"op":"add","title":"t"}]}`
		var out, errb bytes.Buffer
		if code := runWithStdin([]string{"apply"}, strings.NewReader(raw), &out, &errb); code != exitUsage {
			t.Errorf("%s: exit %d, want 1: %s", name, code, out.String())
		}
	}
	if !bytes.Equal(before, readLog(t, dir)) {
		t.Error("a refused request wrote to the log")
	}
}

// "me" is the caller on every side of a batch: claim, release, and live_on.
func TestApplyMeIsTheCaller(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_ACTOR", "agent:me")
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$A", "title": "a", "kind": "workstream", "ns": "workstreams"},
		map[string]any{"op": "add", "as": "$B", "title": "b", "kind": "workstream", "ns": "workstreams"},
		map[string]any{"op": "claim", "node": "$A", "holder": "me"},
	}})
	if code != exitOK {
		t.Fatal(reply)
	}
	tmp := reply["tmp"].(map[string]any)
	a, b := tmp["$A"].(string), tmp["$B"].(string)
	code, reply = applyIn(t, map[string]any{
		"expect": []any{map[string]any{"holder": "me", "live_on": []string{a}}},
		"ops": []any{
			map[string]any{"op": "release", "node": a, "holder": "me"},
			map[string]any{"op": "claim", "node": b, "holder": "me"},
		},
	})
	if code != exitOK {
		t.Fatalf("exit %d: %v", code, reply)
	}
	if on := liveOn(t, "agent:me"); len(on) != 1 || on[0] != b {
		t.Fatalf("caller is live on %v, want only B", on)
	}
}

// children counts live part-of children only: a dropped child no longer
// decomposes its parent, so it must neither block a close nor be required by
// one. Other edge kinds are named with kind, and a missing parent reads as
// {exists: false} like the state expectation.
func TestApplyChildrenExpectation(t *testing.T) {
	scratch(t)
	code, reply := applyIn(t, map[string]any{"ops": []any{
		map[string]any{"op": "add", "as": "$P", "title": "parent"},
		map[string]any{"op": "add", "as": "$K", "title": "kept child"},
		map[string]any{"op": "add", "as": "$D", "title": "dropped child"},
		map[string]any{"op": "add", "as": "$B", "title": "blocker"},
		map[string]any{"op": "link", "from": "$K", "kind": "part-of", "to": "$P"},
		map[string]any{"op": "link", "from": "$D", "kind": "part-of", "to": "$P"},
		map[string]any{"op": "link", "from": "$B", "kind": "depends-on", "to": "$P"},
	}})
	if code != exitOK {
		t.Fatalf("setup: %v", reply)
	}
	id := map[string]string{}
	for k, v := range reply["tmp"].(map[string]any) {
		id[k] = v.(string)
	}
	if code, _, stderr := session(t, "drop", id["$D"]); code != exitOK {
		t.Fatalf("drop: %s", stderr)
	}
	check := func(x map[string]any) (int, map[string]any) {
		return applyIn(t, map[string]any{"expect": []any{x}, "ops": []any{map[string]any{"op": "done", "node": id["$P"]}}}, "--dry-run")
	}
	for name, c := range map[string]struct {
		x    map[string]any
		code int
	}{
		"live child holds":      {map[string]any{"node": id["$P"], "children": []any{id["$K"]}}, exitOK},
		"dropped child counted": {map[string]any{"node": id["$P"], "children": []any{id["$K"], id["$D"]}}, exitConflict},
		"none":                  {map[string]any{"node": id["$P"], "children": []any{}}, exitConflict},
		"other kind":            {map[string]any{"node": id["$P"], "children": []any{id["$B"]}, "kind": "depends-on"}, exitOK},
		"leaf has none":         {map[string]any{"node": id["$K"], "children": []any{}}, exitOK},
	} {
		if code, reply := check(c.x); code != c.code {
			t.Errorf("%s: exit %d, want %d: %v", name, code, c.code, reply)
		}
	}
	code, reply = check(map[string]any{"node": id["$P"], "children": []any{}})
	got := reply["failed"].([]any)[0].(map[string]any)["actual"]
	if want := map[string]any{"children": []any{id["$K"]}}; !reflect.DeepEqual(got, want) {
		t.Errorf("actual = %v, want %v", got, want)
	}
	code, reply = check(map[string]any{"node": strings.Repeat("f", 64), "children": []any{}})
	got = reply["failed"].([]any)[0].(map[string]any)["actual"]
	if want := map[string]any{"exists": false}; code != exitConflict || !reflect.DeepEqual(got, want) {
		t.Errorf("missing parent: exit %d, actual %v", code, got)
	}
}
