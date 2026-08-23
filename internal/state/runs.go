package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// runs.json — the run records of SPEC §10.3, plus the one thing a reader in
// another process cannot get anywhere else: evidence that the daemon which
// wrote them is still there.

// Runs is runs.json in full.
type Runs struct {
	Daemon  Daemon `json:"daemon"`
	Records []Run  `json:"records"`
}

// Daemon is the writer's own account of itself.
type Daemon struct {
	// ID is the orchestrator's daemon identity — the same string that is the
	// actor on every §9.11 entry, so a log line, a transition and this file can
	// be tied to one process.
	ID string `json:"id"`
	// Workflow is the §5 workflow key this directory belongs to.
	Workflow string `json:"workflow"`
	PID      int    `json:"pid"`

	StartedAt time.Time `json:"started_at"`
	// WrittenAt is refreshed every HeartbeatMS whether or not anything changed,
	// and that is the whole point of it.
	//
	// A daemon that died leaves its last runs.json behind, and a stopped daemon
	// with three parked issues is byte-identical to a running one with the same
	// three. The alternative reading — probe the pid — is a guess the OS is free
	// to invalidate by reusing it, which is the same mistake core.RunEvidence
	// exists to avoid (§9.10). A heartbeat answers instead from positive
	// evidence, at a resolution the file states rather than the reader assumes.
	WrittenAt   time.Time `json:"written_at"`
	HeartbeatMS int       `json:"heartbeat_ms"`

	// Draining is SPEC §9.8's shutdown: dispatch has stopped and the daemon is
	// waiting on in-flight runs. An operator watching a slow stop wants this,
	// because from outside it is indistinguishable from a daemon that is idle.
	Draining bool `json:"draining"`
	// HeldClaims counts the retained `done` claims being swept (§9.8). They own
	// no run record — the run is over — so nothing else here would show them.
	HeldClaims int `json:"held_claims"`
	// Stopped marks the last write of a daemon that exited under its own
	// control, at WrittenAt.
	//
	// It exists alongside the heartbeat because the two answer for different
	// deaths. A graceful exit can say so positively and instantly rather than
	// waiting to look stale; a `kill -9` writes nothing at all, which is what
	// leaves the heartbeat as the only evidence. A reader needs both, and a
	// reader that treats Stopped as the only signal will report a killed daemon
	// as running forever.
	Stopped bool `json:"stopped"`
}

// Stale reports whether the heartbeat has not landed within the tolerance the
// file itself declares, meaning the daemon that wrote it is no longer writing.
//
// The tolerance is generous on purpose: a missed beat is a busy host, and
// crying "dead" at one is how a status surface teaches people to ignore it.
// grace is added to the declared interval, so a caller states its own patience
// rather than inheriting one from here.
func (d Daemon) Stale(now time.Time, grace time.Duration) bool {
	if d.HeartbeatMS <= 0 {
		// A writer that declared no interval promised no heartbeat, so its
		// silence is not evidence of anything.
		return false
	}
	return now.Sub(d.WrittenAt) > time.Duration(d.HeartbeatMS)*time.Millisecond+grace
}

// Run is one issue's record: SPEC §9.1's per-issue state, as a reader in
// another process sees it.
type Run struct {
	Issue string `json:"issue"`
	// RunID is the current attempt's correlation handle — the same value the
	// daemon's log lines carry (§10.3) and the child sees as BEN_RUN_ID (§7.6).
	RunID string `json:"run_id,omitempty"`
	// State is one of the nine §9.2 states. Carried as a string because the
	// enum is the orchestrator's, and a file format that imports the authority
	// loop makes the loop unable to ever import the file format.
	State   string `json:"state"`
	Attempt int    `json:"attempt"`
	Turns   int    `json:"turns"`
	// FailureReason is the §7.3 verdict of this record's most recent failure. It
	// is sticky across a retry, exactly as the record's own field is: it says
	// what last went wrong, not what the current state is.
	FailureReason core.FailureReason `json:"failure_reason,omitempty"`
	Branch        string             `json:"branch,omitempty"`
	UpdatedAt     time.Time          `json:"updated_at"`

	// NextTimerAt and NextTimer are the §9.6 wake-up this record is waiting on:
	// a backoff delay or a continuation re-check. §10.3 names "next backoff
	// timers" as a thing `ben status` shows, and it is the one field here that
	// answers "is this stuck, or is it waiting?" — the question a parked-looking
	// daemon actually raises.
	NextTimerAt *time.Time `json:"next_timer_at,omitempty"`
	NextTimer   string     `json:"next_timer,omitempty"`

	// Continuation is the adapter-opaque resume token for the next attempt
	// (§9.6), and §10.3's fourth state-dir item.
	//
	// It is written here and **not** published by `ben status`, which reports
	// only that a session will be resumed. The file is 0600 under a 0700
	// directory and belongs to the daemon; the command's output is a stable
	// contract and the thing an operator pastes into an issue, and what a given
	// provider's session identifier grants is not something BEN can know for
	// every adapter it will ever have. §10.3 does put the session id on every
	// log line, and that asymmetry is deliberate: journald sits behind the
	// supervisor's access control.
	//
	// The separation is structural rather than a rule applied at render time —
	// `ben status` has its own presentation types and this field is absent from
	// them (see cmd/ben/status.go).
	Continuation string `json:"continuation,omitempty"`
	// SessionID is the agent session this attempt announced (§7.4), and §10.3's
	// third correlation attribute. Both current adapters set it to the same
	// string as Continuation; core.Event keeps the two apart, so this does too.
	SessionID string `json:"session_id,omitempty"`
}

// Resuming reports whether the next attempt would carry a continuation token.
func (r Run) Resuming() bool { return r.Continuation != "" }

// WriteRuns replaces runs.json.
//
// The whole file is rewritten every time, which is what makes the rename atomic
// swap available: a reader in another process gets one daemon's consistent view
// of every run, or the previous one, and never a mixture with half the records
// updated (SPEC §11 — read-only, while the daemon runs).
func (d Dir) WriteRuns(r Runs) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encoding run records: %w", err)
	}
	body = append(body, '\n')
	if err := atomicWrite(d.RunsPath(), body); err != nil {
		return fmt.Errorf("state: writing run records: %w", err)
	}
	return nil
}

// ReadRuns reads runs.json back.
//
// Three outcomes, and keeping them apart is this function's job. A file that is
// not there is ErrNoState — no daemon has written this directory. A file that is
// there and does not parse is an error naming it, never an empty Runs: a
// truncated write and an idle daemon must not render the same, or `ben status`
// reports "nothing is running" about a daemon it simply failed to read. Only a
// file that parses is a verdict.
func (d Dir) ReadRuns() (Runs, error) {
	raw, err := os.ReadFile(d.RunsPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Runs{}, fmt.Errorf("%w at %s", ErrNoState, d.RunsPath())
	case err != nil:
		return Runs{}, fmt.Errorf("state: reading run records: %w", err)
	}
	var out Runs
	// DisallowUnknownFields is deliberately *not* set. This file is read by a
	// `ben status` that may be older than the daemon that wrote it — a rolling
	// upgrade, or an operator's older binary on the same host — and refusing to
	// render a status file because it gained a field is a worse failure than
	// rendering the fields both versions agree on.
	if err := json.Unmarshal(raw, &out); err != nil {
		return Runs{}, fmt.Errorf("state: %s is present but unreadable (a torn write, or not a BEN state file): %w", d.RunsPath(), err)
	}
	return out, nil
}
