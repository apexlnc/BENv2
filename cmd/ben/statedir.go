package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The daemon's whole relationship with the §10.3 state dir, and the assembly's
// share of §9.11: the loop and the file format do not import each other, so the
// translation between them lives here, where components that may not import
// each other are bound together (SPEC §11).

// The two cadences runs.json is written on, and they answer different
// questions.
//
// refresh is how soon a change can reach the file — the lag between the loop
// moving and `ben status` being able to see it. heartbeat is how often the file
// is rewritten when *nothing* has changed, which is what makes its freshness
// evidence that the daemon is still there rather than merely evidence that it
// once was.
//
// Both are needed. A heartbeat alone makes every status read up to one interval
// stale, and lowering it to fix that would fsync a file several times a second
// for the lifetime of a daemon that is mostly idle. Change detection alone makes
// an idle daemon indistinguishable from a dead one, which is the reading this
// file exists to prevent.
const (
	refresh   = 250 * time.Millisecond
	heartbeat = 2 * time.Second
)

// stateFiles is the state dir as the daemon uses it: the sink the §9.11 log
// appends through, the run records published on the heartbeat, and the lookup
// the logger correlates through.
type stateFiles struct {
	dir      state.Dir
	key      string
	writer   *state.TransitionWriter
	attempts *state.AttemptWriter
	lookup   *runLookup

	stop func()
}

// openStateFiles prepares the directory and opens the transition log.
//
// Refusing to start when this fails is deliberate, and it is the one place in
// this package where a state-dir failure is fatal. Everything the directory
// holds is forensics or a §9.10 step 6 degradation, so a daemon could run
// without it — but a directory that cannot be created or a log that cannot be
// opened is a wrong path, a full disk or a permission fault, and each of those
// is about to break the transcripts and the worktrees too. Failing at startup
// names the problem while an operator is still watching.
func openStateFiles(dir state.Dir, workflowKey string, lookup *runLookup, log *slog.Logger) (*stateFiles, error) {
	if err := dir.Prepare(); err != nil {
		return nil, err
	}
	w, err := dir.AppendTransitions()
	if err != nil {
		return nil, err
	}
	if w.Repaired > 0 {
		// The one thing in the state dir that says the previous daemon was
		// killed rather than stopped. Reported at startup because that is when
		// somebody is looking, and because §9.10's recovery is about to
		// reconstruct from a log this many bytes shorter than the daemon that
		// wrote it believed.
		log.Warn("repaired an incomplete record at the end of the transition log; the previous daemon did not stop cleanly",
			"bytes_discarded", w.Repaired, "path", dir.TransitionsPath())
	}
	a, err := dir.AppendAttempts()
	if err != nil {
		// Fatal on the same terms as the log above, and for the same reason: a
		// path that cannot be opened is a wrong directory, a full disk or a
		// permission fault, each of which is about to break the transcripts and
		// the worktrees too. That the *contents* are only telemetry does not make
		// the failure a lesser one — it is the same disk.
		w.Close() //nolint:errcheck // the open error is the one that matters
		return nil, err
	}
	if a.Repaired > 0 {
		log.Warn("repaired an incomplete record at the end of the attempt-outcome log; the previous daemon did not stop cleanly",
			"bytes_discarded", a.Repaired, "path", dir.AttemptsPath())
	}
	return &stateFiles{dir: dir, key: workflowKey, writer: w, attempts: a, lookup: lookup}, nil
}

// failures is what orchestrator.Config.FailureReasons takes: the §9.10 step 6
// read of the log this daemon and its predecessors write.
//
// state.TransitionReader satisfies the loop's seam directly — both spell the answer
// as core.RunFailure — so there is no conversion here, unlike the sink below. That
// asymmetry is deliberate: an entry has to be translated because the on-disk record
// is the state dir's shape, while "the last failure" is a domain fact core already
// names, and inventing a second spelling of it would put a mistranslation in the
// assembly, where nothing would see it.
func (s *stateFiles) failures() orchestrator.FailureReasonReader { return s.dir.ReadTransitions() }

// sink is what orchestrator.Config.Transitions takes.
func (s *stateFiles) sink() orchestrator.TransitionSink { return transitionSink{s.writer} }

// attemptSink is what orchestrator.Config.Attempts takes.
func (s *stateFiles) attemptSink() orchestrator.AttemptSink { return attemptSink{s.attempts} }

// transitionSink translates one §9.11 entry into its on-disk record.
//
// The two types are separate so that neither package imports the other: the
// state dir must not depend on the authority loop, because the loop is what will
// want to read a state dir (§9.10 step 6 already does), and a package cannot be
// on both ends of that. This conversion is the whole cost of keeping them apart,
// and it is the assembly's to pay.
type transitionSink struct{ w *state.TransitionWriter }

