package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
)

// Conformance fixtures are committed data, not generated (EVENTS.md §7). Any
// implementation claiming to read weft logs must reproduce these, so the
// fixtures are the portable statement of the contract and this test is merely
// one consumer of them.
const conformanceDir = "../../testdata/conformance"

type expectation struct {
	Nodes int    `json:"nodes"`
	Edges int    `json:"edges"`
	Error string `json:"error"`
	// States and Merged are keyed by full node id. Merged names the node each
	// merged node folded into, which is the part of the contract a reader can
	// get wrong while still counting the right number of nodes.
	States map[string]string `json:"states"`
	Merged map[string]string `json:"merged"`
	// Observed pins the *latest* observation per node+ref. The node's own state
	// staying put beside a diverging observation is the contract: upstream is
	// cached, never authoritative (SPEC.md §10). The payload clock and hash
	// (EVENTS.md §3.8) are pinned as the event's exact strings; empty means the
	// fixture's scheme does not report the field.
	Observed []struct {
		Node     string `json:"node"`
		Ref      string `json:"ref"`
		State    string `json:"state"`
		Title    string `json:"title"`
		Created  string `json:"created"`
		Modified string `json:"modified"`
		Hash     string `json:"hash"`
	} `json:"observed"`
}

func TestConformanceFixtures(t *testing.T) {
	logs, err := filepath.Glob(filepath.Join(conformanceDir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatalf("no fixtures found in %s", conformanceDir)
	}

	for _, logPath := range logs {
		name := strings.TrimSuffix(filepath.Base(logPath), ".jsonl")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(conformanceDir, name+".expect.json"))
			if err != nil {
				t.Fatalf("missing expectation: %v", err)
			}
			var want expectation
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("bad expectation: %v", err)
			}

			// Install the fixture as a real store so the fixture exercises the
			// same read path as production, not a parallel one.
			dir := t.TempDir()
			t.Setenv("WEFT_HOME", dir)
			body, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := Open()
			if err != nil {
				t.Fatal(err)
			}

			g, err := graph.Fold(s.Fold)
			switch want.Error {
			case "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case "unsupported-version":
				var e *event.ErrUnsupportedVersion
				if !errors.As(err, &e) {
					t.Fatalf("want ErrUnsupportedVersion, got %T: %v", err, err)
				}
				return
			case "corrupt":
				var e *ErrCorrupt
				if !errors.As(err, &e) {
					t.Fatalf("want ErrCorrupt, got %T: %v", err, err)
				}
				return
			default:
				t.Fatalf("unknown expected error %q", want.Error)
			}

			if len(g.Nodes) != want.Nodes {
				t.Errorf("folded %d nodes, want %d", len(g.Nodes), want.Nodes)
			}
			if len(g.Edges) != want.Edges {
				t.Errorf("folded %d edges, want %d", len(g.Edges), want.Edges)
			}
			for id, state := range want.States {
				n, ok := g.Nodes[id]
				if !ok {
					t.Errorf("expected node %s to exist", id[:7])
					continue
				}
				if n.State != state {
					t.Errorf("node %s state = %q, want %q", id[:7], n.State, state)
				}
			}
			for _, o := range want.Observed {
				got := g.Observed(o.Node, o.Ref)
				if got == nil {
					t.Errorf("no observation of %s on %s", o.Ref, o.Node[:7])
					continue
				}
				if got.State != o.State || got.Title != o.Title {
					t.Errorf("observation of %s = (%q, %q), want (%q, %q)",
						o.Ref, got.State, got.Title, o.State, o.Title)
				}
				if got.Created != o.Created || got.Modified != o.Modified || got.Hash != o.Hash {
					t.Errorf("observation payload of %s = (%q, %q, %q), want (%q, %q, %q)",
						o.Ref, got.Created, got.Modified, got.Hash, o.Created, o.Modified, o.Hash)
				}
			}
			for id, into := range want.Merged {
				if got := g.Canonical(id); got != into {
					t.Errorf("node %s folded into %s, want %s", id[:7], got[:7], into[:7])
				}
			}
		})
	}
}
