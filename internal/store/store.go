// Package store implements the log's read and write protocol (docs/EVENTS.md §5, §6).
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	apply := func(e event.Event) error {
		// First occurrence wins, so a crashed-and-retried write is idempotent
		// (EVENTS.md §5.2).
		if seen[e.ID()] {
			return nil
		}
		seen[e.ID()] = true
		return fn(e)
	}

	// A batch's events are held back until its tx.commit arrives (EVENTS.md
	// §5.5). A run that never gets one — torn by a crash after its first
	// complete line, or still being written while this unlocked read ran — was
	// never committed, and applying its prefix is exactly the half-applied
	// batch the framing exists to rule out.
	var pending []event.Event
	pendingTx := ""
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
		tx := e.Str(event.FieldTx)
		if e.Kind() == event.KindTxCommit {
			n, _ := e.Int("n")
			if tx != "" && tx == pendingTx && n == len(pending) {
				for _, p := range pending {
					if err := apply(p); err != nil {
						return err
					}
				}
				if err := apply(e); err != nil {
					return err
				}
			}
			// A commit whose run is not all here commits nothing: the run was
			// torn, and the marker alone asserts no work.
			pending, pendingTx = nil, ""
			continue
		}
		if tx != "" {
			if tx != pendingTx {
				pending, pendingTx = nil, tx
			}
			pending = append(pending, e)
			continue
		}
		// An untagged line ends any open run uncommitted. Only an older writer
		// can produce this shape: it truncates the fragment but not the torn
		// run before appending (EVENTS.md §6).
		pending, pendingTx = nil, ""
		if err := apply(e); err != nil {
			return err
		}
	}
	return nil
}

// Rev names the committed state of a log: the offset just past the last
// committed event, and that event's id prefix. The log is append-only, so equal
// revs mean nothing was committed in between. "0:" is the empty log.
func formatRev(end int64, lastID string) string {
	if len(lastID) > 12 {
		lastID = lastID[:12]
	}
	return fmt.Sprintf("%d:%s", end, lastID)
}

// lineHeader is the part of an event the tail scan needs. Decoding only this is
// what keeps the scan cheap enough to run on every append.
type lineHeader struct {
	ID string `json:"id"`
	Ev string `json:"ev"`
	Tx string `json:"tx"`
}

// committedTail finds where the committed log ends: past the last complete line,
// minus any trailing tx run that has no commit marker. It reads backwards in
// growing chunks, so the cost follows the size of the trailing batch, not the
// log. size must sit on a line boundary (just past an LF, or 0).
func committedTail(readAt func([]byte, int64) (int, error), size int64) (end int64, lastID string, err error) {
	for chunk := int64(64 << 10); ; chunk *= 2 {
		start := max(size-chunk, 0)
		buf := make([]byte, size-start)
		if _, err := readAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, "", fmt.Errorf("reading log tail: %w", err)
		}
		base := start
		if start > 0 {
			// The chunk may begin mid-line; that line belongs to the next pass.
			cut := bytes.IndexByte(buf, '\n') + 1
			buf, base = buf[cut:], start+int64(cut)
		}
		// Offsets of each complete line in buf.
		var offs []int
		for i := 0; i < len(buf); {
			offs = append(offs, i)
			j := bytes.IndexByte(buf[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
		}
		header := func(k int) lineHeader {
			lo, hi := offs[k], len(buf)
			if k+1 < len(offs) {
				hi = offs[k+1]
			}
			var h lineHeader
			// An unparseable line is left to the fold to report; here it simply
			// is not part of a tx run.
			_ = json.Unmarshal(bytes.TrimSuffix(buf[lo:hi], []byte("\n")), &h)
			return h
		}
		if len(offs) == 0 {
			if start == 0 {
				return 0, "", nil
			}
			continue
		}
		k := len(offs) - 1
		last := header(k)
		if last.Tx == "" || last.Ev == event.KindTxCommit {
			return size, last.ID, nil
		}
		// A trailing run with no commit: walk back to where it starts.
		for k >= 0 {
			h := header(k)
			if h.Tx != last.Tx || h.Ev == event.KindTxCommit {
				return base + int64(offs[k+1]), h.ID, nil
			}
			k--
		}
		if start == 0 {
			return 0, "", nil
		}
		// The run, or the line before it, starts before this chunk.
	}
}

func bytesReadAt(data []byte) func([]byte, int64) (int, error) {
	return bytes.NewReader(data).ReadAt
}

// lastLF is the line boundary at or before the end of data: a trailing
// fragment is not part of the committed log.
func lastLF(data []byte) int64 { return int64(bytes.LastIndexByte(data, '\n') + 1) }

// FoldRev folds the log like Fold and also returns its rev, both computed from
// one read, so the rev names exactly the state that was folded.
func (s *Store) FoldRev(fn func(event.Event) error) (string, error) {
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading log: %w", err)
	}
	if err := foldBytes(data, fn); err != nil {
		return "", err
	}
	end, id, err := committedTail(bytesReadAt(data), lastLF(data))
	if err != nil {
		return "", err
	}
	return formatRev(end, id), nil
}

