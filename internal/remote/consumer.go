package remote

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Checkpoint is the complete decoder state after one consumption. Tail is the
// unterminated stdout line; it must move atomically with Cursor or a restart can
// parse a split provider record differently from the first process.
type Checkpoint struct {
	Cursor   Cursor `json:"cursor"`
	Tail     []byte `json:"tail,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
}

// Consumption is the unit handed to BEN's durable event consumer. ID is stable
// across replay. Envelope is nil only for a locally derived stream-sealed,
// transport-error or retention-gap outcome.
type Consumption struct {
	ID         string
	Checkpoint Checkpoint
	Envelope   *Envelope
	Events     []core.Event
	// Gap is the closed range of backend sequences this consumption accepted as
	// permanently unavailable, and is set only by a measured retention expiry
	// (RetentionGap). It carries no envelope and no bytes, because there are
	// none: it is the record that BEN advanced its cursor over sequences it
	// never read, which is the one thing a cursor otherwise cannot say. Without
	// it a later recovery could not tell an accepted discontinuity from local
	// history that lost records.
	Gap *EventGap
}

// DurableConsumer is the acknowledgement boundary missing from a RunHandle's
// receive-only channel. Commit must durably retain the raw envelope/transcript,
// normalized events, and checkpoint before returning. Recover is the other half
// of that contract: a later daemon reads the accepted consequences back before
// it resumes the backend stream. Without it, a crash after Commit and before a
// channel send can turn a durable succeeded outcome into failed(crashed).
//
// Commit is idempotent on (ProcessRef, ID): first is true only for the first
// identical commit, and the same key with different content must return
// ErrEventConflict.
// Once a checkpoint with Terminal is accepted, a later distinct consumption
// may retain additional raw envelopes only when it carries no normalized events
// and keeps Terminal set. This makes the terminal bit sticky even if the local
// attach journal is lost before it can mirror the consumer's checkpoint.
//
// Recover returns every accepted consumption for the exact ProcessRef in commit
// order, with payloads intact, until that process journal is closed. Retaining
// the envelopes is load-bearing twice: Attempt redelivers normalized events
// after a daemon crash, and Sequencer rebuilds the digest history that makes a
// changed replay at or below the durable cursor fail closed. A Gap is retained
// for the same reason and completes that history: it is the only thing that
// makes a sequence absent from the envelopes an accepted loss rather than
// evidence this log cannot be trusted. Implementations may store transcript
// bytes separately, but Recover must reconstruct the same Consumption values
// Commit accepted.
//
// Attempt advances its backend cursor only after Commit. Events() is therefore
// an at-least-once live projection of already-durable work, never the evidence
// that authorizes a cursor advance. Orchestrator recovery must tolerate replay
// of a consequence it had already applied before the daemon stopped.
type DurableConsumer interface {
	Commit(ctx context.Context, ref ProcessRef, c Consumption) (first bool, err error)
	Recover(ctx context.Context, ref ProcessRef) ([]Consumption, error)
}
