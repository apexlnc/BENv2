package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// effectsQueueDepth bounds the pending tracker side effects. Deep enough that
// the loop never blocks on a slow label write in practice, shallow enough
// that a wedged tracker surfaces as backpressure rather than unbounded
// memory.
const effectsQueueDepth = 256

// Orchestrator is the single-mutator authority loop (SPEC §9.1).
type Orchestrator struct {
	cfg   Config
	log   *slog.Logger
	clock Clock

	// Transitions is the append-only transition log (SPEC §9.11).
	Transitions *TransitionLog
	// Attempts is the append-only attempt-outcome log (#60).
	Attempts *AttemptLog

	signals chan signal
	effects chan func(context.Context)

	// Loop-owned. No other goroutine may touch these.
	records map[string]*Record
	// held are the retained `done` claims (SPEC §9.8). An issue is in exactly
	// one of records and held, never both: see held.go.
	held map[string]*heldClaim
	// instance names this run of this daemon, for run ids that outlive it. See
	// Record.runID.
	instance string
	// nextToken issues process-unique identities for the two things that can
	// own an issue — a run record and a held claim. See newToken.
	nextToken int
	// reconcileInFlight and dispatchInFlight are separate on purpose: §9.4's
	// reconciliation must run every tick even while the dispatch reads are
	// wedged (see onTick).
	reconcileInFlight bool
	dispatchInFlight  bool
	// markerClears are the run-marker removals this daemon owes. Held here rather
	// than on the record because every caller of freeWorkspaceMarker is on a path
	// that ends in the record being forgotten (see it for why that matters).
	// Loop-owned.
	//
	// A slice, not a map keyed by issue: the same issue can have a clear pending
	// against two *different* stores across an identity reload, and one keyed by
	// identifier alone would drop the first — leaving a marker standing under a root
	// nothing will ever revisit.
	//
	// Pointers, because a clear has a lifetime now rather than only a presence: the
	// removal runs in a worker (see clearMarkerWith), so the loop has to be able to
	// mark one in flight or abandoned and find that state again when its result
	// lands.
	markerClears []*pendingClear
	// sweepDisposing are the issues a §9.10 step 5 pass has been granted permission
	// to dispose the workspace of: taken on this goroutine immediately before the
	// pass touches the workspace, released when it is done with it. Loop-owned.
	//
	// It exists because the pass's ownership set is a *snapshot* and the pass is I/O
	// all the way down. Between the snapshot and the disposal an issue can reopen,
	// be claimed and reach Prepare, and a worker still holding the old answer would
	// then delete a live attempt's worktree. Dispatch and recovery adoption both
	// consult this, so a reservation is two-sided: nothing new can own the issue
	// while it stands, and it is never granted for an issue something already owns.
	sweepDisposing map[string]bool
	// scanOwed says the §9.10 step 1 candidate read failed and must be redone;
	// scanInFlight is its single slot. Both loop-owned. Without the retry, a
	// tracker that was down at startup would leave every claim this principal
	// holds unaccounted for until somebody restarted the daemon — §8.3 excludes
	// assigned issues from the ordinary Fetch, so nothing else ever looks.
	scanOwed     bool
	scanInFlight bool
	// The §9.10 step 5 sweep's outstanding work, all loop-owned.
	//
	// sweepPassOwed is a whole pass that never ran or could not list the directory;
	// sweepDeferred are the individual workspaces whose own answer may still change.
	// Kept apart because they cost differently — one workspace nobody can resolve
	// must not drag the whole directory through a tracker read every tick.
	//
	// sweepBundle is the adapter set the deferred refs were found under. An identity
	// reload moves workspace.root (§10.1), so a ref carried across one names a path
	// under a provider this daemon no longer has; retrySweep drops them rather than
	// re-examining them through the wrong root.
	//
	// sweepSkipped are the workspaces a pass declined to examine because a record or
	// held claim owned the issue, keyed by *identifier* — which is what the record set
	// is keyed by, and so what retrySweep can ask the record set about when it decides
	// which to hand back. A skip is absent from a pass's result, so without this the
	// promise step 5 made about that directory ends the moment its owner is dropped
	// rather than disposed.
	//
	// sweepCursor is where the next pass resumes. A pass examines at most
	// maxSweepExaminations workspaces, and without a cursor that bound would be a
	// permanent floor under the sorted tail rather than a pace.
	sweepPassOwed bool
	sweepDeferred map[string]core.WorkspaceRef
	sweepSkipped  map[string]core.WorkspaceRef
	sweepCursor   string
	sweepBundle   *Bundle
	sweepInFlight bool
	// parkedCursor is the identifier last offered §9.8's one absence confirmation
	// per tick, so the next tick starts past it (offerParkedConfirmations). An
	// identifier rather than an index because the set it rotates over changes
	// between ticks — records are dropped, and new ones park.
	//
	// owedCursor is the same rotation over a different candidate set: the records
	// whose owed tracker write keeps failing (offerOwedConfirmations). heldCursor
	// is the third, over the retained claims that owe a confirming Get — absent
	// from the sweep read, or holding a settled release the tracker keeps refusing
	// (offerHeldConfirmations) — a set of held claims rather than of records, and
	// the one set nothing in the other two can appear in, since a converted claim
	// has no record at all. Separate cursors because the sets are separate — a
	// record can be in both of the first two, and one cursor would let either
	// set's turnover skip the other's records. *One* cursor for held claims,
	// though, however they became candidates: they share a budget, so sharing a
	// rotation is what makes that budget fair across both (#135).
	parkedCursor string
	owedCursor   string
	heldCursor   string
	// draining is loop-owned and drainWaiters are the callers of Shutdown
	// awaiting it. See shutdown.go.
	draining     bool
	drainWaiters []chan struct{}

	// recovered says §9.10 has run. Atomic rather than loop-owned because Run
	// reads it before the loop exists, and Recover writes it from the caller's
	// goroutine.
	recovered atomic.Bool

	mu        sync.RWMutex
	published map[string]Snapshot
	heldCount int
	// drainingPublished mirrors draining for readers outside the loop
	// (`ben status`). The loop-owned flag is the authority; this is its
	// projection, in the same way published mirrors records.
	drainingPublished bool
	// identityWork mirrors identityWorkOutstanding for readers outside the loop
	// (config.WatchOptions.Quiescent), in the same way drainingPublished mirrors
	// draining.
	//
	// A mirror of the *whole* predicate rather than of the record set, and
	// refreshed once per loop turn rather than at each mutation — see
	// publishIdentityWork. Both choices are about the same failure: the advisory
	// and the barrier answering differently means the watcher rebuilds a candidate
	// every tick and is refused every tick, which is the recurring credential
	// check and tracker round trip the advisory exists to prevent.
	identityWork bool

	// applied counts the signals the loop has finished handling, per kind. It
	// exists for tests and is the acknowledgement a negative assertion needs
	// (§12.2's determinism rule, and #106's review): the fakes count a call at
	// *entry* — deliberately, so a test can catch a ladder standing in a gate —
	// so a Stop or Probe counter proves the question was asked and says nothing
	// about whether its answer has reached the loop. Between those two facts sit
	// the in-flight guards, which make a tick arriving inside that window a
	// no-op.
	//
	// Written only by the authority goroutine, one add per signal, so the atomics
	// are for the readers' benefit rather than for its own.
	applied [numSigKinds]atomic.Uint64
}

