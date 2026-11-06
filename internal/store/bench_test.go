package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
)

// seedLog writes exactly n events — a mix of nodes and edges, so the fold does
// real work rather than folding a degenerate node-only log. n is the event
// count because that is what the BUILD.md §1.4 budget is stated in.
func seedLog(tb testing.TB, dir string, n int) {
	tb.Helper()
	f, err := os.Create(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()

	at := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	var prev string
	written := 0
	for i := 0; written < n; i++ {
		node, err := event.NodeAdd("bench", "task", fmt.Sprintf("node %d", i),
			"agent:bench", event.SourceInferred, at.Add(time.Duration(i)*time.Second))
		if err != nil {
			tb.Fatal(err)
		}
		line, err := node.Marshal()
		if err != nil {
			tb.Fatal(err)
		}
		f.Write(line)
		f.Write([]byte("\n"))
		written++

		if prev != "" && i%2 == 0 && written < n {
			e, err := event.EdgeAdd(graph.EdgeDependsOn, node.ID(), prev,
				"agent:bench", event.SourceInferred, at)
			if err != nil {
				tb.Fatal(err)
			}
			line, err := e.Marshal()
			if err != nil {
				tb.Fatal(err)
			}
			f.Write(line)
			f.Write([]byte("\n"))
			written++
		}
		prev = node.ID()
	}
}

// BenchmarkFold10k measures the read path against the BUILD.md §1.4 budget:
// weft show under 15 ms cold at 10,000 events.
func BenchmarkFold10k(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("WEFT_HOME", dir)
	seedLog(b, dir, 10_000)
	s, err := Open()
	if err != nil {
		b.Fatal(err)
	}
	info, _ := os.Stat(s.Path())
	b.ReportMetric(float64(info.Size())/(1<<20), "MiB")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := graph.Fold(s.Fold); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMutate10k measures the write path. Every validating mutation folds
// the whole log under an exclusive lock (EVENTS.md §6.1), so this is the number
// that degrades with log length and serializes concurrent agents — the likeliest
// first breach of the budget, and the reason it is measured now rather than at
// milestone 8.
func BenchmarkMutate10k(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("WEFT_HOME", dir)
	seedLog(b, dir, 10_000)
	s, err := Open()
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := s.Mutate(func(f func(func(event.Event) error) error) ([]event.Event, error) {
			g, err := graph.Fold(f)
			if err != nil {
				return nil, err
			}
			e, err := event.NodeAdd("bench", "task", fmt.Sprintf("added %d", i),
				"agent:bench", event.SourceInferred, time.Now())
			if err != nil {
				return nil, err
			}
			// Touch the validator so the benchmark covers the real write path.
			_ = g.CheckEdge(graph.EdgeDependsOn, e.ID(), e.ID())
			return []event.Event{e}, nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