func (t transitionSink) Append(e orchestrator.TransitionEntry) error {
	err := t.w.Append(state.Transition{
		TS:    e.TS,
		Issue: e.Issue,
		// The §9.2 state names are the file format. A rename of these constants
		// changes what a later daemon reads back, which is why the acceptance
		// test drives a real failure through a real daemon and asks the reader
		// for it, rather than asserting these two lines.
		From:          string(e.From),
		To:            string(e.To),
		Actor:         e.Actor,
		Reason:        e.Reason,
		RunID:         e.RunID,
		FailureReason: e.FailureReason,
	})
	// The permanence of a failure travels; its spelling does not. The loop knows
	// ErrSinkUnwritable because that is part of the sink contract it defined, and
	// it deliberately does not know internal/state — the same boundary the entry
	// conversion above exists to keep.
	return unwritable(err)
}

// attemptSink translates one finished attempt into its on-disk record (#60), on
// transitionSink's terms and for its reasons.
type attemptSink struct{ w *state.AttemptWriter }

func (t attemptSink) Append(o orchestrator.AttemptOutcome) error {
	return unwritable(t.w.Append(state.Attempt{
		Issue:   o.Issue,
		Attempt: o.Attempt,
		Turns:   o.Turns,
		RunID:   o.RunID,
		Agent:   o.Agent.Kind,
		Model:   o.Agent.Model,

		StartedAt: o.StartedAt,
		EndedAt:   o.EndedAt,
		Ran:       o.Ran,

		FailureReason: o.FailureReason,
		// The §9.7 verdict as a string, because the file format may not import
		// the enum. Its own spelling, not a numeric cast: a rename of the loop's
		// constants changes what a later reader sees, which is what the test
		// pinning state.VerdictPublished to orchestrator.VerdictPublished is for.
		Verdict: verdictName(o.Verdict),

		InputTokens:  o.Usage.InputTokens,
		OutputTokens: o.Usage.OutputTokens,
		CostUSD:      o.Usage.CostUSD,
	}))
}

// verdictName spells a §9.7 verdict for the file. VerdictUnknown writes nothing
// at all: the field means "what verification concluded", and an attempt that
// never reached verification has no answer to record — writing "unknown" would
// make a failed launch indistinguishable from a check that ran and could not
// decide.
func verdictName(v orchestrator.Verdict) string {
	if v == orchestrator.VerdictUnknown {
		return ""
	}
	return v.String()
}

// unwritable carries the permanence of a state-dir failure across the boundary
// the two sink types exist to keep. Shared by both, so a log that grows a third
// writer cannot acquire a different opinion of what is retryable.
func unwritable(err error) error {
	if errors.Is(err, state.ErrLogUnwritable) {
		return fmt.Errorf("%w: %v", orchestrator.ErrSinkUnwritable, err)
	}
	return err
}

// attach binds the running orchestrator and starts publishing run records. It
// returns once the first file is on disk, so `ben status` against a daemon that
// has started never has to distinguish "not written yet" from "not running".
func (s *stateFiles) attach(o *orchestrator.Orchestrator, log *slog.Logger) {
	s.lookup.set(o)

	meta := state.Daemon{
		ID:          o.DaemonID(),
		Workflow:    s.key,
		PID:         os.Getpid(),
		StartedAt:   time.Now(),
		HeartbeatMS: int(heartbeat / time.Millisecond),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// prev is the last projection written, with WrittenAt excluded — that field
	// changes on every write by construction, so comparing it would make every
	// tick look like a change and defeat the whole point of comparing.
	var prev state.Runs
	var beat time.Time
	write := func(stopped bool) {
		runs := runsNow(o, meta, stopped)
		now := time.Now()
		if reflect.DeepEqual(runs, prev) && now.Sub(beat) < heartbeat && !stopped {
			return
		}
		prev, beat = runs, now
		runs.Daemon.WrittenAt = now
		if err := s.dir.WriteRuns(runs); err != nil {
			log.Error("could not write the run records", "error", err)
		}
	}
	write(false)

	go func() {
		defer close(done)
		t := time.NewTicker(refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				write(false)
			}
		}
	}()

	s.stop = func() {
		cancel()
		<-done
		// A last write after the publisher has stopped, marking the daemon
		// stopped rather than leaving the heartbeat to go stale. Both readings
		// exist because both happen: a graceful exit says so positively and
		// immediately, and a `kill -9` — which writes nothing — is what the
		// staleness check is still there for.
		write(true)
	}
}