// New builds an orchestrator. It starts nothing; Run does.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("orchestrator: Runtime is required — the definition and the adapters built from it arrive together (SPEC §5.4)")
	}
	// Checked here rather than trusted, because a zero snapshot is what a source
	// nobody has published to hands back, and every adapter call below would
	// then be a nil dereference at the first tick.
	start, _ := cfg.Runtime.Load()
	if err := checkBundle(start.Definition, start.Runtime); err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	var clock Clock = realClock{}
	if cfg.Clock != nil {
		clock = cfg.Clock
	}
	daemon := cfg.DaemonID
	if daemon == "" {
		host, _ := os.Hostname()
		daemon = host + "/" + start.Definition.Key
	}
	cfg.DaemonID = daemon

	// The instance: this *run* of this daemon, distinct from DaemonID, which
	// names the daemon across restarts. Run ids are written to a log that
	// outlives the process, so without it two attempts on either side of a
	// restart carry the same handle (see Record.runID). Base 36 of the start
	// instant keeps it short enough to read in a log line and monotonic enough
	// to sort by.
	instance := cfg.Instance
	if instance == "" {
		instance = strconv.FormatInt(clock.Now().UnixMilli(), 36)
	}

	transitions := &TransitionLog{}
	transitions.attach(cfg.Transitions, log)
	attempts := &AttemptLog{}
	attempts.attach(cfg.Attempts, log)

	return &Orchestrator{
		cfg:            cfg,
		instance:       instance,
		log:            log,
		clock:          clock,
		Transitions:    transitions,
		Attempts:       attempts,
		signals:        make(chan signal, 64),
		effects:        make(chan func(context.Context), effectsQueueDepth),
		records:        map[string]*Record{},
		held:           map[string]*heldClaim{},
		published:      map[string]Snapshot{},
		sweepDisposing: map[string]bool{},
	}, nil
}

