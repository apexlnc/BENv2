package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

const defaultStopGrace = 10 * time.Second

// Runner is a core.AgentRunner over a durable remote process backend.
type Runner struct {
	backend   ProcessBackend
	store     Store
	consumer  DurableConsumer
	bind      Binder
	invoke    Invoker
	translate Translator
	caps      core.Capabilities
	ready     func(context.Context) error
	stopGrace time.Duration
	reconnect ReconnectPolicy
	log       *slog.Logger
}

// Binding connects an attempt to its durable workspace and BEN-chosen run id.
type Binding struct {
	Identity Identity
	Run      RunID
	Meta     Meta
	Git      GitScope
}

type Binder func(core.RunSpec) (Binding, error)
type Invoker func(core.RunSpec) (Invocation, error)

type Invocation struct {
	Argv  []string
	Env   map[string]string
	Stdin []byte
}

// Options are the assembly seams. Consumer is required because a receive-only
// core event channel cannot acknowledge durable processing.
type Options struct {
	Backend   ProcessBackend
	Store     Store
	Consumer  DurableConsumer
	Bind      Binder
	Invoke    Invoker
	Translate Translator

	Capabilities core.Capabilities
	Ready        func(context.Context) error
	StopGrace    time.Duration
	// Reconnect bounds how long an attempt waits out a backend whose event
	// reads are failing. The zero value is the default policy.
	Reconnect ReconnectPolicy
	// Logger is where an attempt reports read failures. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

func New(opts Options) (*Runner, error) {
	switch {
	case opts.Backend == nil:
		return nil, errors.New("remote: a runner needs a process backend")
	case opts.Store == nil:
		return nil, errors.New("remote: a runner needs a durable store")
	case opts.Consumer == nil:
		return nil, errors.New("remote: a runner needs a durable event consumer")
	case opts.Bind == nil:
		return nil, errors.New("remote: a runner needs a binder")
	case opts.Invoke == nil:
		return nil, errors.New("remote: a runner needs an invoker")
	case opts.Translate == nil:
		return nil, errors.New("remote: a runner needs a translator")
	}
	grace := opts.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		backend: opts.Backend, store: opts.Store, consumer: opts.Consumer,
		bind: opts.Bind, invoke: opts.Invoke, translate: opts.Translate,
		caps: opts.Capabilities, ready: opts.Ready, stopGrace: grace,
		reconnect: opts.Reconnect.withDefaults(), log: logger,
	}, nil
}

func (r *Runner) Capabilities() core.Capabilities { return r.caps }

func (r *Runner) Ready(ctx context.Context) error {
	if r.ready == nil {
		return nil
	}
	return r.ready(ctx)
}