// close stops publishing and releases both logs. Safe on a stateFiles that was
// never attached, which is the path a startup refusal takes.
//
// Both are closed whatever the first one answers: leaving a descriptor open
// because its neighbour failed to close is how a supervised restart inherits a
// handle on a file it is about to repair.
func (s *stateFiles) close() error {
	if s.stop != nil {
		s.stop()
	}
	return errors.Join(s.writer.Close(), s.attempts.Close())
}

// runsNow projects the orchestrator's published snapshots into the file format.
// WrittenAt is deliberately left zero: the caller stamps it at the moment it
// decides to write, which is also what lets two projections be compared.
func runsNow(o *orchestrator.Orchestrator, meta state.Daemon, stopped bool) state.Runs {
	meta.Draining = o.Draining()
	meta.HeldClaims = o.HeldCount()
	meta.Stopped = stopped

	snaps := o.Status()
	records := make([]state.Run, 0, len(snaps))
	for _, s := range snaps {
		r := state.Run{
			Issue:         s.Identifier,
			RunID:         s.RunID,
			State:         string(s.State),
			Attempt:       s.Attempt,
			Turns:         s.Turns,
			FailureReason: s.FailureReason,
			Branch:        s.Branch,
			UpdatedAt:     s.UpdatedAt,
			Continuation:  s.Continuation,
			SessionID:     s.SessionID,
		}
		if !s.NextTimerAt.IsZero() {
			at := s.NextTimerAt
			r.NextTimerAt, r.NextTimer = &at, s.NextTimer.String()
		}
		records = append(records, r)
	}
	return state.Runs{Daemon: meta, Records: records}
}

// runLookup resolves an issue identifier to its correlation attributes.
//
// A cell because the logger exists before the orchestrator does, and cannot be
// built afterwards: config.Watch takes the logger and publishes revision 1
// before it returns, and orchestrator.New needs what Watch returns. Every line
// logged in that window simply carries no correlation, which is correct — no run
// exists yet to correlate to.
type runLookup struct {
	o atomic.Pointer[orchestrator.Orchestrator]
}

func (l *runLookup) set(o *orchestrator.Orchestrator) { l.o.Store(o) }

func (l *runLookup) find(issue string) (runID, sessionID string, ok bool) {
	o := l.o.Load()
	if o == nil {
		return "", "", false
	}
	s, found := o.StatusFor(issue)
	if !found {
		return "", "", false
	}
	return s.RunID, s.SessionID, true
}

// correlate is the §10.3 logging contract: every line about a run carries the
// issue identifier, the run id and the session id.
//
// A handler rather than a logger threaded through the loop, because the loop
// logs from thirty places and the attributes are derivable from the one the loop
// already supplies. The alternative — a per-record logger — makes every new log
// line a place the contract can be forgotten, and none of them fail a test when
// it is.
//
// Only lines that name an issue are enriched. A line about the daemon itself has
// no run to correlate to, and inventing attributes for it would be worse than
// omitting them.
type correlate struct {
	inner  slog.Handler
	lookup func(string) (string, string, bool)
	// issue is one carried in by With("issue", …) rather than passed per call.
	// Both spellings reach here and both mean the same thing, so both are
	// enriched; without this a logger derived once per run would be the one
	// place the contract silently did not hold.
	issue string
}

func (h correlate) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h correlate) Handle(ctx context.Context, r slog.Record) error {
	issue := h.issue
	if issue == "" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "issue" {
				issue = a.Value.String()
				return false
			}
			return true
		})
	}
	if issue != "" && h.lookup != nil {
		if runID, session, ok := h.lookup(issue); ok {
			if runID != "" {
				r.AddAttrs(slog.String("run_id", runID))
			}
			if session != "" {
				r.AddAttrs(slog.String("session_id", session))
			}
		}
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs and WithGroup rebuild the wrapper rather than returning the inner
// handler. Embedding would have been shorter and would have silently dropped
// correlation from every logger derived with With — which is exactly the shape
// of logger a per-run context encourages.
func (h correlate) WithAttrs(as []slog.Attr) slog.Handler {
	out := correlate{inner: h.inner.WithAttrs(as), lookup: h.lookup, issue: h.issue}
	for _, a := range as {
		if a.Key == "issue" {
			out.issue = a.Value.String()
		}
	}
	return out
}

func (h correlate) WithGroup(name string) slog.Handler {
	return correlate{inner: h.inner.WithGroup(name), lookup: h.lookup, issue: h.issue}
}