// checkBundle refuses a runtime the loop cannot work with. Every field is one
// the loop calls unconditionally, so an absent one is a crash at the first tick
// rather than a degraded mode.
func checkBundle(def *config.WorkflowDefinition, b *Bundle) error {
	switch {
	case def == nil || b == nil:
		return errors.New("orchestrator: the runtime source has published nothing; Watch stores revision 1 before it returns")
	case b.Definition == nil:
		return errors.New("orchestrator: the bundle carries no definition, so nothing can check the adapters against it")
	case b.Tracker == nil || b.Workspaces == nil || b.Runner == nil || b.Verifier == nil:
		return errors.New("orchestrator: Tracker, Workspaces, Runner and Verifier are all required")
	case b.ClaimPrincipal == "":
		return errors.New("orchestrator: ClaimPrincipal is required — without it §9.8 cannot tell our own claim from a human who replaced it")
	}
	return nil
}

// Run drives the loop until ctx is cancelled (SPEC §9.1: errgroup
// supervision). The first tick fires immediately.
//
// It refuses to start until Recover has run. The first tick dispatches, and
// dispatch skips only issues a record already covers — so a loop started without
// recovery puts a second agent onto every issue this principal already holds.
// See ErrNotRecovered for why the seam is explicit rather than implicit.
func (o *Orchestrator) Run(ctx context.Context) error {
	if !o.recovered.Load() {
		return ErrNotRecovered
	}
	g, ctx := errgroup.WithContext(ctx)

	// Tracker side effects — label projection and milestone comments — run on
	// one serial queue. Per-issue ordering is what makes the projection
	// coherent: a "set to running" that overtook a "set to claimed" would
	// leave the wrong label standing, and B04's comment markers anchor on the
	// label transition that precedes them.
	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case fn := <-o.effects:
				fn(ctx)
			}
		}
	})

	g.Go(func() error {
		for {
			o.send(ctx, signal{kind: sigTick})
			// Both read under one lock, so a reload landing between them
			// cannot leave this waiting on the old cadence with the wake it
			// already replaced.
			interval, wake := o.pollWait()
			select {
			case <-ctx.Done():
				return nil
			// Re-read per iteration rather than captured once: §5.4 says a
			// valid reload applies to reconciliation, and reconciliation only
			// happens on a tick — so a poll interval captured at startup makes
			// the operator's edit load, validate, log as adopted, and change
			// nothing. `ben status` would report the new interval while the
			// daemon kept the old one.
			case <-o.clock.After(interval):
			case <-wake:
				// A reload landed mid-wait. Tick now rather than only re-arming:
				// a reload can then add ticks but never postpone one, so a
				// watcher that fired repeatedly could not starve reconciliation
				// by restarting its wait each time. The in-flight guards in
				// onTick collapse a burst into at most one outstanding read of
				// each kind, which is what makes that safe.
			}
		}
	})

	// The durable half of each append-only log. Serial per log, so records land
	// in the order they were appended, and off the authority goroutine, because
	// they fsync. Two goroutines rather than one queue: an attempt outcome and a
	// transition go to different files, so there is no order between them to
	// preserve, and one wedged file would otherwise stall the other.
	g.Go(func() error {
		o.Transitions.persist(ctx)
		return nil
	})
	g.Go(func() error {
		o.Attempts.persist(ctx)
		return nil
	})

	g.Go(func() error {
		o.loop(ctx)
		return nil
	})

	err := g.Wait()
	// Every append is over — the authority goroutine has returned — so this
	// drain cannot race one more arriving, and the last transitions of a
	// shutdown reach the disk. They are the ones an operator asking why the
	// daemon stopped will look for.
	o.Transitions.flush()
	o.Attempts.flush()
	return err
}

// loop is the authority goroutine. Every state mutation happens here.
func (o *Orchestrator) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-o.signals:
			o.handle(ctx, s)
			// Once per turn, rather than at each of the dozen sites that change what
			// is outstanding. Every one of those would have to remember, and the one
			// that forgot would leave the advisory disagreeing with the barrier — which
			// is not a visible failure, only a watcher spending a credential check and
			// a tracker round trip per tick to be refused again (#127's finding 5).
			o.publishIdentityWork()
			// After, not before: the count means "this signal's consequences have
			// landed", which is the only reading a barrier can use.
			o.applied[s.kind].Add(1)
		}
	}
}

// Status returns the current run records, for `ben status` (SPEC §10.3).
func (o *Orchestrator) Status() []Snapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]Snapshot, 0, len(o.published))
	for _, s := range o.published {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out
}

// StatusFor returns one issue's record, for the log's correlation attributes
// (SPEC §10.3). Separate from Status because a log handler asks it per line: a
// map lookup rather than a copy-and-sort of every record.
func (o *Orchestrator) StatusFor(identifier string) (Snapshot, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	s, ok := o.published[identifier]
	return s, ok
}

// DaemonID is the actor this daemon writes on every §9.11 entry. Read back
// rather than assumed by the assembly, because New derives one when the config
// leaves it empty and the state file has to name the same daemon the log does.
func (o *Orchestrator) DaemonID() string { return o.cfg.DaemonID }

