package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// DirConsumer is BEN's durable event inbox on disk: the production
// DurableConsumer the daemon commits a remote run's evidence to before its
// cursor may advance (#205).
//
// One append-only file per ProcessRef, one whole record per line, fsynced before
// Commit returns. The shape is internal/state's jsonl writer's and it is chosen
// for the same reasons:
//
//   - **Append, not replace.** A consumption is evidence that has already
//     happened, and the file is a log of them in commit order — which is exactly
//     what Recover has to reproduce. Rewriting the whole set on every event would
//     also make the cost of a chatty run quadratic in its own output.
//   - **One record per line, written whole.** A reader — this process after a
//     restart, or a human — sees complete records or nothing. A torn one would be
//     a checkpoint nobody can parse, and the cursor it carries is what decides
//     which backend events BEN thinks it has seen.
//
// The idempotency index is held in memory and rebuilt from the file on first
// touch. That is safe because exactly one daemon writes a state directory
// (SPEC §10.1's single claim principal), and it is what keeps Commit from
// re-reading its own log on every event.
type DirConsumer struct {
	root string

	mu   sync.Mutex
	refs map[string]*consumerLog
}

// NewDirConsumer names a directory. It is created on first write, so a daemon
// whose remote path is never reached leaves no tree behind.
func NewDirConsumer(root string) *DirConsumer {
	return &DirConsumer{root: root, refs: map[string]*consumerLog{}}
}

// Root is the directory itself.
func (c *DirConsumer) Root() string { return c.root }

// Path is where one run's consumption log lives. Readable prefix plus a digest
// of the exact reference, for DirStore.Path's reason.
func (c *DirConsumer) Path(ref ProcessRef) string {
	name := claimFilename(ref.Identity.Claim) + "-events-" + shortDigest(ref.String()) + ".jsonl"
	return filepath.Join(c.root, "events", name)
}

// consumerLog is one reference's in-memory index over its file.
type consumerLog struct {
	loaded bool
	// seen maps a consumption id to the canonical bytes accepted under it, so a
	// replay of the same key with different content is a conflict rather than a
	// silent overwrite.
	seen map[string][]byte
	// order is every accepted consumption, in commit order — what Recover hands
	// back.
	order []Consumption
	// terminal is sticky: once a normalized outcome has been accepted, no second
	// one may be, even across a lost local checkpoint (DurableConsumer).
	terminal bool
}

// Commit durably retains one consumption and reports whether it is new.
func (c *DirConsumer) Commit(ctx context.Context, ref ProcessRef, item Consumption) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !ref.Complete() {
		return false, fmt.Errorf("%w: consumption for an incomplete process reference", ErrProcessMismatch)
	}
	if item.ID == "" {
		return false, fmt.Errorf("%w: consumption has no idempotency key", ErrEventConflict)
	}
	body, err := json.Marshal(item)
	if err != nil {
		return false, fmt.Errorf("remote: encoding consumption %s: %w", item.ID, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	log, err := c.logFor(ref)
	if err != nil {
		return false, err
	}
	if previous, ok := log.seen[item.ID]; ok {
		if !bytes.Equal(previous, body) {
			return false, fmt.Errorf("%w: consumption %s", ErrEventConflict, item.ID)
		}
		return false, nil
	}
	// The terminal bit is sticky across distinct keys. Later raw bytes may still
	// be retained — a transcript keeps being written after the outcome line — but
	// a second normalized outcome must never be accepted, because the first one
	// has already been projected and the claim routed on it.
	if log.terminal && (!item.Checkpoint.Terminal || len(item.Events) != 0) {
		return false, fmt.Errorf("%w: consumption %s follows a terminal outcome", ErrEventConflict, item.ID)
	}
	if err := appendLine(c.Path(ref), body); err != nil {
		return false, err
	}
	log.seen[item.ID] = body
	log.order = append(log.order, cloneConsumption(item))
	if item.Checkpoint.Terminal {
		log.terminal = true
	}
	return true, nil
}

// Recover returns every accepted consumption for a reference in commit order.
func (c *DirConsumer) Recover(ctx context.Context, ref ProcessRef) ([]Consumption, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	log, err := c.logFor(ref)
	if err != nil {
		return nil, err
	}
	out := make([]Consumption, 0, len(log.order))
	for _, item := range log.order {
		out = append(out, cloneConsumption(item))
	}
	return out, nil
}

// Discard removes a reference's log. Called once the run's journal is closed —
// never before, because a log removed over a possibly-live run is the evidence a
// restart would have redelivered.
//
// Absent is not an error, for Store.Delete's reason: a repeated cleanup after a
// crash must not fail.
func (c *DirConsumer) Discard(ctx context.Context, ref ProcessRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.refs, ref.String())
	if err := os.Remove(c.Path(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remote: removing the consumption log for %s: %w", ref, err)
	}
	return nil
}

// logFor returns the in-memory index, reading the file the first time.
//
// A decode failure is an error rather than a truncation. Everything in this file
// is evidence BEN has already acted on: dropping a record would let a replayed
// sequence pass the conflict check, and dropping a *terminal* one would let a
// second outcome be accepted for a run already routed.
func (c *DirConsumer) logFor(ref ProcessRef) (*consumerLog, error) {
	key := ref.String()
	if log, ok := c.refs[key]; ok && log.loaded {
		return log, nil
	}
	log := &consumerLog{loaded: true, seen: map[string][]byte{}}
	file, err := os.Open(c.Path(ref))
	switch {
	case errors.Is(err, os.ErrNotExist):
		c.refs[key] = log
		return log, nil
	case err != nil:
		return nil, fmt.Errorf("remote: reading the consumption log for %s: %w", ref, err)
	}
	defer file.Close() //nolint:errcheck // read-only handle

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), consumerLineLimit)
	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var item Consumption
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("remote: consumption log %s line %d: %w", c.Path(ref), line, err)
		}
		body, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("remote: re-encoding consumption %s: %w", item.ID, err)
		}
		log.seen[item.ID] = body
		log.order = append(log.order, item)
		if item.Checkpoint.Terminal {
			log.terminal = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remote: reading the consumption log for %s: %w", ref, err)
	}
	c.refs[key] = log
	return log, nil
}

// consumerLineLimit bounds one record, mirroring the framing ceiling an
// unterminated provider line already has (ErrFrameTooLarge). A payload larger
// than this cannot have been admitted in the first place.
const consumerLineLimit = 8 << 20

// appendLine writes one whole record and fsyncs before returning. The write is a
// single Write call so the kernel does not interleave it with another, and the
// sync is what makes "durably retained before Commit returns" true rather than
// hoped for.
func appendLine(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	line := append(append([]byte(nil), body...), '\n')
	if _, err := file.Write(line); err != nil {
		file.Close() //nolint:errcheck // the write error is the one that matters
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close() //nolint:errcheck // as above
		return err
	}
	return file.Close()
}

func cloneConsumption(item Consumption) Consumption {
	out := item
	out.Checkpoint.Tail = append([]byte(nil), item.Checkpoint.Tail...)
	if item.Envelope != nil {
		envelope := *item.Envelope
		envelope.Payload = append([]byte(nil), item.Envelope.Payload...)
		out.Envelope = &envelope
	}
	if item.Gap != nil {
		gap := *item.Gap
		out.Gap = &gap
	}
	out.Events = append([]core.Event(nil), item.Events...)
	return out
}
