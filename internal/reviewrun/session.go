package reviewrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// Composer builds the reviewer invocation for one subject. It is the BEN
// adapter boundary #204 names: the configured Codex argv and prompt are
// composed *here*, in the trusted process, and the substrate receives opaque
// process input.
type Composer func(Subject) (Request, error)

// SandboxSource resolves where one subject's review must run: the sandbox the
// issue's workspace cycle already selected, at the profile revision it is
// pinned to.
//
// Nil under the local executor, which has none and says so. Non-nil, it is the
// *reader* of the cycle record rather than an acquirer: a review never
// allocates a sandbox. An issue whose cycle has none is a review that parks,
// because the tree the coding attempt produced is what a review is about.
type SandboxSource func(context.Context, Subject) (Placement, error)

// Sleeper is the one clock seam. A test drives the poll loop without elapsed
// time; the daemon waits.
type Sleeper func(context.Context, time.Duration) error

// Options are what one session is constructed from.
type Options struct {
	Executor Executor
	Store    Store
	Compose  Composer
	Sandbox  SandboxSource
	// Secrets is what the trusted process holds and the reviewer must not. It is
	// a function rather than a slice because a minted credential has a lifetime,
	// and comparing against a stale copy would pass a request carrying the
	// current one.
	Secrets func() []string
	// Poll is how long to wait between event reads while a run is live, and
	// Deadline how long one Review call may wait for the stream to seal. A run
	// that outlives the deadline is ErrRunIncomplete — not a failure: the record
	// stands and the next sweep resumes it at the committed cursor.
	Poll     time.Duration
	Deadline time.Duration
	Sleep    Sleeper
	Logger   *slog.Logger
}

// DefaultPoll and DefaultDeadline bound one Review call. The deadline is
// deliberately shorter than a coding attempt's: a review reads a bounded diff
// and produces a small object, and a sweep that resumes is cheaper than one
// that blocks.
const (
	DefaultPoll     = 2 * time.Second
	DefaultDeadline = 15 * time.Minute
)

// Session turns validated subjects into closed verdicts over one executor.
type Session struct {
	exec     Executor
	store    Store
	compose  Composer
	sandbox  SandboxSource
	secrets  func() []string
	poll     time.Duration
	deadline time.Duration
	sleep    Sleeper
	log      *slog.Logger
}

