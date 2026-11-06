package main

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// A derived number written into prose is a cached value with no invalidation.
// It reads as authoritative long after it goes false, and nothing fails when it
// does — README and SPEC both said "Seventeen commands" while the binary had
// eighteen. Pinning it here is the cheapest invalidation available: the number
// cannot drift without a test going red.
//
// Only the two documents that state the count as a *fact* are checked. BUILD.md
// deliberately no longer names one, which is the better fix where the number
// was decoration on a rationale rather than a claim.
func TestDocsAgreeOnTheCommandCount(t *testing.T) {
	words := map[int]string{
		15: "Fifteen", 16: "Sixteen", 17: "Seventeen", 18: "Eighteen",
		19: "Nineteen", 20: "Twenty",
	}
	word, ok := words[len(commands)]
	if !ok {
		t.Fatalf("weft has %d commands, which this test has no word for — add it "+
			"to `words` and to the docs", len(commands))
	}
	for _, doc := range []string{"../../README.md", "../../docs/SPEC.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if !strings.Contains(string(b), word+" commands") {
			t.Errorf("%s does not say %q; weft has %d commands", doc, word+" commands", len(commands))
		}
	}
}

// Every command must be listed in the README, which is where a human looks
// first. The agent snippet has its own equivalent (agentdoc_test.go); this is
// the same guarantee for the other audience, and it caught `env` being added to
// the binary and to the snippet but not to the README.
func TestReadmeNamesEveryCommand(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	var missing []string
	for name := range commands {
		// Backticked, which is how the README lists them, so a command name
		// that happens to appear inside prose does not count as documented.
		if !strings.Contains(readme, "`"+name+"`") && !strings.Contains(readme, "weft "+name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the README does not name: %s", strings.Join(missing, ", "))
	}
}