// Instance is this run of this daemon, as it appears in every run id. See
// Record.runID for why a process-scoped identity is not enough.
func (o *Orchestrator) Instance() string { return o.instance }

// MaxRetryBackoffMS reports the backoff ceiling currently in force.
// `ben status` wants the effective limits, and they move when a reload lands
// (SPEC §5.4).
func (o *Orchestrator) MaxRetryBackoffMS() int {
	return o.configNow().Definition.Config.Limits.MaxRetryBackoffMS
}

// configNow is the configuration currently in force: definition, adapters,
// dispatch verdict and revision, read from the one cell that holds them.
//
// Every policy question — §5.4 names dispatch, retry scheduling, reconciliation,
// hooks and launches — and every adapter call resolves here or from a snapshot
// taken here, and nowhere else.
//
// A decision that fans out over several items takes one snapshot at its
// linearization point and carries it, rather than calling this per item: a
// reload landing mid-pass would otherwise judge the first issue under one
// configuration and the next under another, and the pass would describe a
// configuration that never existed. The same rule makes an asynchronous
// operation capture its snapshot once, at entry, and complete through it — see
// beginPrepare and its siblings.
func (o *Orchestrator) configNow() snapshot {
	snap, _ := o.cfg.Runtime.Load()
	return snap
}

// definition is the workflow definition in force.
func (o *Orchestrator) definition() *config.WorkflowDefinition {
	return o.configNow().Definition
}

// bundle is the adapter set in force.
func (o *Orchestrator) bundle() *Bundle { return o.configNow().Runtime }

// IdentityQuiescent reports whether the loop currently owns no work bound to the
// configuration's identity.
//
// Advisory only, and deliberately cheap: it may be stale the moment it returns,
// because dispatch runs on the authority goroutine and this does not. Its one job
// is to keep the config watcher from rebuilding a deferred candidate on every
// tick, which would spend a credential check and a tracker round trip per tick to
// be told what it already knows (config.WatchOptions.Quiescent). AdoptIdentity is
// the authority.
//
// It is the *same predicate* the barrier applies, read from a published mirror
// rather than recomputed — which is the whole point, and was the bug. An advisory
// that answered "quiescent" while the barrier refused over an unresolved candidate
// scan or outstanding §9.10 step 5 debt made the watcher rebuild on every tick and
// be refused on every tick: the recurring cost the advisory exists to remove,
// arrived at by disagreeing with the authority instead of by not having one.
func (o *Orchestrator) IdentityQuiescent() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return !o.identityWork
}

// publishIdentityWork refreshes the mirror IdentityQuiescent reads. Called from
// the loop after every signal, and from Recover, which establishes the scan and
// sweep debt before any loop exists.
func (o *Orchestrator) publishIdentityWork() {
	work := o.identityWorkOutstanding()
	o.mu.Lock()
	o.identityWork = work
	o.mu.Unlock()
}

// adoption is one publication awaiting the loop's verdict.
//
// The state word is the whole arbitration. A caller that has given up and a loop
// that is about to commit must not both proceed — that publishes a configuration
// the watcher has reported as a failed reload — and neither may the commit run and
// its result be lost. So exactly one of cancellation and execution wins a
// compare-and-swap, and whichever wins, the caller reports it.
type adoption struct {
	state atomic.Int32
	// ack is buffered so the loop never blocks on a caller that has stopped
	// listening. An unbuffered one is how a timed-out adoption came to publish and
	// then wedge the authority goroutine.
	ack chan error
	// prev and next are what the publication moves between; the loop compares
	// their identities to decide whether outstanding work is an obstacle at all.
	prev, next *Bundle
	commit     func()
	// stack is a recovered panic's only diagnosis, carried out of the loop turn
	// rather than logged inside it: logging is I/O, and I/O on the authority
	// goroutine can block every run in flight.
	stack []byte
}

// The states of an adoption. `executing` exists so that `committed` can mean "the
// store has happened": set before the store, a panic in between would be reported
// as a failure with the state already claiming success. `failed` is terminal for
// the same reason in the other direction — returning to `pending` would let a
// caller whose context has since fired win the cancellation race and report
// ctx.Err() for a publication that had actually panicked.
const (
	adoptPending int32 = iota
	adoptCancelled
	adoptExecuting
	adoptCommitted
	adoptFailed
)

