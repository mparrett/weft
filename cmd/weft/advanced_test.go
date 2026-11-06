package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The advanced walkthrough is a longer sequence than getting-started and depends
// on order in ways a reader cannot see: the merge has to happen before the show
// that proves the merged id still resolves, the claim has to expire before the
// stale sweep, and the `done --undo` has to land before the closing state. A
// page like that rots without anybody noticing, so the whole document is
// executed here in document order against a scratch store.
//
// Two things differ from gettingstarted_test.go, and both are in the page's
// markup rather than in a list kept here:
//
//   - data-exit="N" on a command that is meant to fail. Section 8 shows what
//     weft refuses; asserting exit 0 everywhere would make those examples
//     unrunnable, and asserting nothing would let a rejection quietly become a
//     success.
//   - data-skip on the one command that shells out to `gh`. The reason travels
//     with the line, and this test still checks that nothing else claims it.
func TestAdvancedWalkthroughCommandsAllRun(t *testing.T) {
	page, err := os.ReadFile("../../docs/advanced-walkthrough.html")
	if err != nil {
		t.Fatal(err)
	}
	scratch(t)
	t.Setenv("WEFT_NS", "acme/api")
	t.Setenv("WEFT_ACTOR", "user:you")

	// Ids are content hashes, so the page's are not reproducible. Map each one
	// to the id this run created for the same title.
	ids := map[string]string{}
	pageIDs := map[string]bool{}
	titles := []struct{ pageID, title string }{
		{"0576d6a", "batch export v2"},
		{"8baa619", "batch export spec"},
		{"6413156", "N+1 in report generation"},
		{"2d7e850", "extract report query"},
		{"8aa903a", "export progress panel"},
		{"6c66798", "rate-limit middleware"},
		{"e277ad7", "extract the report query"},
	}
	for _, want := range titles {
		pageIDs[want.pageID] = true
	}

	line := regexp.MustCompile(`<span class="cmd"([^>]*)>\$ ([^<]+)</span>`)
	attr := func(tag, name string) (string, bool) {
		m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil {
			return "", false
		}
		return m[1], true
	}

	var ran, skipped int
	outputs := map[string]string{}
	for _, m := range line.FindAllStringSubmatch(string(page), -1) {
		tag, raw := m[1], strings.TrimSpace(m[2])
		// The pipe examples hand off to another program; only weft's half is
		// this suite's business.
		if i := strings.Index(raw, "|"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		args := fields(raw)
		if len(args) == 0 {
			continue
		}
		// `$ sleep 3` is load-bearing: the stale sweep two lines later returns
		// nothing without it, and the page would be showing an output it cannot
		// produce.
		if args[0] == "sleep" {
			d, err := time.ParseDuration(args[1] + "s")
			if err != nil || d > 10*time.Second {
				t.Fatalf("`%s`: not a sleep this test is willing to run", raw)
			}
			time.Sleep(d)
			continue
		}
		// Lines that drive the Python tools are shown, not run here; they have
		// their own tests under tools/.
		if args[0] != "weft" {
			continue
		}
		args = args[1:]
		if reason, ok := attr(tag, "data-skip"); ok {
			// Only the GitHub shell-out gets to opt out. Anything else marked
			// skip would be an example nobody is checking.
			if args[0] != "import" {
				t.Errorf("`%s` is marked data-skip=%q, but only `import` may skip", raw, reason)
			}
			skipped++
			continue
		}
		for i, a := range args {
			if sub, ok := ids[a]; ok {
				args[i] = sub
			}
		}
		// A page id that survived substitution means the step that was supposed
		// to create that node did not run. Left unchecked the command exits 2,
		// the edge never forms, and the closing assertions pass anyway because
		// a task with no blocker is trivially ready — a broken page shipping
		// green, which is what this test exists to prevent.
		for _, a := range args {
			if pageIDs[a] {
				t.Fatalf("`%s` still names the page's id %q; the add before it "+
					"did not resolve, so everything after this is meaningless", raw, a)
			}
		}
		want := exitOK
		if v, ok := attr(tag, "data-exit"); ok {
			if want, err = strconv.Atoi(v); err != nil {
				t.Fatalf("`%s` has an unparseable data-exit %q", raw, v)
			}
		}
		ran++
		code, stdout, stderr := session(t, args...)
		if code != want {
			t.Fatalf("the page shows `%s` exiting %d; it exits %d\nstdout: %s\nstderr: %s",
				raw, want, code, stdout, stderr)
		}
		outputs[raw] = stdout + stderr
		if args[0] == "add" {
			for _, want := range titles {
				if strings.Contains(raw, `"`+want.title+`"`) {
					if f := fields(stdout); len(f) > 0 {
						ids[want.pageID] = f[0]
					}
				}
			}
		}
	}
	// A parser that silently matched nothing would make this test pass while
	// checking nothing at all.
	if ran < 40 {
		t.Errorf("only %d commands were exercised; the parser probably missed a block", ran)
	}
	if skipped != 1 {
		t.Errorf("%d commands were skipped; exactly one (`import`) should be", skipped)
	}

	// The claims section only means something if the sweep actually swept. The
	// TTL is two seconds precisely so this is reachable.
	if got := outputs["weft ls --stale --all-ns"]; !strings.Contains(got, "N+1 in report generation") {
		t.Errorf("the expired claim did not turn up in `ls --stale`:\n%s", got)
	}

	// Section 2's claim: the merged node stops being its own node, and its id
	// keeps resolving to the survivor.
	_, all, _ := session(t, "ls", "--all-ns")
	if strings.Contains(all, "extract the report query") {
		t.Errorf("the merged node is still listed in its own right:\n%s", all)
	}
	if !strings.Contains(all, "extract report query") {
		t.Errorf("the merge survivor is missing:\n%s", all)
	}
	// Section 7 drops the PR, and the page shows it still listed and annotated
	// — drop is not delete.
	if !strings.Contains(all, "rate-limit middleware  (dropped)") {
		t.Errorf("a dropped node should still list, annotated:\n%s", all)
	}

	// Section 3 is the page's real argument: a namespace filter must not hide a
	// blocker that lives in another namespace. Section 7 undoes the `done`, so
	// this has to hold at the end of the document too.
	panelID, ok := ids["8aa903a"]
	if !ok {
		t.Fatal("the walkthrough never created the progress panel")
	}
	_, why, _ := session(t, "why", panelID)
	if !strings.Contains(why, "batch export spec") {
		t.Errorf("the cross-namespace blocker is gone from `why`:\n%s", why)
	}
	_, ready, _ := session(t, "ls", "--ready", "--ns", "acme/web")
	if strings.Contains(ready, "export progress panel") {
		t.Errorf("`done --undo` did not put the panel back behind its blocker:\n%s", ready)
	}
}