// Start either dispatches the exact persisted request or resolves its existing
// dispatch. Recovery deliberately lives here: core.AgentRunner has no Reattach
// method, so returning ErrAlreadyStarted would make the orchestrator discard the
// only path to an ambiguously-started process.
func (r *Runner) Start(ctx context.Context, spec core.RunSpec) (core.RunHandle, error) {
	binding, processSpec, ref, err := r.compose(spec)
	if err != nil {
		return nil, err
	}

	journal, err := OpenJournal(r.store, binding.Identity.Claim)
	switch {
	case err == nil:
		if journal.Record().ProcessRef() != ref {
			return nil, fmt.Errorf("%w: claim %s already names %s", ErrProcessMismatch, binding.Identity.Claim, journal.Record().ProcessRef())
		}
	case errors.Is(err, ErrNoRecord):
		meta := binding.Meta
		meta.replay = replaySpecOf(spec)
		journal, err = Reserve(ctx, r.store, ref, meta)
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	if journal.Record().BackendRunID == "" {
		if err := journal.RecordReplay(ctx, spec); err != nil {
			return nil, err
		}
	}

	_, dispatched, err := journal.Resume()
	if err != nil {
		return nil, err
	}
	if dispatched {
		return r.resolveExisting(ctx, journal, processSpec)
	}
	return r.dispatch(ctx, journal, processSpec)
}

func (r *Runner) compose(spec core.RunSpec) (Binding, ProcessSpec, ProcessRef, error) {
	binding, err := r.bind(spec)
	if err != nil {
		return Binding{}, ProcessSpec{}, ProcessRef{}, fmt.Errorf("remote: binding the run to a workspace: %w", err)
	}
	if !binding.Identity.Complete() {
		return Binding{}, ProcessSpec{}, ProcessRef{}, fmt.Errorf("%w: %s", ErrIdentityMissing, binding.Identity.Claim)
	}
	inv, err := r.invoke(spec)
	if err != nil {
		return Binding{}, ProcessSpec{}, ProcessRef{}, fmt.Errorf("remote: composing the harness invocation: %w", err)
	}
	processSpec := ProcessSpec{
		Identity: binding.Identity,
		Argv:     inv.Argv,
		Env:      inv.Env,
		Stdin:    inv.Stdin,
		Limits:   spec.Limits,
		Git:      binding.Git,
	}
	if err := processSpec.Git.Validate(); err != nil {
		return Binding{}, ProcessSpec{}, ProcessRef{}, err
	}
	digest, err := ProcessRequestDigest(processSpec)
	if err != nil {
		return Binding{}, ProcessSpec{}, ProcessRef{}, err
	}
	ref := ProcessRef{Identity: binding.Identity, RunID: binding.Run, RequestDigest: digest}
	return binding, processSpec, ref, nil
}

// ResolveStart reconstructs and replays an unanswered dispatch from the
// non-secret RunSpec reserved with its journal. The current provider binding
// supplies credentials; the persisted request digest proves every resulting
// byte is identical before the backend is called.
func (r *Runner) ResolveStart(ctx context.Context, ref ProcessRef) (Status, error) {
	journal, err := OpenJournal(r.store, ref.Identity.Claim)
	if err != nil {
		return Status{}, err
	}
	rec := journal.Record()
	if rec.ProcessRef() != ref {
		return Status{}, fmt.Errorf("%w: claim %s names %s, not %s",
			ErrProcessMismatch, ref.Identity.Claim, rec.ProcessRef(), ref)
	}
	if !rec.Dispatched || rec.Replay == nil {
		return Status{}, fmt.Errorf("%w: %s", ErrReplayUnavailable, ref)
	}
	_, processSpec, rebuilt, err := r.compose(rec.Replay.runSpec())
	if err != nil {
		return Status{}, err
	}
	if rebuilt != ref {
		return Status{}, fmt.Errorf("%w: reconstructed %s, want %s", ErrProcessMismatch, rebuilt, ref)
	}
	st, err := r.backend.Start(ctx, ref, processSpec)
	if err != nil {
		return Status{}, err
	}
	if st.BackendRunID == "" {
		return Status{}, fmt.Errorf("%w: exact replay of %s returned no permanent run id", ErrProcessUnresolved, ref)
	}
	if err := journal.Observe(context.WithoutCancel(ctx), st); err != nil {
		return Status{}, fmt.Errorf("remote: recording the resolved backend run id: %w", err)
	}
	return st, nil
}

func (r *Runner) dispatch(ctx context.Context, journal *Journal, spec ProcessSpec) (core.RunHandle, error) {
	st, err := journal.Dispatch(ctx, func(ctx context.Context, ref ProcessRef) (Status, error) {
		return r.backend.Start(ctx, ref, spec)
	})
	if err == nil {
		_ = journal.Observe(context.WithoutCancel(ctx), st)
		return r.attachHandle(ctx, journal)
	}
	if errors.Is(err, ErrProcessRefused) {
		// The one Start error that is not ambiguous: the backend refused the body
		// before committing anything, and would refuse the same body again. No
		// process exists to hand back a handle for, and a handle over nothing
		// would hold the claim open on an outcome that is already known. The
		// error reaches the orchestrator as a launch that never happened.
		return nil, err
	}

	// Any other Start error says nothing about whether Airlock committed the
	// keyed request. Its run id exists only in the response, so Attach cannot
	// name the unknown result. The sole safe resolution is the frozen Airlock
	// contract: replay the exact idempotency key and body and receive the stored
	// result.
	ref, _, rerr := journal.Resume()
	if rerr != nil {
		return nil, rerr
	}
	st, replayErr := r.backend.Start(ctx, ref, spec)
	if replayErr == nil {
		_ = journal.Observe(context.WithoutCancel(ctx), st)
		return r.attachHandle(ctx, journal)
	}
	if errors.Is(replayErr, ErrProcessRefused) {
		return nil, replayErr
	}
	// The replay itself is unavailable, so the run remains outcome-unknown. A
	// non-nil handle retains the durable identity; returning nil would make the
	// caller release the claim and mint a replacement. A later Runner.Start
	// replays the same request again.
	return r.attachHandle(ctx, journal)
}

func (r *Runner) resolveExisting(ctx context.Context, journal *Journal, spec ProcessSpec) (core.RunHandle, error) {
	ref, _, err := journal.Resume()
	if err != nil {
		return nil, err
	}
	backendRunID := journal.Record().BackendRunID
	if backendRunID != "" {
		st, attachErr := r.backend.Attach(ctx, ref, backendRunID)
		if attachErr == nil {
			_ = journal.Observe(context.WithoutCancel(ctx), st)
		}
		// A known resource id is permanent; replaying the creation key instead
		// would be unsafe after Airlock's idempotency window expires. Preserve an
		// unavailable attachment as an ambiguity-retaining handle.
		return r.attachHandle(ctx, journal)
	}

	// A dispatch mark without a backend id is the lost-response state. The
	// original spec is still available here, so recover the response by replaying
	// the exact creation request rather than trying to attach without an id.
	st, err := r.backend.Start(ctx, ref, spec)
	switch {
	case err == nil:
		_ = journal.Observe(context.WithoutCancel(ctx), st)
	case errors.Is(err, ErrProcessRefused):
		// Resolved, and the resolution is that nothing was ever started (see
		// dispatch). The journal keeps its mark as the record of the attempt.
		return nil, err
	}
	return r.attachHandle(ctx, journal)
}

func (r *Runner) attachHandle(ctx context.Context, journal *Journal) (core.RunHandle, error) {
	return Attach(ctx, AttemptConfig{
		Backend: r.backend, Journal: journal, Consumer: r.consumer,
		Translate: r.translate, StopGrace: r.stopGrace,
		Reconnect: r.reconnect, Logger: r.log,
	})
}

func (r *Runner) Journal(claim Claim) (*Journal, error) { return OpenJournal(r.store, claim) }