// AdoptIdentity runs a publication at a point linearized with this loop's own
// identity-creating work, and reports whether it ran. B11 wires it to
// config.WatchOptions.Barrier.
//
// The test and the commit are one loop step. Read from any other goroutine,
// "nothing outstanding" is answered and then invalidated: dispatch claims an issue
// on the authority goroutine, so between the answer and the publication a claim can
// exist under the principal being replaced. On the loop, no claim can land in
// between — beginClaim and the held-claim conversion run there too.
//
// Only an *identity* change is gated on the record set. A policy edit, a hook
// change or a credential rotation that keeps the principal has nothing for a claim
// to be bound to, and §5.4 gives all of them to the reload however busy the daemon
// is; gating those too would mean no reload lands at all while any work exists.
func (o *Orchestrator) AdoptIdentity(ctx context.Context, prev, next *Bundle, commit func()) error {
	a := &adoption{ack: make(chan error, 1), prev: prev, next: next, commit: commit}
	o.send(ctx, signal{kind: sigAdopt, adopt: a})

	err := o.awaitAdoption(ctx, a)
	if a.stack != nil {
		// Logged here, on the caller's goroutine, for the reason the stack is
		// carried at all.
		o.log.Error("panic while adopting a configuration; keeping the last-known-good",
			"error", err, "stack", string(a.stack))
	}
	return err
}

func (o *Orchestrator) awaitAdoption(ctx context.Context, a *adoption) error {
	select {
	case err := <-a.ack:
		return err
	case <-ctx.Done():
		if a.state.CompareAndSwap(adoptPending, adoptCancelled) {
			// Cancellation won: the loop will find the state moved and will not
			// commit.
			return ctx.Err()
		}
		// The loop got there first, so the outcome is its to report and ours to
		// wait for — a commit that ran must never be reported as a failure, and a
		// commit that panicked must not be reported as a timeout.
		return <-a.ack
	}
}

// onAdopt is the loop's half of AdoptIdentity: the emptiness test and the commit,
// in one turn, with nothing fallible between the store and the state that says it
// happened.
func (o *Orchestrator) onAdopt(a *adoption) {
	if o.identityMoved(a) && o.identityWorkOutstanding() {
		if a.state.CompareAndSwap(adoptPending, adoptCancelled) {
			a.ack <- fmt.Errorf("%w: %d run records, %d held claims, %d pending run-marker clears, "+
				"recovery scan outstanding: %v, workspace cleanup outstanding: %v",
				config.ErrWorkOutstanding, len(o.records), len(o.held), len(o.markerClears),
				o.scanOwed || o.scanInFlight, o.sweepOwed())
		}
		return
	}
	if !a.state.CompareAndSwap(adoptPending, adoptExecuting) {
		// The caller gave up. Committing now would publish a configuration it has
		// already reported as unadopted.
		return
	}
	a.ack <- o.runCommit(a)
}

// identityWorkOutstanding reports whether anything is bound to the identity now in
// force.
//
// The record and held sets are the obvious half. The §9.10 candidate scan is the
// other, and it is not obvious at all: an unresolved scan means this daemon does not
// yet *know* what it holds, and an empty record set then says "nothing outstanding"
// when the truth is "nothing discovered". Adopting a new identity there moves the
// workspace root out from under claims about to be classified — and the next pass
// would read the new root's absent marker as a free workspace while the old root's
// process is still running, which is a second agent in a live worktree by way of a
// reload.
//
// Step 5's unfinished cleanup is the third, and it is outstanding for a reason of
// the same shape. A deferred workspace is a *promise*: §9.10 says such a directory
// is "left in place and swept once that run is confirmed gone", and the only thing
// that can keep it is a daemon still addressing the root it was found under. Let an
// identity change past while that debt stands and it is not deferred but dropped —
// retrySweep can only scan the new root, and once this process exits nothing ever
// looks at the old one again.
//
// An owed marker removal is the fourth, and it is the one that is *not* about
// finishing the work: it is about the root string being a weaker key than the
// directory it names. Two roots that differ only by a symlink — `/var/x` and
// `/private/var/x` — address one file, and markerStore compares strings, so a
// root-alias reload would leave a clear neither abandoned nor awaited and its retry
// would delete a live run's marker. Refusing the move while any clear is owed is what
// lets that comparison stay a string comparison on the authority goroutine; see
// markerStore for why canonicalizing there is not an option.
func (o *Orchestrator) identityWorkOutstanding() bool {
	return len(o.records) > 0 || len(o.held) > 0 ||
		o.scanOwed || o.scanInFlight || o.sweepOwed() || len(o.markerClears) > 0
}

// sweepOwed reports whether §9.10 step 5 has unfinished work: a whole pass that never
// ran, individual workspaces whose answer may still change, one in flight, or a
// workspace whose owner has gone since the pass that skipped it.
func (o *Orchestrator) sweepOwed() bool {
	return o.sweepPassOwed || len(o.sweepDeferred) > 0 || o.sweepInFlight || o.sweepHandbackOwed()
}

