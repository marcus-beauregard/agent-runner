package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ErrLedgerClosed reports an append after Close. The stream is finished at that
// point, and extending it would put events after the run's end.
var ErrLedgerClosed = errors.New("ledger closed")

// Event is one line of run.ndjson. Field order is fixed by struct order rather
// than a map, so every line of a run lists its fields the same way. Data is
// handed through untouched.
type Event struct {
	Seq   int64           `json:"seq"`             // increases across a run, including after a resume
	TS    time.Time       `json:"ts"`              // UTC
	Kind  string          `json:"kind"`            // "stage", "tool", "decision", "gate", "trigger", "state"
	Stage string          `json:"stage,omitempty"` // the stage an event belongs to, if any
	Data  json.RawMessage `json:"data"`            // the payload
}

// Ledger appends events to a run's ndjson file. It is safe for concurrent use:
// one mutex serialises appends and assigns Seq, so two goroutines never
// interleave half-lines and the order on disk is the order of the sequence
// numbers.
type Ledger struct {
	mu     sync.Mutex
	w      *os.File
	seq    int64
	broken error // first failed write; after it the stream cannot be trusted
	path   string
}

// OpenLedger opens path for appending, creating it 0600 if it does not exist,
// and continues the numbering of a file it finds. An existing file is never
// truncated to nothing: the stream is append-only, and a run that resumes keeps
// counting where the process that died left off.
//
// A torn final line is the one thing a crash can leave behind. It held no
// complete event, and leaving it in place would glue the next append onto its
// debris, so it is cut back to the last newline. Every line in the file then
// parses, which is the promise readers are written against.
func OpenLedger(path string) (*Ledger, error) {
	// Read-write, because continuing a file means reading its last line.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	if err := privateFile(f); err != nil {
		f.Close()
		return nil, err
	}
	seq, err := recoverSeq(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &Ledger{w: f, seq: seq, path: path}, nil
}

// Append writes one event and returns once it is on disk. The line is a single
// write of one buffer, so a crash can only cut the line being written; every
// line before it stays whole. data must be JSON-encodable; an encoding error
// writes nothing and consumes no sequence number.
func (l *Ledger) Append(kind, stage string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode event data: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(kind, stage, raw)
}

// appendLocked writes one event line and returns once it is on disk. The caller
// holds l.mu. Splitting it out lets a caller that has to read the stream before
// extending it — a trigger spend — do both without another append landing in
// between.
func (l *Ledger) appendLocked(kind, stage string, raw json.RawMessage) error {
	if l.w == nil {
		return ErrLedgerClosed
	}
	if l.broken != nil {
		return fmt.Errorf("ledger unusable after a failed write: %w", l.broken)
	}
	next := l.seq + 1
	line, err := json.Marshal(Event{
		Seq:   next,
		TS:    time.Now().UTC(),
		Kind:  kind,
		Stage: stage,
		Data:  raw,
	})
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	line = append(line, '\n')
	if _, err := l.w.Write(line); err != nil {
		// A write that failed part-way leaves a fragment with no newline. A
		// later append would not start a line of its own, it would finish that
		// fragment into an unparseable line in the middle of the stream, so the
		// ledger stops being usable here instead.
		l.broken = err
		return fmt.Errorf("write event: %w", err)
	}
	// Seq advances once the line exists on disk, even if the sync below fails:
	// the line is there, and reusing its number would make the stream lie.
	l.seq = next
	if err := l.w.Sync(); err != nil {
		return fmt.Errorf("sync event: %w", err)
	}
	return nil
}

// Close closes the file, ending the stream. It is safe to call twice, and an
// Append after it returns ErrLedgerClosed rather than writing to a closed
// descriptor.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return nil
	}
	f := l.w
	l.w = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("close ledger %s: %w", l.path, err)
	}
	return nil
}

// recoverSeq returns the sequence number of the file's last complete line, after
// cutting back a torn tail. Reading only happens to find that number: nothing
// here interprets an event, and a whole-file scan belongs to the report and
// replay commands.
func recoverSeq(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat ledger: %w", err)
	}
	size := fi.Size()
	if size == 0 {
		return 0, nil
	}

	// Walk back until the last complete line and the newline before it are both
	// in the buffer. Lines are payload-sized, so a small window usually holds
	// two; growing to the start of the file handles one enormous line.
	const chunk = 4096
	var buf []byte
	for start := size; start > 0; {
		begin := start - chunk
		if begin < 0 {
			begin = 0
		}
		part := make([]byte, start-begin)
		if _, err := f.ReadAt(part, begin); err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("read ledger tail: %w", err)
		}
		buf = append(part, buf...)
		start = begin
		if bytes.Count(buf, []byte("\n")) >= 2 {
			break
		}
	}

	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		// No complete line at all: everything in the file is a torn write.
		if err := f.Truncate(0); err != nil {
			return 0, fmt.Errorf("drop torn ledger: %w", err)
		}
		return 0, nil
	}
	if end := int64(len(buf) - last - 1); end > 0 {
		if err := f.Truncate(size - end); err != nil {
			return 0, fmt.Errorf("drop torn ledger tail: %w", err)
		}
		buf = buf[:last+1]
	}

	lineEnd := last
	lineStart := bytes.LastIndexByte(buf[:lineEnd], '\n') + 1
	var e Event
	if err := json.Unmarshal(buf[lineStart:lineEnd], &e); err != nil {
		return 0, fmt.Errorf("last line is not an event: %w", err)
	}
	return e.Seq, nil
}

// privateFile tightens a ledger whose permissions are looser than 0600. The
// record holds paths and reasoning, and a file created by an earlier process is
// not automatically private just because this one would have created it so.
func privateFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat ledger: %w", err)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("tighten ledger: %w", err)
	}
	return nil
}
