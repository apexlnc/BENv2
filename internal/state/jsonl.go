package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// The append-only machinery shared by every JSONL log in the state dir: the
// §9.11 transition log (transitions.go) and the attempt-outcome log
// (attempts.go).
//
// One implementation, deliberately. Everything here is a property that is only
// worth having if it is *always* had — a torn tail repaired before the next
// append, a failed write that leaves nothing behind, a reader that tells a
// trailing fragment from a hole — and a second copy written for the second log
// would be the copy that got one of them subtly wrong. Which is the worse
// failure of the two: a log whose durability is asserted rather than
// implemented reads as evidence right up to the crash it was for.
//
// `what` threads through the messages so each log names itself in an operator's
// error. It is a noun, singular, lowercase ("transition", "attempt outcome").

// maxRecordBytes bounds one log line, on both sides: the writer refuses to
// write a longer record and the reader refuses to read one.
//
// Both halves are needed and they are needed together. A trigger or detail
// string is operator-facing text built from an error, so its length is not
// something this package controls; without a bound the reader would allocate
// whatever a malformed or hostile file claims one line is. Bounding only the
// reader would instead let the writer produce a log it cannot itself read back,
// which is the shape of failure that only shows up during recovery.
const maxRecordBytes = 64 << 10

// ErrLogUnwritable marks a log this process can no longer append to safely.
//
// It is distinct from an ordinary append failure because it means something
// different to the caller: an ordinary failure is worth retrying, and this is
// not — every later attempt would append past a fragment. A caller that retries
// on it turns one bad write into a spin.
var ErrLogUnwritable = errors.New("state: the log can no longer be appended to")

// appendLog holds one log open for appending. One per daemon per log; each
// holds its file open for the daemon's life because records are rare and
// reopening per append would trade nothing for a wider window in which the path
// can move.
type appendLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	what string
	// offset is where the file ends, tracked so a failed append can roll back to
	// it without asking the filesystem that just failed.
	offset int64
	// broken latches a rollback that could not complete. See rollback.
	broken error
	// failAfterWrite injects a post-write failure. Tests only; see
	// injectedFailure.
	failAfterWrite func() error
	// Repaired is how many bytes of an incomplete trailing record this open
	// discarded. Non-zero means the previous daemon died mid-append. Reported
	// rather than swallowed: it is the one thing here that says a process was
	// killed rather than stopped, and the assembly logs it.
	Repaired int64
}

// openAppendLog opens a log for appending, creating it if needed, after
// repairing an incomplete trailing record.
//
// **The repair is the point, and it is not tidiness.** A reader tolerates a
// trailing fragment — it is the ordinary shape of a file being appended to. A
// *writer* cannot: O_APPEND starts the next record at the fragment's end, so the
// two become one line that parses as neither. That line is permanent. Every
// later read fails on it, which means one crash mid-write costs not the record
// it was writing but the whole log — including, for transitions, the §9.10 step 6
// reasons a restart is exactly when anything wants.
//
// So the fragment is truncated away before the file is written to again. It was
// never a record — the crash took its durability with it — and dropping it is
// the same verdict the reader already reaches, applied where it has to be
// applied to stay true.
func openAppendLog(path, what string) (*appendLog, error) {
	// The repair takes a handle of its own, and the append handle is opened
	// afterwards. Truncating needs O_RDWR and a real offset; appending needs
	// O_APPEND, whose whole value is that the seek and the write are one step.
	// One descriptor cannot be both without giving up the second, so there are
	// two, in that order.
	repaired, err := repairTail(path, what)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: opening the %s log: %w", what, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // the stat error is the one that matters
		return nil, fmt.Errorf("state: sizing the %s log: %w", what, err)
	}
	return &appendLog{f: f, path: path, what: what, offset: fi.Size(), Repaired: repaired}, nil
}

// repairTail truncates an incomplete trailing record, returning how much it
// dropped.
func repairTail(path, what string) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("state: opening the %s log: %w", what, err)
	}
	defer f.Close() //nolint:errcheck // Truncate and Sync reported what mattered

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("state: sizing the %s log: %w", what, err)
	}
	size := fi.Size()
	if size == 0 {
		return 0, nil
	}

	// A complete record is at most maxRecordBytes, so a window that size is
	// enough to find the newline ending the last one — if the tail is a legal
	// fragment at all.
	window := int64(maxRecordBytes)
	if window > size {
		window = size
	}
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, size-window); err != nil {
		return 0, fmt.Errorf("state: reading the tail of the %s log: %w", what, err)
	}
	if buf[len(buf)-1] == '\n' {
		return 0, nil // the last record is complete: the ordinary case
	}

	keep := int64(-1)
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		keep = size - window + int64(i) + 1
	} else if window == size {
		// No newline anywhere in a file shorter than one record: the whole thing
		// is a fragment.
		keep = 0
	}
	if keep < 0 {
		// The tail runs longer than any record this package will write, so it is
		// not a fragment of one. Refuse rather than guess how much to discard:
		// this is somebody else's file, or damage of a kind truncation would
		// compound. Moving it aside is an operator's decision, not ours.
		return 0, fmt.Errorf("state: %s ends with %d bytes containing no record boundary; refusing to append to it", path, window)
	}

	if err := f.Truncate(keep); err != nil {
		return 0, fmt.Errorf("state: repairing the %s log: %w", what, err)
	}
	// Durable before anything is appended, for the reason the repair exists: a
	// truncation still in the page cache when the next crash lands leaves the
	// fragment behind with new records written after it.
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("state: syncing the repaired %s log: %w", what, err)
	}
	return size - keep, nil
}