// sweepHandbackOwed reports whether any workspace a pass declined to examine has lost
// the record or held claim it was left to.
//
// This is the whole of the handback mechanism's bookkeeping: a skipped ref is *owed*
// exactly when nothing owns its issue any more, so the question is asked of the record
// set rather than answered by a flag something has to remember to set. Nothing hooks
// `forget`, and nothing can therefore forget to.
func (o *Orchestrator) sweepHandbackOwed() bool {
	for id := range o.sweepSkipped {
		if _, tracked := o.records[id]; tracked {
			continue
		}
		if _, retained := o.held[id]; retained {
			continue
		}
		return true
	}
	return false
}

// identityMoved reports whether this publication changes what outstanding work is
// bound to. A publication with nothing to compare against — no previous runtime —
// is treated as a move, which is the fail-closed direction.
func (o *Orchestrator) identityMoved(a *adoption) bool {
	if a.prev == nil || a.next == nil {
		return true
	}
	return a.prev.Identity() != a.next.Identity()
}

// runCommit performs the single store and records that it happened.
//
// The panic net is here and not at the watcher, because this runs on the authority
// goroutine: the watcher's own recovery is on the goroutine that called
// AdoptIdentity and cannot see this one, and an unrecovered panic here takes the
// daemon down with every in-flight run attached, which §5.4 forbids outright.
//
// It records `failed`, never `pending`: the publication has been attempted, and a
// state that says otherwise would let a caller whose context fired meanwhile win
// the cancellation race and report a timeout for a panic. Nothing is logged from
// here — the stack goes back with the error and is written outside the loop turn.
func (o *Orchestrator) runCommit(a *adoption) (err error) {
	defer func() {
		if r := recover(); r != nil {
			a.stack = debug.Stack()
			a.state.Store(adoptFailed)
			err = fmt.Errorf("%w: %v", config.ErrReloadPanic, r)
		}
	}()
	a.commit()
	a.state.Store(adoptCommitted)
	return nil
}

// newToken mints an identity for one *tenure* of ownership over an issue: a
// run record from dispatch to its exit, or a held claim from conversion to
// its release. Unique for the life of the process, and drawn from one
// sequence, so no two owners — past or present, record or held — ever share
// one.
//
// It is what `generation` cannot be. Generation counts attempts *within* a
// record and restarts at zero for the next record on the same issue, so it
// can only tell a superseded attempt from a live one; it cannot tell one
// record's work from its successor's. Most workers do not need more, because
// Record.pending refuses to forget a record while one is out. The exceptions
// are the results that outlive that guarantee: see deliverable.
func (o *Orchestrator) newToken() int {
	o.nextToken++
	return o.nextToken
}

// limits are the run limits in force. Single-moment decisions — is another
// attempt owed, how long is the next wait — read them here; a decision that
// spans several records takes a snapshot instead (see configNow).
func (o *Orchestrator) limits() config.LimitsConfig { return o.definition().Config.Limits }

// pollWait is the tick cadence currently in force, plus the channel that says
// it has changed. One acquisition of the source gives both, so a reload landing
// between them cannot leave the ticker asleep on the old cadence holding a wake
// that has already fired (SPEC §5.4).
func (o *Orchestrator) pollWait() (time.Duration, <-chan struct{}) {
	snap, wake := o.cfg.Runtime.Load()
	return time.Duration(snap.Definition.Config.Polling.IntervalMS) * time.Millisecond, wake
}

// revalidate runs the §9.4 step 2 backstop and reports what is in force
// afterwards. A nil hook means never blocked and never reloaded — the shape
// B12's fakes and unit tests want — so what is in force is simply what already
// was.
func (o *Orchestrator) revalidate(ctx context.Context) snapshot {
	if o.cfg.Revalidate == nil {
		return o.configNow()
	}
	return o.cfg.Revalidate(ctx)
}

// send posts a signal without blocking the caller past cancellation.
func (o *Orchestrator) send(ctx context.Context, s signal) {
	select {
	case o.signals <- s:
	case <-ctx.Done():
	}
}

// enqueue schedules a tracker side effect on the serial queue. It reports
// whether the queue accepted it: an operation that latches state on "I have
// started this" must not latch when the queue dropped it.
func (o *Orchestrator) enqueue(ctx context.Context, fn func(context.Context)) bool {
	select {
	case o.effects <- fn:
		return true
	case <-ctx.Done():
		return false
	default:
		// A full queue means the tracker is not keeping up. Dropping a label
		// write silently would desynchronize the projection, so say so; the
		// next transition for the issue re-asserts it.
		o.log.Error("tracker effect queue full; a label or comment write was dropped")
		return false
	}
}

// transition applies one edge of the §9.2 map, logs it (§9.11), and projects
// the consequences onto the tracker (§9.3, §8.4).
func (o *Orchestrator) transition(ctx context.Context, r *Record, to State, reason string) error {
	return o.transitionCaused(ctx, r, to, reason, "")
}

