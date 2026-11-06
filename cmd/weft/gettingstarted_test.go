package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The getting-started page is the first thing a human runs, so an example that
// no longer works is the worst possible first impression — and it is exactly
// the kind of rot nothing catches. The agent snippet is pinned against the
// binary this same way (agentdoc_test.go); this is the equivalent for the human
// artifact.
//
// Every `$ weft …` line in the document is executed in order against a scratch
// store, with the page's own node ids replaced by the ones this run produced.
// Placeholders would have made this vacuous: the page shows real ids, and the
// point is that the sequence works end to end.
func TestGettingStartedCommandsAllRun(t *testing.T) {
	page, err := os.ReadFile("../../docs/getting-started.html")
	if err != nil {
		t.Fatal(err)
	}
	scratch(t)
	t.Setenv("WEFT_NS", "acme/api")
	t.Setenv("WEFT_ACTOR", "user:you")

	// Ids are content hashes, so the page's are not reproducible. Map each one
	// to the id this run created for the same title, in the order the page adds
	// them.
	ids := map[string]string{}
	// The ids exactly as the page prints them, so a substitution that silently
	// did not happen is detectable rather than passed through to weft.
	pageIDs := map[string]bool{
		"f46bc2b": true, "7f9341e": true, "ca75891": true, "b8723f9": true,
	}
	titles := []struct{ pageID, title, kind string }{
		{"f46bc2b", "N+1 in report generation", "bug"},
		{"7f9341e", "cache invalidation race", "bug"},
		{"ca75891", "batch export spec", "spec"},
		{"b8723f9", "task 7: batch export", "task"},
	}

	line := regexp.MustCompile(`<span class="cmd">\$ weft ([^<]+)</span>`)
	var ran int
	for _, m := range line.FindAllStringSubmatch(string(page), -1) {
		raw := strings.TrimSpace(m[1])
		// The pipe examples hand off to another program; only weft's half is
		// this suite's business.
		if i := strings.Index(raw, "|"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		args := fields(raw)
		if len(args) == 0 {
			continue
		}
		for i, a := range args {
			if sub, ok := ids[a]; ok {
				args[i] = sub
			}
		}
		// A page id that survived substitution means the step before it did not
		// produce the node this one names. Left unchecked, `link` then exits 2,
		// the dependency never exists, and the closing assertions pass because
		// an unblocked task is trivially ready — a broken example shipping
		// green, which is the one outcome this test exists to prevent.
		for _, a := range args {
			if _, isPageID := pageIDs[a]; isPageID {
				t.Fatalf("`weft %s` still names the page's id %q; the add before it "+
					"did not resolve, so everything after this is meaningless", raw, a)
			}
		}
		ran++
		code, stdout, stderr := session(t, args...)
		// Every command the page shows is one a reader will run and expect to
		// work, so nothing less than success will do. Exit 2 was tolerated here
		// once, and it is exactly what a failed substitution produces.
		if code != exitOK {
			t.Fatalf("getting-started shows `weft %s`, which exits %d:\n%s", raw, code, stderr)
		}
		// Learn the id of anything the page just added, so later steps that
		// name it resolve.
		if args[0] == "add" {
			for _, want := range titles {
				if strings.Contains(raw, want.title) {
					if f := fields(stdout); len(f) > 0 {
						ids[want.pageID] = f[0]
					}
				}
			}
		}
	}
	// A parser that silently matched nothing would make this test pass while
	// checking nothing at all.
	if ran < 12 {
		t.Errorf("only %d commands were exercised; the parser probably missed a block", ran)
	}

	// The document's payoff is the frontier moving. If that stops being true
	// the page is telling a story the tool no longer performs.
	_, ready, _ := session(t, "ls", "--ready")
	if !strings.Contains(ready, "task 7: batch export") {
		t.Errorf("after the spec is done, the task must be ready — the page says so:\n%s", ready)
	}
	if strings.Contains(ready, "batch export spec") {
		t.Errorf("a done node is still in the frontier:\n%s", ready)
	}
}

// fields splits a command line, honouring the double quotes the page uses for
// titles and notes.
func fields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, has := false, false
	flush := func() {
		if has {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			has = true
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}