// Append writes events under an exclusive lock.
//
// For mutations that validate against current state, use Mutate: holding the
// lock only across the append is not enough to keep an invariant (EVENTS.md §6).
func (s *Store) Append(events ...event.Event) error {
	return s.withLock(func(f *os.File) error {
		_, err := appendLocked(f, events)
		return err
	})
}

// Mutate runs the full transaction: lock, fold, validate, append, fsync.
//
// fn sees the graph as of the moment the lock was taken and returns the events
// to commit, or an error to abort. This is what stops two processes from each
// validating opposite edges and both committing the cycle they just rejected.
func (s *Store) Mutate(fn func(fold func(func(event.Event) error) error) ([]event.Event, error)) error {
	_, err := s.MutateRev(func(fold func(func(event.Event) error) error, _ string) ([]event.Event, error) {
		return fn(fold)
	})
	return err
}

// MutateRev is Mutate that also hands fn the rev of the state it validates
// against, and returns the rev after the commit (unchanged when fn returned no
// events). The after-rev is computed from the bytes written rather than from a
// fresh read, so reporting it cannot fail a write that landed (BUILD.md §5).
func (s *Store) MutateRev(fn func(fold func(func(event.Event) error) error, rev string) ([]event.Event, error)) (string, error) {
	var after string
	err := s.withLock(func(f *os.File) error {
		data, err := os.ReadFile(s.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading log: %w", err)
		}
		end, id, err := committedTail(bytesReadAt(data), lastLF(data))
		if err != nil {
			return err
		}
		before := formatRev(end, id)
		events, err := fn(func(apply func(event.Event) error) error {
			return foldBytes(data, apply)
		}, before)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			after = before
			return nil
		}
		newEnd, err := appendLocked(f, events)
		if err != nil {
			return err
		}
		after = formatRev(newEnd, events[len(events)-1].ID())
		return nil
	})
	return after, err
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

// appendLocked writes events after discarding anything uncommitted at the tail,
// and returns the new end of the log.
func appendLocked(f *os.File, events []event.Event) (int64, error) {
	if err := truncatePartial(f); err != nil {
		return 0, err
	}
	end, err := truncateTornBatch(f)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	for _, e := range events {
		line, err := e.Marshal()
		if err != nil {
			return 0, fmt.Errorf("marshalling event: %w", err)
		}
		if bytes.ContainsRune(line, '\n') {
			return 0, errors.New("marshalled event contains a newline")
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Seek(0, 2); err != nil {
		return 0, fmt.Errorf("seeking to end: %w", err)
	}
	// One write of all complete lines. Concurrent appends cannot interleave
	// mid-line because every writer holds the exclusive lock.
	if _, err := f.Write(buf.Bytes()); err != nil {
		return 0, fmt.Errorf("appending: %w", err)
	}
	// Before releasing the lock: this is the durable artifact.
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return end + int64(buf.Len()), nil
}

// truncateTornBatch removes a trailing tx run that has no commit marker — a
// batch whose write was cut short after at least one complete line. Readers
// already ignore it (EVENTS.md §5.5); removing it is what keeps the next append
// from leaving it mid-log, where an older reader would apply it. Safe only
// under the exclusive lock, and only after truncatePartial, so the file ends on
// a line boundary.
func truncateTornBatch(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	end, _, err := committedTail(f.ReadAt, size)
	if err != nil {
		return 0, err
	}
	if end < size {
		if err := f.Truncate(end); err != nil {
			return 0, fmt.Errorf("truncating torn batch: %w", err)
		}
	}
	return end, nil
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