// transitionCaused is transition for the edges a failure caused, carrying the
// §7.3 verdict that caused them into the log.
//
// The cause is a parameter rather than a read of Record.FailureReason, and that
// is the whole point of the second function. That field is sticky — "the §7.3
// verdict of the most recent failure, if any" — and survives into the retry it
// triggers, so reading it here would stamp `crashed` on the `preparing`,
// `running` and `done` of the attempt that recovered from the crash. §9.10
// step 6 reads this field back out of the log to name a failure, and it would
// name that one.
func (o *Orchestrator) transitionCaused(ctx context.Context, r *Record, to State, reason string, cause core.FailureReason) error {
	from := r.State
	if !Allowed(from, to) {
		err := &IllegalTransitionError{Issue: r.Issue.Identifier, From: from, To: to}
		// SPEC §9.2: an illegal transition is a bug, not a no-op.
		o.log.Error("illegal state transition", "issue", r.Issue.Identifier,
			"from", from, "to", to, "error", err)
		return err
	}

	r.State = to
	r.UpdatedAt = o.clock.Now()
	o.Transitions.append(TransitionEntry{
		TS: r.UpdatedAt, Issue: r.Issue.Identifier, From: from, To: to,
		Actor: o.cfg.DaemonID, Reason: reason,
		RunID: r.runID(), FailureReason: cause,
	})
	o.publish(r)

	if want := stateLabel(to); want != stateLabel(from) {
		// Every projection is owed, not fired and forgotten. §9.10's table
		// turns on which label is standing, so a write that quietly failed
		// would leave recovery reading a state this daemon never reached.
		issue, what := r.Issue, "project "+labelName(want)
		if from == StateQueued && to == StateClaimed {
			what = effectClaimLabel
		}
		o.oweProjection(ctx, r, what, func(ctx context.Context, o *Orchestrator) error {
			return o.bundle().Tracker.SetStateLabels(ctx, issue, want)
		})
	}
	return nil
}

func labelName(l core.StateLabel) string {
	if l == core.StateLabelNone {
		return "no state label"
	}
	return "ben:" + string(l)
}

// comment posts one of the four milestones (SPEC §8.4), on the same serial
// queue as the label writes so it lands after the transition that owns it.
func (o *Orchestrator) comment(ctx context.Context, r *Record, c core.MilestoneComment) {
	issue := r.Issue
	o.owe(ctx, r, "comment "+string(c.Milestone), effectTracker, func(ctx context.Context, o *Orchestrator) error {
		return o.bundle().Tracker.Comment(ctx, issue, c)
	})
}

func (o *Orchestrator) publish(r *Record) {
	o.mu.Lock()
	o.published[r.Issue.Identifier] = r.snapshot()
	o.mu.Unlock()
}

// forget drops the local record. Only ever called once the record owes the
// tracker nothing: an unlanded write would otherwise be lost with it.
func (o *Orchestrator) forget(identifier string) {
	delete(o.records, identifier)
	o.mu.Lock()
	delete(o.published, identifier)
	o.mu.Unlock()
}

// release drops the claim — an exit from the machine, not a state
// (SPEC §9.2). The record is kept until the tracker confirms it, and retried
// on later ticks if it does not: forgetting first would strand the claim on a
// single failed write, and a stranded claim blocks the issue for everyone
// (§8.3).
func (o *Orchestrator) release(ctx context.Context, r *Record, why string) {
	if r.releasing {
		return
	}
	r.releasing = true
	r.stopReason = why
	// A tracker release is the last local chance to retire a pre-pin intent.
	// Keep the ordering here rather than at selected callers: terminal Prepare
	// failures release directly, while the ordinary exit and recovery paths may
	// already have queued the same idempotent cleanup earlier.
	o.abandonPendingClaimBase(ctx, r)
	issue := r.Issue
	o.owe(ctx, r, effectRelease, effectTracker, func(ctx context.Context, o *Orchestrator) error {
		return o.bundle().Tracker.Release(ctx, issue)
	})
}

// attemptEnded fires the §6.5 after-run hook, exactly once per attempt that
// actually started a process.
//
// "Per attempt" is the unit that matters: the hook exists to run after an
// agent has finished with the worktree, so it owes one call to every attempt
// that had one — including the retries and continuations that go on to
// prepare again — and none to an attempt that never launched. The flags reset
// in beginPrepare, which is where the next attempt begins.
func (o *Orchestrator) attemptEnded(ctx context.Context, r *Record) {
	if !r.ranThisAttempt || r.afterRunFired {
		return
	}
	r.afterRunFired = true

	// Whether this provider family has an after-run hook at all, asked now because
	// an effect owed to a provider that has none would never land.
	if _, ok := o.bundle().Workspaces.(afterRunner); !ok || !r.hasWorkspace() {
		return
	}
	ws := r.Workspace
	o.owe(ctx, r, "after_run hook", effectLocal, func(ctx context.Context, o *Orchestrator) error {
		// The instance is resolved when the call fires, not when it was queued:
		// §5.4's linearization point for a hook is the entry of the provider call
		// that runs it, and an effect can sit behind a slow tracker write for
		// several ticks. An edit that landed in that window is the hook this
		// attempt should run.
		hook, ok := o.bundle().Workspaces.(afterRunner)
		if !ok {
			return nil
		}
		hook.AfterRun(ctx, ws)
		return nil
	})
}

