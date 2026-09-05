package remote_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// BEN's durable event inbox on disk. What is provable here is the contract
// DurableConsumer states, and one thing beyond it that only a file-backed
// implementation has: the index is rebuilt from the log, so a *second process*
// sees exactly what the first accepted.

func consumerRef(t *testing.T, run remote.RunID) remote.ProcessRef {
	t.Helper()
	identity := remote.Identity{
		Claim:     remote.Claim{Repository: "acme/widgets", Issue: "7", Epoch: 100},
		Branch:    "ben/issue-7",
		BaseSHA:   "1111111111111111111111111111111111111111",
		SandboxID: "sandbox-1", ProfileRevision: "rev-1",
	}
	return remote.ProcessRef{Identity: identity, RunID: run, RequestDigest: "sha256:abc"}
}

func consumption(id string, cursor int64, terminal bool, events ...core.Event) remote.Consumption {
	return remote.Consumption{
		ID:         id,
		Checkpoint: remote.Checkpoint{Cursor: remote.Cursor(cursor), Tail: []byte("part"), Terminal: terminal},
		Envelope:   &remote.Envelope{Seq: cursor, Stream: remote.StreamStdout, Payload: []byte("raw-" + id)},
		Events:     events,
	}
}

func TestDirConsumerAcceptsOnceAndReplaysIdentically(t *testing.T) {
	t.Parallel()
	c := remote.NewDirConsumer(t.TempDir())
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	first, err := c.Commit(ctx, ref, consumption("c1", 1, false))
	if err != nil || !first {
		t.Fatalf("first commit = %v, %v; want true", first, err)
	}
	// A replay is expected and deduplicated — the crash-between-Commit-and-cursor
	// window replays the same key by design.
	again, err := c.Commit(ctx, ref, consumption("c1", 1, false))
	if err != nil || again {
		t.Fatalf("replayed commit = %v, %v; want false with no error", again, err)
	}
	// A *different* payload under the same key is not a replay.
	changed := consumption("c1", 1, false)
	changed.Envelope.Payload = []byte("something else")
	if _, err := c.Commit(ctx, ref, changed); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("a changed replay = %v, want ErrEventConflict", err)
	}
}

// The terminal bit is sticky across distinct keys: later raw bytes may be
// retained, but no second normalized outcome may be accepted for a run whose
// first one has already been projected.
func TestDirConsumerKeepsTheTerminalOutcomeSticky(t *testing.T) {
	t.Parallel()
	c := remote.NewDirConsumer(t.TempDir())
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	if _, err := c.Commit(ctx, ref, consumption("c1", 1, true, core.Event{Type: core.EventSucceeded})); err != nil {
		t.Fatal(err)
	}
	// Raw bytes after the outcome: accepted, because a transcript keeps being
	// written after the outcome line.
	if _, err := c.Commit(ctx, ref, consumption("c2", 2, true)); err != nil {
		t.Fatalf("retaining raw bytes after a terminal outcome: %v", err)
	}
	// A second normalized outcome: refused.
	if _, err := c.Commit(ctx, ref, consumption("c3", 3, true, core.Event{Type: core.EventFailed})); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("a second outcome = %v, want ErrEventConflict", err)
	}
	// And so is a consumption that drops the bit.
	if _, err := c.Commit(ctx, ref, consumption("c4", 4, false)); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("a non-terminal consumption after the outcome = %v, want ErrEventConflict", err)
	}
}

// A later process reads back exactly what the first accepted, in commit order
// and with payloads intact. This is the property the whole file exists for: a
// terminal outcome accepted just before a crash is re-projected rather than lost.
func TestDirConsumerRecoversAcrossAProcess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	first := remote.NewDirConsumer(root)
	for _, item := range []remote.Consumption{
		consumption("c1", 1, false, core.Event{Type: core.EventStarted, SessionID: "s"}),
		consumption("c2", 2, false, core.Event{Type: core.EventProgress, Text: "working"}),
		consumption("c3", 3, true, core.Event{Type: core.EventSucceeded}),
	} {
		if _, err := first.Commit(ctx, ref, item); err != nil {
			t.Fatal(err)
		}
	}

	// A different value over the same directory: a restart, as far as this
	// implementation can tell.
	second := remote.NewDirConsumer(root)
	got, err := second.Recover(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("recovered %d consumptions, want 3", len(got))
	}
	for i, want := range []string{"c1", "c2", "c3"} {
		if got[i].ID != want {
			t.Fatalf("recovered %v out of order", got)
		}
	}
	if string(got[0].Envelope.Payload) != "raw-c1" {
		t.Fatalf("payloads did not survive: %q", got[0].Envelope.Payload)
	}
	if !got[2].Checkpoint.Terminal || got[2].Events[0].Type != core.EventSucceeded {
		t.Fatalf("the terminal outcome did not survive: %+v", got[2])
	}
	// The rebuilt index enforces the same rules the first process did.
	if _, err := second.Commit(ctx, ref, consumption("c4", 4, true, core.Event{Type: core.EventFailed})); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("the restarted consumer accepted a second outcome: %v", err)
	}
	if again, err := second.Commit(ctx, ref, consumption("c1", 1, false, core.Event{Type: core.EventStarted, SessionID: "s"})); err != nil || again {
		t.Fatalf("the restarted consumer treated a replay as new: %v, %v", again, err)
	}
}