// appendRecord writes one already-encoded record durably, and either the record
// is in the file afterwards or nothing of it is. subject names the record in the
// over-size refusal — an issue identifier, for every log here.
//
// **That all-or-nothing is a contract, not an accident, because the caller
// retries.** A write can fail having put some bytes down, and a Sync can fail
// over bytes already written. Left alone, the first turns the retry into the
// glued-fragment corruption openAppendLog exists to repair — recreated
// mid-process, where no reopen will find it — and the second writes the record
// twice. So a failed append truncates back to where the file stood before it,
// and "error" therefore means "not appended" to everyone above.
//
// One write of one complete record, newline included: O_APPEND makes the offset
// and the write a single step, so a reader in another process sees whole records
// and a trailing partial one, never interleaved halves of two.
//
// It syncs. That is a real cost per record and it buys the only thing these
// files are for — a fact that survives the crash, rather than one still in the
// page cache when the machine goes down, which is exactly the case §9.10 step 6's
// reader exists to serve. Records are seconds apart at their densest; the launch
// each one accompanies costs orders of magnitude more.
func (w *appendLog) appendRecord(body []byte, subject string) error {
	if len(body)+1 > maxRecordBytes {
		return fmt.Errorf("state: %s record for %s is %d bytes, over the %d-byte limit", w.what, subject, len(body)+1, maxRecordBytes)
	}
	body = append(body, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return fmt.Errorf("state: the %s log is closed", w.what)
	}
	if w.broken != nil {
		return w.broken
	}

	// The offset is tracked rather than stat-ed: it is what the rollback needs,
	// and asking the filesystem for it after a failed write asks the thing that
	// just failed.
	before := w.offset
	n, err := w.f.Write(body)
	w.offset += int64(n)
	if err == nil {
		err = w.injectedFailure()
	}
	if err == nil {
		err = w.f.Sync()
	}
	if err != nil {
		return w.rollback(before, err)
	}
	return nil
}

// rollback undoes a partial or unconfirmed append so the caller's retry starts
// from a clean end-of-file.
//
// Truncating after a *Sync* failure can discard bytes that did reach the disk.
// That is the intended direction: the record is pending in memory and will be
// written again, so a discarded duplicate costs nothing while a kept one is a
// record the log claims happened twice.
//
// If the truncation itself fails there is a fragment in the file and no way to
// remove it, so the writer refuses everything afterwards. Appending past a
// fragment is the corruption this whole path exists to prevent, and doing it
// knowingly would be worse than the crash that motivated the repair.
func (w *appendLog) rollback(to int64, cause error) error {
	if err := w.f.Truncate(to); err != nil {
		w.broken = fmt.Errorf("%w: %s left an incomplete record in %s that could not be removed (%v)",
			ErrLogUnwritable, cause, w.path, err)
		return w.broken
	}
	w.offset = to
	return fmt.Errorf("state: appending to the %s log: %w", w.what, cause)
}

// injectedFailure is the seam for testing the rollback. Production leaves it
// nil; there is no other way to make a real file fail after a real write.
func (w *appendLog) injectedFailure() error {
	if w.failAfterWrite == nil {
		return nil
	}
	return w.failAfterWrite()
}

// Close releases the file. Idempotent: shutdown paths call it more than once.
func (w *appendLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	if err != nil {
		return fmt.Errorf("state: closing the %s log: %w", w.what, err)
	}
	return nil
}

// walkLog visits every complete record in a log, in order, decoding each into T.
//
// It holds no handle between calls: a reader can outlive, precede, or run
// alongside the daemon that writes the file without either holding the other
// open.
//
// Two failure shapes, and they are not the same fault. A complete line that does
// not parse is corruption and an error: a log with a hole in it must not quietly
// answer questions about what is in the rest of it. A *trailing* incomplete line
// is not a record at all — it is the writer caught between its write and the
// reader's read, which is the ordinary case for a file read while a daemon runs
// (SPEC §11) — and is dropped. After a crash mid-append that drop is permanent,
// which makes it one more instance of §9.10 step 6's documented loss rather than
// a new one: the record it described is one whose own durability the crash took
// with it.
//
// Every record is decoded, including those a caller will discard. That is
// deliberate and it is the cost being paid here: it is what makes a corrupt
// record anywhere in the log loud rather than loud-only-if-recent.
func walkLog[T any](path, what string, visit func(T)) error {
	f, err := os.Open(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w at %s", ErrNoState, path)
	case err != nil:
		return fmt.Errorf("state: reading the %s log: %w", what, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	// ReadSlice rather than ReadString: it reports an over-long line as
	// ErrBufferFull instead of allocating it, so the bound is enforced before
	// the memory is spent rather than after. The returned slice is only valid
	// until the next read, which is why each record is decoded immediately.
	br := bufio.NewReaderSize(f, maxRecordBytes)
	for line := 1; ; line++ {
		raw, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			return fmt.Errorf("state: %s line %d exceeds the %d-byte record limit", path, line, maxRecordBytes)
		case errors.Is(err, io.EOF):
			// raw is whatever preceded EOF without a newline: the trailing
			// incomplete record, or nothing at all. Dropped either way.
			return nil
		case err != nil:
			return fmt.Errorf("state: reading the %s log: %w", what, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("state: %s line %d is not a %s record: %w", path, line, what, err)
		}
		visit(rec)
	}
}
