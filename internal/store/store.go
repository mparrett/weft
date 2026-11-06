// Package store implements the log's read and write protocol (docs/EVENTS.md §5, §6).
package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mparrett/weft/internal/event"
)

// Store is a handle on the append-only log. It holds no state between calls;
// every read folds the file (EVENTS.md §5).
type Store struct {
	path string
}

// ErrCorrupt reports an unparseable line that is not a partial final record.
// Unlike a trailing fragment, this cannot be explained by a crashed writer, so
// it is fatal rather than skippable (EVENTS.md §5.3).
type ErrCorrupt struct {
	Line int
	Err  error
}

func (e *ErrCorrupt) Error() string {
	return fmt.Sprintf("log corrupt at line %d: %v", e.Line, e.Err)
}

func (e *ErrCorrupt) Unwrap() error { return e.Err }

// Dir resolves the store directory.
//
// WEFT_HOME wins so tests and agents can point at a scratch store. Otherwise
// XDG applies even on macOS: Application Support is the platform convention,
// but a path that differs per platform is a path that gets hardcoded wrong
// (BUILD.md §1.3).
func Dir() (string, error) {
	dir, _, err := ResolveDir()
	return dir, err
}

// ResolveDir is Dir plus the rule that chose the path.
//
// Which rule applied matters as much as the path, for the same reason it does
// for a namespace: "/Users/x/.weft" alone does not say whether WEFT_HOME is set
// in this shell and following you between projects. A store written to by
// accident is the one mistake here that is both silent and durable.
func ResolveDir() (dir, source string, err error) {
	if d := os.Getenv("WEFT_HOME"); d != "" {
		return d, "WEFT_HOME", nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "weft"), "XDG_DATA_HOME", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".weft"), "default", nil
}

// Open resolves the store path, creating the directory if needed. It does not
// create the log; an absent log folds to an empty graph.
func Open() (*Store, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating store directory: %w", err)
	}
	return &Store{path: filepath.Join(dir, "log.jsonl")}, nil
}

// Path returns the log's location.
func (s *Store) Path() string { return s.path }

// Fold reads the log and applies fn to each event in file order.
//
// File order is the total order, never "at" order: wall clocks move backwards
// and file position does not (EVENTS.md §5.1).
//
// Reads take no lock. A trailing fragment left by a crashed writer is discarded
// because a committed event includes its terminating LF (EVENTS.md §5.3).
func (s *Store) Fold(fn func(event.Event) error) error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading log: %w", err)
	}
	return foldBytes(data, fn)
}

func foldBytes(data []byte, fn func(event.Event) error) error {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.Split(data, []byte("\n"))
	// A complete final event leaves an empty element after the trailing LF; an
	// incomplete one leaves the fragment. Either way the last element is not a
	// committed event.
	lines = lines[:len(lines)-1]

	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		if len(line) == 0 {
			return &ErrCorrupt{Line: i + 1, Err: errors.New("blank line")}
		}
		e, err := event.Parse(line)
		if err != nil {
			var unsupported *event.ErrUnsupportedVersion
			if errors.As(err, &unsupported) {
				// Not corruption — a newer writer. Refusing is the point.
				return err
			}
			return &ErrCorrupt{Line: i + 1, Err: err}
		}
		// First occurrence wins, so a crashed-and-retried write is idempotent
		// (EVENTS.md §5.2).
		if seen[e.ID()] {
			continue
		}
		seen[e.ID()] = true
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// Append writes events under an exclusive lock.
//
// For mutations that validate against current state, use Mutate: holding the
// lock only across the append is not enough to keep an invariant (EVENTS.md §6).
func (s *Store) Append(events ...event.Event) error {
	return s.withLock(func(f *os.File) error {
		return appendLocked(f, events)
	})
}

// Mutate runs the full transaction: lock, fold, validate, append, fsync.
//
// fn sees the graph as of the moment the lock was taken and returns the events
// to commit, or an error to abort. This is what stops two processes from each
// validating opposite edges and both committing the cycle they just rejected.
func (s *Store) Mutate(fn func(fold func(func(event.Event) error) error) ([]event.Event, error)) error {
	return s.withLock(func(f *os.File) error {
		data, err := os.ReadFile(s.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading log: %w", err)
		}
		events, err := fn(func(apply func(event.Event) error) error {
			return foldBytes(data, apply)
		})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		return appendLocked(f, events)
	})
}

// withLock opens the log and holds an exclusive lock for the whole of fn.
//
// Every mutation goes through here. Read paths deliberately do not, so a long
// fold never blocks a writer (EVENTS.md §6).
func (s *Store) withLock(fn func(*os.File) error) error {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return err
	}
	defer func() { _ = unlockFile(f) }()

	return fn(f)
}

func appendLocked(f *os.File, events []event.Event) error {
	if err := truncatePartial(f); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range events {
		line, err := e.Marshal()
		if err != nil {
			return fmt.Errorf("marshalling event: %w", err)
		}
		if bytes.ContainsRune(line, '\n') {
			return errors.New("marshalled event contains a newline")
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Seek(0, 2); err != nil {
		return fmt.Errorf("seeking to end: %w", err)
	}
	// One write of all complete lines. Concurrent appends cannot interleave
	// mid-line because every writer holds the exclusive lock.
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("appending: %w", err)
	}
	// Before releasing the lock: this is the durable artifact.
	return f.Sync()
}

// truncatePartial removes a trailing fragment left by a crashed writer. Safe
// only under the exclusive lock, and safe at all because the fragment was never
// a committed event (EVENTS.md §5.3).
func truncatePartial(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return fmt.Errorf("reading final byte: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return fmt.Errorf("reading log: %w", err)
	}
	keep := bytes.LastIndexByte(data, '\n') + 1 // 0 when there is no LF at all
	if err := f.Truncate(int64(keep)); err != nil {
		return fmt.Errorf("truncating partial record: %w", err)
	}
	return nil
}