// An accepted retention range survives to the next process, because it is the
// only thing that makes a sequence absent from the envelopes an accepted loss
// rather than evidence this log cannot be trusted.
//
// Anchored at the disk boundary rather than only through the pump: the record is
// carried by the on-disk encoding, so a field the writer forgets to persist
// would leave a recovery unable to tell the two apart while every in-memory test
// still passed.
func TestDirConsumerRetainsAnAcceptedRetentionRange(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	gap := remote.EventGap{From: 2, To: 4}
	accepted := remote.Consumption{
		ID:         "event-gap:2-4",
		Checkpoint: remote.Checkpoint{Cursor: 4, Terminal: true},
		Gap:        &gap,
		Events:     []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}},
	}
	first := remote.NewDirConsumer(root)
	if _, err := first.Commit(ctx, ref, consumption("c1", 1, false, core.Event{Type: core.EventStarted})); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx, ref, accepted); err != nil {
		t.Fatalf("committing an accepted retention range: %v", err)
	}

	second := remote.NewDirConsumer(root)
	got, err := second.Recover(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("recovered %d consumptions, want 2", len(got))
	}
	if got[1].Gap == nil || *got[1].Gap != gap {
		t.Fatalf("recovered gap record = %v, want %v", got[1].Gap, gap)
	}
	if got[1].Envelope != nil {
		t.Errorf("an accepted range carries envelope %+v; there are no bytes for a range nobody read", got[1].Envelope)
	}
	// The replayed range is a replay, not a second acceptance.
	if again, err := second.Commit(ctx, ref, accepted); err != nil || again {
		t.Fatalf("the restarted consumer treated a replayed range as new: %v, %v", again, err)
	}
	// And the range does not soften the sticky terminal bit it was committed with.
	if _, err := second.Commit(ctx, ref, consumption("c3", 5, true, core.Event{Type: core.EventSucceeded})); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("a success after an accepted range = %v, want ErrEventConflict", err)
	}
}

// Two runs of one claim keep separate logs: the reference is the key, so a
// retry's inbox cannot be answered with the previous attempt's evidence.
func TestDirConsumerSeparatesRunsOfOneClaim(t *testing.T) {
	t.Parallel()
	c := remote.NewDirConsumer(t.TempDir())
	ctx := context.Background()
	one, two := consumerRef(t, "run-1"), consumerRef(t, "run-2")

	if _, err := c.Commit(ctx, one, consumption("c1", 1, true, core.Event{Type: core.EventSucceeded})); err != nil {
		t.Fatal(err)
	}
	// The second run's inbox is empty and its own terminal bit is unset.
	got, err := c.Recover(ctx, two)
	if err != nil || len(got) != 0 {
		t.Fatalf("Recover for a second run = %v, %v; want empty", got, err)
	}
	if _, err := c.Commit(ctx, two, consumption("c1", 1, true, core.Event{Type: core.EventSucceeded})); err != nil {
		t.Fatalf("the second run inherited the first's terminal bit: %v", err)
	}
}

// Discard removes a finished run's log, and a repeated cleanup after a crash
// must not fail.
func TestDirConsumerDiscardsAFinishedRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := remote.NewDirConsumer(root)
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	if _, err := c.Commit(ctx, ref, consumption("c1", 1, true, core.Event{Type: core.EventSucceeded})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Path(ref)); err != nil {
		t.Fatalf("the log was not written: %v", err)
	}
	if err := c.Discard(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Path(ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the log survived the discard: %v", err)
	}
	if err := c.Discard(ctx, ref); err != nil {
		t.Fatalf("a repeated discard: %v", err)
	}
	// And the index went with it, so a reused reference starts clean.
	got, err := c.Recover(ctx, ref)
	if err != nil || len(got) != 0 {
		t.Fatalf("Recover after a discard = %v, %v; want empty", got, err)
	}
}

// A log that cannot be parsed is an error, never a truncation. Everything in it
// is evidence BEN has already acted on: dropping a record would let a replayed
// sequence pass, and dropping a terminal one would let a second outcome through.
func TestDirConsumerRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := remote.NewDirConsumer(root)
	ref := consumerRef(t, "run-1")
	ctx := context.Background()

	if _, err := c.Commit(ctx, ref, consumption("c1", 1, false)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(c.Path(ref))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(ref), append(body, []byte("{not json\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	second := remote.NewDirConsumer(root)
	if _, err := second.Recover(ctx, ref); err == nil {
		t.Fatal("a corrupt log was read as a shorter one")
	}
	if _, err := second.Commit(ctx, ref, consumption("c2", 2, false)); err == nil {
		t.Fatal("a corrupt log accepted a further commit")
	}
}

// An incomplete reference has no address to file evidence under.
func TestDirConsumerRefusesAnIncompleteReference(t *testing.T) {
	t.Parallel()
	c := remote.NewDirConsumer(t.TempDir())
	ctx := context.Background()
	if _, err := c.Commit(ctx, remote.ProcessRef{}, consumption("c1", 1, false)); !errors.Is(err, remote.ErrProcessMismatch) {
		t.Fatalf("an incomplete reference = %v, want ErrProcessMismatch", err)
	}
	ref := consumerRef(t, "run-1")
	if _, err := c.Commit(ctx, ref, remote.Consumption{}); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("a consumption with no key = %v, want ErrEventConflict", err)
	}
	// The log lands under the root it was named with, and nowhere else.
	if rel, err := filepath.Rel(c.Root(), c.Path(ref)); err != nil || rel == "" || rel[0] == '.' {
		t.Fatalf("the log path %q escapes the root %q", c.Path(ref), c.Root())
	}
}