// dispose returns the workspace, keeping it for forensics where the spec says
// to. Once per record — see Record.disposalOwed for why the exit can be reached
// twice and what a second disposal would cost.
func (o *Orchestrator) dispose(ctx context.Context, r *Record, keep bool) {
	if !r.hasWorkspace() || r.disposalOwed {
		return
	}
	r.disposalOwed = true
	ws := r.Workspace
	o.owe(ctx, r, "dispose workspace", effectLocal, func(ctx context.Context, o *Orchestrator) error {
		return o.bundle().Workspaces.Dispose(ctx, ws, keep)
	})
}

// abandonPendingClaimBase queues the local half of ending a claim. It runs
// after any earned after_run hook and before tracker release, while the
// workspace is confirmed quiet. Pinned state is a provider no-op and remains
// the outgoing fact for the next assignment (SPEC §6.2, §9.8).
func (o *Orchestrator) abandonPendingClaimBase(ctx context.Context, r *Record) {
	if r.claimBaseAbandonOwed {
		return
	}
	r.claimBaseAbandonOwed = true
	issue := r.Issue
	o.owe(ctx, r, effectAbandonClaimBase, effectLocal, func(ctx context.Context, o *Orchestrator) error {
		return o.bundle().Workspaces.AbandonPendingClaimBase(ctx, issue)
	})
}

func (o *Orchestrator) runningCount() int {
	n := 0
	for _, r := range o.records {
		// The cap is on live agent processes, the one scarce resource
		// (SPEC §9.5). Claiming and preparing count: they are on their way to
		// being one, and letting them over-subscribe would only push the
		// breach a moment later.
		switch r.State {
		case StateQueued, StateClaimed, StatePreparing, StateRunning:
			// queued counts: a record only exists in that state because
			// dispatch selected it and its claim is in flight. Not counting
			// it would let the whole eligible queue past the cap before the
			// first claim came back.
			n++
		case StateVerifying:
			// The one state on the way *out*, so the reasoning inverts. The
			// state moves on the terminal event, which is §9.2's trigger, but
			// the process may outlive it — a descendant in the group is what
			// §7.5's Stop is asked about — so it counts until the group is
			// confirmed gone.
			//
			// After that it does not. The §9.7 evidence check reads git and
			// the tracker with no agent running at all, and it can be slow:
			// it probes origin and calls FindPR. Holding a slot for it caps
			// concurrency on something that is not the scarce resource, and
			// with a slow tracker a full set of verifying records starves
			// dispatch entirely while nothing is executing.
			if !r.groupGone {
				n++
			}
		}
	}
	return n
}

// freeSlots is capacity under one definition, taken by the caller: the cap is
// asked once per candidate in a dispatch pass, and all of them must be judged
// against the same one.
func (o *Orchestrator) freeSlots(def *config.WorkflowDefinition) int {
	free := def.Config.Limits.MaxConcurrentAgents - o.runningCount()
	if free < 0 {
		return 0
	}
	return free
}

// renderPrompt builds the attempt's prompt from the record's own definition
// snapshot (SPEC §5.4: a reload never changes a live run's ground).
func (o *Orchestrator) renderPrompt(r *Record) (string, error) {
	vars := template.Vars{
		Issue:     r.Issue,
		Attempt:   r.Attempt,
		Workspace: r.Workspace.Path,
		Run: template.Run{
			ID:              fmt.Sprintf("%s-%d", r.Issue.Identifier, r.Attempt),
			PreviousOutcome: previousOutcome(r),
			// Read from the record, never derived here: composing it at render
			// would read a workspace this attempt's Prepare has already reattached
			// and its hooks may still be touching (SPEC §5.6, see summary.go).
			PreviousAttempt: r.previousAttempt,
		},
	}
	// Through the definition, which applies its own `limits.max_prompt_bytes`
	// (SPEC §5.6, #50). The definition is the record's snapshot, so the ceiling
	// a live run renders under is the one its claim was taken against, not
	// whatever a later reload installed.
	return r.Definition.RenderPrompt(vars)
}

// previousOutcome is recorded from an actual attempt outcome rather than
// inferred from the attempt number. A git-derived floor can make attempt 2 the
// first dispatch in a fresh record, and §5.6 requires null in that case.
func previousOutcome(r *Record) string {
	return r.lastOutcome
}
