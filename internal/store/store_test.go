package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("WEFT_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustNode(t *testing.T, title, actor string) event.Event {
	t.Helper()
	e, err := event.NodeAdd("test", "bug", title, actor, event.SourceAsserted, time.Now())
	if err != nil {
		t.Fatalf("NodeAdd: %v", err)
	}
	return e
}

func fold(t *testing.T, s *Store) *graph.Graph {
	t.Helper()
	g, err := graph.Fold(s.Fold)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	return g
}

func TestEmptyLogFolds(t *testing.T) {
	s := testStore(t)
	g := fold(t, s)
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Errorf("empty store folded to %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}
}

func TestAppendAndFold(t *testing.T) {
	s := testStore(t)
	a := mustNode(t, "first", "user:matt")
	b := mustNode(t, "second", "user:matt")
	if err := s.Append(a, b); err != nil {
		t.Fatalf("Append: %v", err)
	}
	g := fold(t, s)
	if len(g.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(g.Nodes))
	}
	if g.Nodes[a.ID()].Title != "first" {
		t.Errorf("title not preserved: %q", g.Nodes[a.ID()].Title)
	}
}

// The headline exit condition for milestone 1. Two writers concurrently attempt
// opposite depends-on edges; each validates against a graph with no edges. If
// validation happened outside the lock, both would commit and produce the cycle
// both had just rejected (EVENTS.md §6).
//
// flock is held on the open file description, so two separate opens contend
// identically whether they are in one process or two.
func TestRacingOppositeEdgesCommitExactlyOne(t *testing.T) {
	s := testStore(t)
	a := mustNode(t, "A", "user:matt")
	b := mustNode(t, "B", "user:matt")
	if err := s.Append(a, b); err != nil {
		t.Fatal(err)
	}

	addEdge := func(from, to string) error {
		st, err := Open()
		if err != nil {
			return err
		}
		return st.Mutate(func(f func(func(event.Event) error) error) ([]event.Event, error) {
			g, err := graph.Fold(f)
			if err != nil {
				return nil, err
			}
			if err := g.CheckEdge(graph.EdgeDependsOn, from, to); err != nil {
				return nil, err
			}
			e, err := event.EdgeAdd(graph.EdgeDependsOn, from, to, "user:racer", event.SourceAsserted, time.Now())
			if err != nil {
				return nil, err
			}
			return []event.Event{e}, nil
		})
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, pair := range [][2]string{{a.ID(), b.ID()}, {b.ID(), a.ID()}} {
		wg.Add(1)
		go func(i int, from, to string) {
			defer wg.Done()
			<-start
			errs[i] = addEdge(from, to)
		}(i, pair[0], pair[1])
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 racing edges committed, want exactly 1 (errors: %v)", succeeded, errs)
	}

	g := fold(t, s)
	if len(g.Edges) != 1 {
		t.Fatalf("log holds %d edges, want 1", len(g.Edges))
	}
	// Whichever direction won, the loser's edge must now be rejected — that is
	// the invariant the transaction exists to preserve.
	for _, e := range g.Edges {
		if err := g.CheckEdge(e.Kind, e.To, e.From); err == nil {
			t.Errorf("reverse of the committed edge %s->%s was not rejected as a cycle",
				e.From[:7], e.To[:7])
		}
	}
}

// N processes x M events must produce exactly N*M valid lines, none interleaved
// mid-line. This is the bug class that corrupts the durable artifact rather
// than merely returning a wrong answer.
func TestConcurrentAppend(t *testing.T) {
	s := testStore(t)
	const writers, each = 8, 50

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st, err := Open()
			if err != nil {
				t.Error(err)
				return
			}
			<-start
			for i := 0; i < each; i++ {
				e, err := event.NodeAdd("test", "bug",
					fmt.Sprintf("w%d-%d", w, i), fmt.Sprintf("agent:%d", w),
					event.SourceInferred, time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				if err := st.Append(e); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != writers*each {
		t.Fatalf("got %d lines, want %d", len(lines), writers*each)
	}
	for i, line := range lines {
		if _, err := event.Parse([]byte(line)); err != nil {
			t.Fatalf("line %d does not parse: %v", i+1, err)
		}
	}

	g := fold(t, s)
	if len(g.Nodes) != writers*each {
		t.Errorf("folded %d nodes, want %d", len(g.Nodes), writers*each)
	}
}

// A writer killed mid-append leaves a fragment. Readers discard it because a
// committed event includes its terminating LF; the next writer truncates it
// (EVENTS.md §5.3).
func TestPartialFinalRecord(t *testing.T) {
	s := testStore(t)
	good := mustNode(t, "committed", "user:matt")
	if err := s.Append(good); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(s.Path(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"ev":"node.add","id":"aaa`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// The reader ignores the fragment rather than failing.
	g := fold(t, s)
	if len(g.Nodes) != 1 {
		t.Fatalf("reader folded %d nodes across a partial record, want 1", len(g.Nodes))
	}

	// The next writer truncates it, so the log is well-formed again.
	next := mustNode(t, "after crash", "user:matt")
	if err := s.Append(next); err != nil {
		t.Fatalf("append after partial record: %v", err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"id":"aaa`) {
		t.Error("fragment survived the next append")
	}
	if g := fold(t, s); len(g.Nodes) != 2 {
		t.Errorf("folded %d nodes after recovery, want 2", len(g.Nodes))
	}
}

// A partial record that is the *only* content must also be recoverable.
func TestPartialFirstRecord(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(s.Path(), []byte(`{"v":1,"ev":"node`), 0o644); err != nil {
		t.Fatal(err)
	}
	if g := fold(t, s); len(g.Nodes) != 0 {
		t.Fatalf("folded %d nodes from a lone fragment, want 0", len(g.Nodes))
	}
	if err := s.Append(mustNode(t, "recovered", "user:matt")); err != nil {
		t.Fatalf("append over a lone fragment: %v", err)
	}
	if g := fold(t, s); len(g.Nodes) != 1 {
		t.Errorf("folded %d nodes after recovery, want 1", len(g.Nodes))
	}
}

// An unparseable non-final line cannot be explained by a crashed writer, so it
// is fatal rather than skippable (EVENTS.md §5.3).
func TestCorruptNonFinalLineIsFatal(t *testing.T) {
	s := testStore(t)
	if err := s.Append(mustNode(t, "ok", "user:matt")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.Path())
	corrupted := append([]byte("this is not json\n"), data...)
	if err := os.WriteFile(s.Path(), corrupted, 0o644); err != nil {
		t.Fatal(err)
	}

	var corrupt *ErrCorrupt
	_, err := graph.Fold(s.Fold)
	if err == nil {
		t.Fatal("expected a corruption error")
	}
	if !errors.As(err, &corrupt) {
		t.Fatalf("expected ErrCorrupt, got %T: %v", err, err)
	}
	if corrupt.Line != 1 {
		t.Errorf("reported line %d, want 1", corrupt.Line)
	}
}

// Readers must not block writers, and must see a consistent prefix.
func TestReadDuringAppend(t *testing.T) {
	s := testStore(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			e, err := event.NodeAdd("test", "bug", fmt.Sprintf("n%d", i), "user:w", event.SourceAsserted, time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			if err := s.Append(e); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			if _, err := graph.Fold(s.Fold); err != nil {
				t.Fatalf("read during append failed: %v", err)
			}
		}
	}
}

// A crashed-and-retried write must be idempotent (EVENTS.md §5.2).
func TestDuplicateEventIDIgnored(t *testing.T) {
	s := testStore(t)
	e := mustNode(t, "written twice", "user:matt")
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if g := fold(t, s); len(g.Nodes) != 1 {
		t.Errorf("folded %d nodes from a duplicated event, want 1", len(g.Nodes))
	}
}

func TestMutateAbortWritesNothing(t *testing.T) {
	s := testStore(t)
	sentinel := errors.New("abort")
	err := s.Mutate(func(func(func(event.Event) error) error) ([]event.Event, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the sentinel", err)
	}
	if _, err := os.Stat(s.Path()); err == nil {
		data, _ := os.ReadFile(s.Path())
		if len(data) != 0 {
			t.Errorf("aborted mutation wrote %d bytes", len(data))
		}
	}
}

func TestDirPrecedence(t *testing.T) {
	t.Setenv("WEFT_HOME", "/explicit")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if d, _ := Dir(); d != "/explicit" {
		t.Errorf("WEFT_HOME should win, got %s", d)
	}
	t.Setenv("WEFT_HOME", "")
	if d, _ := Dir(); d != filepath.Join("/xdg", "weft") {
		t.Errorf("XDG_DATA_HOME should apply, got %s", d)
	}
}