// New validates the seams. A session missing one would fail at the first
// review rather than at startup.
func New(opts Options) (*Session, error) {
	switch {
	case opts.Executor == nil:
		return nil, errors.New("reviewrun: a session needs an executor")
	case opts.Store == nil:
		return nil, errors.New("reviewrun: a session needs a durable store")
	case opts.Compose == nil:
		return nil, errors.New("reviewrun: a session needs an invocation composer")
	}
	s := &Session{
		exec: opts.Executor, store: opts.Store, compose: opts.Compose, sandbox: opts.Sandbox,
		secrets: opts.Secrets, poll: opts.Poll, deadline: opts.Deadline, sleep: opts.Sleep, log: opts.Logger,
	}
	if s.secrets == nil {
		s.secrets = func() []string { return nil }
	}
	if s.poll <= 0 {
		s.poll = DefaultPoll
	}
	if s.deadline <= 0 {
		s.deadline = DefaultDeadline
	}
	if s.sleep == nil {
		s.sleep = sleep
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Review drives one review run for a subject to a validated verdict.
//
// It is idempotent over the subject and that is the whole design: called again
// after a crash, after a lost start response, after an Airlock restart, or
// simply on the next daemon sweep, it resolves the *same* durable run rather
// than minting another. A run that has already stated a verdict returns it
// without touching the executor at all, which is what makes "a terminal run
// with no published review resumes publication" cost no second Codex
// invocation.
//
// Every refusal below leaves the occurrence unrouted. None of them is `clean`.
func (s *Session) Review(ctx context.Context, sub Subject) (review.Report, error) {
	if err := sub.Validate(); err != nil {
		return review.Report{}, err
	}
	run, err := sub.RunID()
	if err != nil {
		return review.Report{}, err
	}
	subject, err := sub.SubjectDigest()
	if err != nil {
		return review.Report{}, err
	}

	req, err := s.compose(sub)
	if err != nil {
		return review.Report{}, fmt.Errorf("reviewrun: composing the reviewer invocation: %w", err)
	}
	// Checked before the request can reach an executor, so a leak is a refusal
	// rather than something a later audit of the wire would have to find. A
	// session with a SandboxSource is one whose requests cross to a backend, and
	// the wider provider-credential rule applies to exactly those.
	if err := CheckRequest(req, s.secrets(), s.sandbox != nil); err != nil {
		return review.Report{}, err
	}

	place, err := s.resolveSandbox(ctx, sub)
	if err != nil {
		return review.Report{}, err
	}
	digest, err := s.exec.Digest(Ref{
		Run: run, Repository: sub.Repository, Issue: sub.Issue, Cycle: sub.Cycle,
		Branch: place.Branch, BaseSHA: place.BaseSHA, TargetBranch: place.TargetBranch,
		Sandbox: place.Sandbox, Profile: place.Profile,
	}, req)
	if err != nil {
		return review.Report{}, err
	}

	rec, err := s.open(ctx, sub, run, subject, digest, place)
	if err != nil {
		return review.Report{}, err
	}
	if rec.Terminal() {
		if !rec.Quiet {
			if rec, err = s.refreshQuiet(ctx, rec); err != nil {
				return review.Report{}, err
			}
		}
		if !rec.Quiet {
			return review.Report{}, fmt.Errorf("%w: %s", ErrRunIncomplete, rec.Run)
		}
		s.log.Info("resuming an already-stated review verdict without a second reviewer run",
			"issue", sub.Issue, "run", rec.Run, "reviewer_profile", rec.ReviewerProfile, "verdict", rec.Verdict)
		return review.Report{Verdict: review.Verdict(rec.Verdict), Findings: rec.Findings}, nil
	}

	rec, state, err := s.resolveRun(ctx, rec, req)
	if err != nil {
		return review.Report{}, err
	}
	return s.consume(ctx, rec, state)
}

// Retire removes the durable record for a subject once its route is complete.
//
// The forge markers are the policy record and outlive this; the execution
// record's only remaining reader would be a resume that can no longer happen.
// Refused while the run is not terminal and quiet, for the reason Journal.Close
// is: a record removed over a possibly-live run is a run nothing can attach to.
func (s *Session) Retire(ctx context.Context, sub Subject) error {
	run, err := sub.RunID()
	if err != nil {
		return err
	}
	rec, err := s.store.Load(run)
	switch {
	case errors.Is(err, ErrNoRecord):
		return nil
	case err != nil:
		return err
	}
	if !rec.Sealed || !rec.Quiet {
		if rec, err = s.refreshQuiet(ctx, rec); err != nil {
			return err
		}
	}
	if !rec.Sealed || !rec.Quiet {
		return fmt.Errorf("%w: %s", ErrNotQuiet, rec.Run)
	}
	return s.store.Delete(rec.Run)
}

// Record exposes the durable execution record for a subject, for `ben status`
// and the startup survey. ErrNoRecord when nothing was ever dispatched.
func (s *Session) Record(sub Subject) (Record, error) {
	run, err := sub.RunID()
	if err != nil {
		return Record{}, err
	}
	return s.store.Load(run)
}

// Reconcile is the startup survey: every retained review run, with a fresh
// quiet observation where the record does not already carry one.
//
// A read that repairs only the one fact a restart cannot reconstruct. It is
// completed before the daemon dispatches new review work, so a retained cycle
// whose previous run is still live cannot have a replacement started into it.
func (s *Session) Reconcile(ctx context.Context) ([]Record, error) {
	records, err := s.store.Records()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(records))
	for _, rec := range records {
		if !rec.Dispatched || rec.Quiet {
			out = append(out, rec)
			continue
		}
		refreshed, err := s.refreshQuiet(ctx, rec)
		if err != nil {
			// Reported rather than fatal: an unreachable executor leaves the
			// record saying "possibly live", which is the reading that costs a
			// parked review rather than a second one.
			s.log.Warn("could not observe a retained review run; it stays possibly-live",
				"run", rec.Run, "issue", rec.Issue, "error", err)
			out = append(out, rec)
			continue
		}
		out = append(out, refreshed)
	}
	return out, nil
}

func (s *Session) resolveSandbox(ctx context.Context, sub Subject) (Placement, error) {
	if s.sandbox == nil {
		return Placement{}, nil
	}
	place, err := s.sandbox(ctx, sub)
	if err != nil {
		return Placement{}, fmt.Errorf("reviewrun: resolving the workspace-cycle sandbox for issue %s: %w", sub.Issue, err)
	}
	if !place.Complete() {
		// A remote review with no sandbox to name would have to be given one, and
		// the only sandbox available to give is somebody else's.
		return Placement{}, fmt.Errorf("%w: issue %s has no completely named sandbox to review in", ErrSandboxMismatch, sub.Issue)
	}
	return place, nil
}

// open reads or reserves the durable record, holding an existing one to the
// exact request, subject and sandbox it was minted for.
//
// Reserving writes the identity *before* anything may be dispatched, which is
// what lets a later process name the run a crash left behind.
func (s *Session) open(
	ctx context.Context, sub Subject, run, subject, digest string, place Placement,
) (Record, error) {
	rec, err := s.store.Load(run)
	switch {
	case err == nil:
		reopened, err := s.reopen(rec, subject, digest, place.Sandbox, place.Profile)
		if err == nil || !rec.Refused() {
			return reopened, err
		}
		// A refused record holds no run, so the mismatch that would otherwise
		// refuse a resume is instead the operator's recovery: the request was
		// recomposed — a lower diff bound, a different invocation — and nothing
		// that was dispatched can be confused with what is about to be.
		s.log.Info("superseding a refused review run with a recomposed request",
			"run", rec.Run, "issue", rec.Issue, "reason", rec.Refusal.Reason, "mismatch", err)
		if err := s.store.Delete(rec.Run); err != nil {
			return Record{}, err
		}
	case !errors.Is(err, ErrNoRecord):
		return Record{}, err
	}

	if err := s.gate(ctx, sub.CycleAddress(), run); err != nil {
		return Record{}, err
	}
	rec = Record{
		Version: RecordVersion, Run: run, Cycle: sub.CycleAddress(), Role: Role,
		Repository: sub.Repository, Issue: sub.Issue,
		Approval: sub.Cycle, Occurrence: sub.Occurrence, Claim: sub.Claim,
		PR: sub.PR, Base: sub.Base, Head: sub.Head,
		ReviewerProfile: sub.ReviewerProfile,
		Branch:          place.Branch, BaseSHA: place.BaseSHA, TargetBranch: place.TargetBranch,
		Sandbox: place.Sandbox, Profile: place.Profile, Digest: digest, Subject: subject,
	}
	if err := s.store.Save(rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// reopen is the resume comparison, and each clause is a way a matching address
// could otherwise carry a different meaning.
func (s *Session) reopen(rec Record, subject, digest, sandbox, profile string) (Record, error) {
	if rec.Digest != digest {
		return Record{}, fmt.Errorf("%w: %s was dispatched as %s and is now composed as %s",
			ErrRunMismatch, rec.Run, rec.Digest, digest)
	}
	if rec.Subject != subject {
		// Same endpoints, different bytes: a force push that kept the SHAs is
		// impossible, so this is a diff read differently. Either way the recorded
		// run did not judge what is being asked about now.
		return Record{}, fmt.Errorf("%w: %s judged a different subject", ErrRunMismatch, rec.Run)
	}
	if sandbox != "" && rec.Sandbox != sandbox {
		return Record{}, fmt.Errorf("%w: %s is recorded in sandbox %q and this cycle selects %q",
			ErrSandboxMismatch, rec.Run, rec.Sandbox, sandbox)
	}
	if profile != "" && rec.Profile != profile {
		return Record{}, fmt.Errorf("%w: %s is pinned at %q and the backend now reports %q",
			ErrProfileDrift, rec.Run, rec.Profile, profile)
	}
	return rec, nil
}

// gate refuses a new run while another in the same workspace cycle has not been
// positively observed quiet, and prunes the ones that have.
//
// Positive only. An unreachable executor, an unknown state, a phase that merely
// stopped saying "running" — all of them answer "not quiet", because the
// alternative reading puts two agents in one sandbox (SPEC §9.8,
// remote.MayReuse).
func (s *Session) gate(ctx context.Context, cycle, run string) error {
	records, err := s.store.Records()
	if err != nil {
		return err
	}
	for _, other := range records {
		if other.Run == run || other.Cycle != cycle {
			continue
		}
		if other.Dispatched && !other.Quiet {
			refreshed, err := s.refreshQuiet(ctx, other)
			if err != nil {
				return fmt.Errorf("%w: %s could not be observed: %v", ErrNotQuiet, other.Run, err)
			}
			other = refreshed
		}
		if other.Dispatched && !other.Quiet {
			return fmt.Errorf("%w: %s is still executing in %s", ErrNotQuiet, other.Run, cycle)
		}
		if err := s.store.Delete(other.Run); err != nil {
			return err
		}
		s.log.Info("retired a finished review run's execution record",
			"run", other.Run, "cycle", cycle, "verdict", other.Verdict)
	}
	return nil
}

func (s *Session) refreshQuiet(ctx context.Context, rec Record) (Record, error) {
	if !rec.Dispatched || rec.Refused() {
		// Nothing was ever started at this address — never attempted, or
		// attempted and definitively refused — so nothing is executing.
		rec.Quiet = true
		return rec, s.store.Save(rec)
	}
	st, err := s.exec.Status(ctx, rec.Ref())
	if err != nil {
		return rec, err
	}
	if !st.Reachable || !st.Quiet {
		return rec, nil
	}
	rec.Quiet = true
	if st.Sealed {
		rec.Sealed = true
	}
	return rec, s.store.Save(rec)
}

// resolveRun dispatches or resolves the run at this address, in the one order
// that cannot produce two of them.
func (s *Session) resolveRun(ctx context.Context, rec Record, req Request) (Record, State, error) {
	if !rec.Dispatched {
		// The mark lands before the call. A crash anywhere inside it — including
		// after the executor accepted the start — leaves a record that says a
		// dispatch was attempted, and the branch below is what resolves it.
		rec.Dispatched = true
		if err := s.store.Save(rec); err != nil {
			return rec, State{}, err
		}
		st, err := s.exec.Start(ctx, rec.Ref(), req)
		if err != nil && !errors.Is(err, ErrRunRefused) {
			// A start error says nothing about whether the backend committed the
			// keyed request, and the run id exists only in the response — so
			// Attach cannot name the unknown result. The one safe resolution is to
			// replay the exact address and body and receive the stored result.
			// A refusal is the exception: it is the answer, not its absence.
			s.log.Warn("the reviewer start was not acknowledged; replaying the same idempotency address",
				"run", rec.Run, "error", err)
			st, err = s.replayStart(ctx, rec, req)
		}
		return s.settle(rec, st, err)
	}

	if rec.BackendRunID != "" {
		// A known resource id is permanent; replaying the creation key instead
		// would be unsafe once the backend's idempotency window has expired.
		st, err := s.exec.Attach(ctx, rec.Ref(), rec.BackendRunID)
		if err != nil {
			return rec, State{}, fmt.Errorf("%w: %s: %v", ErrRunUnresolved, rec.Run, err)
		}
		return s.observe(rec, st)
	}

	// Dispatched with no backend id is either the lost-response state or a
	// recorded refusal, and both re-offer the exact request at the exact
	// address. For the first that recovers the stored result rather than
	// attaching without an id. For the second it costs the executor nothing
	// while nothing has changed — it answers an unchanged body from its own
	// record — and it is what lets a body it can now deliver go out without an
	// operator having to know which sweep to poke.
	st, err := s.replayStart(ctx, rec, req)
	return s.settle(rec, st, err)
}

// settle turns a start's answer into the record's next state. A refusal is
// retained as the durable answer and returned as one; any other error leaves
// the dispatch unresolved for the next sweep to replay; a status is observed.
func (s *Session) settle(rec Record, st State, err error) (Record, State, error) {
	if refusal, ok := RefusalOf(err); ok {
		rec.Refusal = &refusal
		// Nothing was started, so nothing is executing: the gate may let another
		// run into this sandbox, and Retire may release this record.
		rec.Quiet = true
		if saveErr := s.store.Save(rec); saveErr != nil {
			return rec, State{}, saveErr
		}
		s.log.Warn("the executor refused to admit the reviewer invocation; nothing was started",
			"run", rec.Run, "issue", rec.Issue, "head", rec.Head, "reason", refusal.Reason, "detail", refusal.Detail)
		return rec, State{}, fmt.Errorf("%w: %s", err, rec.Run)
	}
	if err != nil {
		return rec, State{}, fmt.Errorf("%w: %s: %v", ErrRunUnresolved, rec.Run, err)
	}
	return s.observe(rec, st)
}

func (s *Session) replayStart(ctx context.Context, rec Record, req Request) (State, error) {
	replayer, ok := s.exec.(StartReplayer)
	if !ok {
		return State{}, fmt.Errorf("%w: %s: this executor cannot replay a start across a process boundary",
			ErrRunUnresolved, rec.Run)
	}
	return replayer.ReplayStart(ctx, rec.Ref(), req)
}

// observe retains what a fresh state told us, and refuses the two ways a
// matching address can name a world that has moved.
func (s *Session) observe(rec Record, st State) (Record, State, error) {
	if st.Profile != "" && rec.Profile != "" && st.Profile != rec.Profile {
		return rec, st, fmt.Errorf("%w: %s is pinned at %q and the run reports %q",
			ErrProfileDrift, rec.Run, rec.Profile, st.Profile)
	}
	if st.Sandbox != "" && rec.Sandbox != "" && st.Sandbox != rec.Sandbox {
		return rec, st, fmt.Errorf("%w: %s is recorded in %q and the run reports %q",
			ErrSandboxMismatch, rec.Run, rec.Sandbox, st.Sandbox)
	}
	if st.BackendRunID != "" && (st.BackendRunID != rec.BackendRunID || rec.Refused()) {
		rec.BackendRunID = st.BackendRunID
		if rec.Refused() {
			// A refused address now names a run. The request is the same bytes,
			// so what changed is how the executor delivers them; the refusal is
			// history, and the quiet it implied is not — a run exists now.
			rec.Refusal = nil
			rec.Quiet = false
		}
		if err := s.store.Save(rec); err != nil {
			return rec, st, err
		}
	}
	return rec, st, nil
}

// consume reads durable events forward from the committed cursor until the
// stream seals, then extracts the sole verdict from admitted stdout.
func (s *Session) consume(ctx context.Context, rec Record, st State) (review.Report, error) {
	waitCtx := ctx
	if s.deadline > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, s.deadline)
		defer cancel()
	}

	for {
		chunks, err := s.exec.Events(waitCtx, rec.Ref(), rec.Cursor)
		if err != nil {
			return review.Report{}, fmt.Errorf("reviewrun: reading durable output for %s: %w", rec.Run, err)
		}
		next, err := admit(rec, chunks)
		if err != nil {
			return review.Report{}, err
		}
		// The act before the position: the bytes and the cursor that admits them
		// land in one replacement of one record, so no reader can resume from a
		// position whose evidence it does not hold.
		next.Quiet = st.Reachable && st.Quiet
		next.Sealed = st.Reachable && st.Sealed
		if next.Cursor != rec.Cursor || next.Quiet != rec.Quiet || next.Sealed != rec.Sealed {
			if err := s.store.Save(next); err != nil {
				return review.Report{}, err
			}
		}
		rec = next

		if st.Reachable && st.Sealed && st.Quiet {
			break
		}
		if err := s.sleep(waitCtx, s.poll); err != nil {
			// The deadline is not a failure: the record stands at its committed
			// cursor and the next sweep resumes from exactly there.
			return review.Report{}, fmt.Errorf("%w: %s", ErrRunIncomplete, rec.Run)
		}
		if st, err = s.exec.Status(waitCtx, rec.Ref()); err != nil {
			return review.Report{}, fmt.Errorf("reviewrun: observing %s: %w", rec.Run, err)
		}
		if rec, st, err = s.observe(rec, st); err != nil {
			return review.Report{}, err
		}
	}

	report, err := ExtractVerdict(rec.Output)
	if err != nil {
		// Persist what was observed — sealed, domain quiet, and the bytes it produced —
		// but never a verdict. The occurrence stays unrouted, and a re-read of
		// this record cannot turn silence into an answer.
		rec.Sealed = true
		rec.Quiet = st.Reachable && st.Quiet
		if saveErr := s.store.Save(rec); saveErr != nil {
			return review.Report{}, saveErr
		}
		return review.Report{}, err
	}

	rec.Sealed = true
	rec.Quiet = true // the loop exits only on a reachable, sealed, quiet state
	rec.Verdict = string(report.Verdict)
	rec.Findings = report.Findings
	if err := s.store.Save(rec); err != nil {
		return review.Report{}, err
	}
	s.log.Info("a review run stated a closed verdict",
		"issue", rec.Issue, "run", rec.Run, "head", rec.Head,
		"reviewer_profile", rec.ReviewerProfile, "verdict", rec.Verdict)
	return report, nil
}

// admit folds new chunks into the record: deduplicating replay, refusing a gap,
// refusing a conflict, and never advancing past bytes it did not retain.
//
// Batch admission is atomic. A conflict or a gap anywhere in the batch leaves
// the record untouched, so a partial application cannot commit BEN to a prefix
// of a stream it has already refused.
func admit(rec Record, chunks []Chunk) (Record, error) {
	next := rec
	next.Output = append([]byte(nil), rec.Output...)
	next.Admitted = make(map[int64]string, len(rec.Admitted)+len(chunks))
	for seq, d := range rec.Admitted {
		next.Admitted[seq] = d
	}

	for _, c := range chunks {
		if c.Truncated {
			return rec, fmt.Errorf("%w: %s at sequence %d", ErrOutputTruncated, rec.Run, c.Seq)
		}
		digest := chunkDigest(c)
		if c.Seq <= next.Cursor {
			// Replay. Identical bytes are dropped; different bytes at a sequence
			// already admitted mean this is not the stream BEN accepted.
			if prior, ok := next.Admitted[c.Seq]; ok && prior != digest {
				return rec, fmt.Errorf("%w: %s at sequence %d", ErrEventConflict, rec.Run, c.Seq)
			}
			continue
		}
		if c.Seq != next.Cursor+1 {
			return rec, fmt.Errorf("%w: %s expected sequence %d and was offered %d",
				ErrEventGap, rec.Run, next.Cursor+1, c.Seq)
		}
		if c.Stream == ChunkStdout && len(next.Output)+len(c.Payload) > MaxOutput {
			return rec, fmt.Errorf("%w: %s at sequence %d", ErrOutputOverflow, rec.Run, c.Seq)
		}
		if c.Stream == ChunkStdout {
			next.Output = append(next.Output, c.Payload...)
		}
		next.Admitted[c.Seq] = digest
		next.Cursor = c.Seq
	}
	if len(next.Admitted) == 0 {
		next.Admitted = nil
	}
	return next, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
