package airlock

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Options are what one Airlock backend is constructed from. Every field is a
// value the loader validated or an instance assembly built; nothing here is read
// from the process environment, which is what keeps a workflow the single
// statement of what a daemon does (SPEC §3.7).
type Options struct {
	// BaseURL is the cluster-internal Airlock endpoint. HTTPS, and refused if it
	// embeds credentials.
	BaseURL string
	// Auth mints the bearer token, per request and from its own cache. The
	// cached view rather than the fresh one: this client makes several calls per
	// tick per claim, and an exchange per request would multiply the issuer's
	// traffic by the daemon's — the same call trackerOptions makes.
	Auth core.Source
	// Profile is the operator-approved profile a sandbox is provisioned from.
	// BEN never submits a pod or sandbox spec; it names this and Airlock pins an
	// immutable revision.
	Profile string
	// TLS is the client's verification material. Nil uses the host's roots.
	TLS *tls.Config
	// Timeouts are the client's own clocks, never a run's.
	Timeouts Timeouts
	// Retention is the sandbox lifetime policy and the end-of-claim disposal.
	Retention Retention
	// Store holds the two durable facts this package owns.
	Store Store
	// Transport substitutes the HTTP round tripper. For the contract fake, which
	// is an in-process server; a production assembly leaves it nil.
	Transport http.RoundTripper
	// Labels are extra opaque labels attached to every run. Operator-chosen and
	// never derived from issue content.
	Labels map[string]string
}

// Substrate is the constructed Airlock backend: the three #192 seams over one
// endpoint, one credential and one approved profile.
type Substrate struct {
	Workspaces *Workspaces
	Processes  *Processes
	Hooks      *Hooks

	client  *client
	store   Store
	profile string
	retain  Retention
	binding SubstrateBinding

	// limits is the *current* revision's stdin envelope as Ready read it. It
	// serves the assembly's configuration check and nothing at dispatch: a run
	// is judged by the revision its sandbox is pinned to, which is recorded on
	// the sandbox (SandboxRecord.Limits), not by whatever the profile says now.
	mu     sync.Mutex
	limits ProfileLimits
}

// New validates the options and builds the backend. Pure apart from
// constructing an HTTP client: no request is made until Ready.
func New(opts Options) (*Substrate, error) {
	if strings.TrimSpace(opts.Profile) == "" {
		return nil, fmt.Errorf("%w: no approved profile named", ErrConfig)
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: no durable store", ErrConfig)
	}
	timeouts := opts.Timeouts.withDefaults()
	if timeouts.PollWait >= timeouts.Poll {
		// The server would still be holding the poll open when the client gave
		// up on it, so every long poll would look like a transport failure.
		return nil, fmt.Errorf("%w: the long-poll wait (%s) must be shorter than the long-poll request timeout (%s)",
			ErrConfig, timeouts.PollWait, timeouts.Poll)
	}
	c, err := newClient(opts.BaseURL, opts.Auth, opts.TLS, timeouts, opts.Transport)
	if err != nil {
		return nil, err
	}
	binding, err := clientSubstrateBinding(c)
	if err != nil {
		return nil, err
	}
	s := &Substrate{
		client:  c,
		store:   opts.Store,
		profile: strings.TrimSpace(opts.Profile),
		retain:  opts.Retention,
		binding: binding,
	}
	s.Workspaces = &Workspaces{
		client: c, store: opts.Store, profile: s.profile, binding: binding,
		settle: timeouts.Settle, poll: settlePollInterval, retain: opts.Retention,
		sleep: c.sleep,
	}
	s.Processes = &Processes{
		client: c, store: opts.Store, timeouts: timeouts, binding: binding,
		labels: opts.Labels, stdinOffsets: map[string]int64{}, workspaces: s.Workspaces,
	}
	s.Hooks = &Hooks{client: c, store: opts.Store, timeouts: timeouts, binding: binding}
	return s, nil
}

// settlePollInterval is how often a settling sandbox or a pending deletion is
// re-read. Short enough that acquiring a warm sandbox is not dominated by it,
// long enough that a slow provision does not become a request storm.
const settlePollInterval = 2 * time.Second

// Ready proves the backend can serve this workflow here and now: the endpoint
// answers, the credential is accepted, and the named profile is one this tenant
// may actually provision.
//
// The profile check is the one worth having. An operator withdrawing a profile
// is the change most likely to break a working deployment, and without this it
// would surface as a failed claim at the first dispatch rather than as a refused
// reload — the same reason CheckBaseCache runs at build time on the local path.
func (s *Substrate) Ready(ctx context.Context) error {
	var profile Profile
	err := s.client.do(ctx, request{
		method: "GET", path: "/v2/profiles/" + url.PathEscape(s.profile), out: &profile,
	})
	if err != nil {
		return fmt.Errorf("%w: profile %q: %w", ErrUnready, s.profile, err)
	}
	if !profile.Provisionable() {
		return fmt.Errorf("%w: profile %q is %s and cannot be provisioned", ErrUnready, s.profile, profile.Status)
	}
	// The current revision's stdin envelope, kept for the assembly's
	// configuration check. Dispatch does not read it: a sandbox is pinned to
	// the revision it was created against and its runs are judged by that
	// revision's limits, which Workspaces.learnLimits records on the sandbox.
	s.mu.Lock()
	s.limits = profile.Limits
	s.mu.Unlock()
	return nil
}

// StdinLimits is one revision's stdin envelope, reduced to what BEN plans a
// prompt's delivery by (#284). The zero value is unknown; Known reports which.
type StdinLimits struct {
	// Inline is the largest decoded prompt that travels in the start request
	// itself. Above it a prompt streams. Zero means every non-empty prompt
	// streams.
	Inline int64
	// Chunk is the largest single streaming write, decoded.
	Chunk int64
	// Total is the largest prompt a run may receive at all, by either path.
	// Zero is unbounded, which is how the server reads it too.
	Total int64
	// RequestBody is the largest whole request body, enforced before parsing.
	// An inline prompt is base64 inside JSON, so this can refuse a body whose
	// decoded prompt is under Inline; and each streaming write is a body too.
	RequestBody int64
}

// Known reports whether a profile has been read. The chunk limit is the
// witness: the contract's schema gives it a minimum of one, so a published
// profile can never legitimately report zero.
func (l StdinLimits) Known() bool { return l.Chunk > 0 }

// Admits reports whether a prompt of n bytes can be delivered at all.
func (l StdinLimits) Admits(n int64) bool { return l.Total <= 0 || n <= l.Total }

// stdinLimitsOf projects a recorded envelope. Nil is unknown.
func stdinLimitsOf(l *ProfileLimits) StdinLimits {
	if l == nil {
		return StdinLimits{}
	}
	return StdinLimits{
		Inline: l.MaxStdinInlineBytes, Chunk: l.MaxStdinChunkBytes,
		Total: l.MaxStdinTotalBytes, RequestBody: l.MaxRequestBodyBytes,
	}
}

// StdinLimits reports the current revision's stdin envelope as Ready read it,
// for an assembly that composes prompts and has to know before the first
// dispatch whether its largest one can be delivered at all. It is the profile
// a *new* sandbox would pin; an existing sandbox is judged by its own recorded
// envelope, and a refusal there is a surfaced, definite answer rather than a
// startup fact.
func (s *Substrate) StdinLimits() StdinLimits {
	s.mu.Lock()
	defer s.mu.Unlock()
	limits := s.limits
	return stdinLimitsOf(&limits)
}

// RunnerConfig is what remote.New needs beyond this backend: the durable
// journal store, BEN's event consumer, and the provider adapter's own argv and
// translator.
//
// Translate is the *adapter's* function, not one this package supplies. That is
// the boundary: Airlock sees opaque process bytes, and the only thing entitled
// to read a provider record is the adapter that wrote the argv producing it.
type RunnerConfig struct {
	Journals     remote.Store
	Consumer     remote.DurableConsumer
	Bind         remote.Binder
	Invoke       remote.Invoker
	Translate    remote.Translator
	Capabilities core.Capabilities
	StopGrace    time.Duration
	// Reconnect bounds how long an attempt waits out an unreadable event
	// stream. The zero value is remote's default policy. The client's own
	// per-request retries are the short end of the same problem: they absorb a
	// blip, this absorbs an API rollout (#275).
	Reconnect remote.ReconnectPolicy
	// Logger is where an attempt reports event reads it could not complete.
	Logger *slog.Logger
}

// Runner composes this backend into a core.AgentRunner. There is no second
// control loop: the orchestrator's states, retries and budgets are reached
// unchanged (SPEC §7.1, §9).
func (s *Substrate) Runner(cfg RunnerConfig) (*remote.Runner, error) {
	return remote.New(remote.Options{
		Backend:      s.Processes,
		Store:        cfg.Journals,
		Consumer:     cfg.Consumer,
		Bind:         cfg.Bind,
		Invoke:       cfg.Invoke,
		Translate:    cfg.Translate,
		Capabilities: cfg.Capabilities,
		Ready:        s.Ready,
		StopGrace:    cfg.StopGrace,
		Reconnect:    cfg.Reconnect,
		Logger:       cfg.Logger,
	})
}

// Outcome is why a claim cycle is finished with its workspace. It selects a
// Disposal from the configured Retention.
type Outcome uint8

const (
	// OutcomeRetry is another attempt of the same claim. The workspace is
	// always reused: SPEC §6.2 reattaches rather than recreating, and a retry
	// that rebuilt the tree would discard the prior attempt's work that §9.6's
	// continuation prompt reports on.
	OutcomeRetry Outcome = iota
	// OutcomePublished is a claim whose publication the daemon-side verifier
	// confirmed (#193, SPEC §9.7). Never an Airlock success response: a run
	// reporting that it pushed is the run's own assertion.
	OutcomePublished
	// OutcomeFailed is a failed or attempt-exhausted claim.
	OutcomeFailed
	// OutcomeRevoked is a claim taken away — the label removed, the assignment
	// moved, the issue closed (SPEC §9.5, §9.9).
	OutcomeRevoked
	// OutcomeShutdown is the daemon stopping with the claim still held.
	OutcomeShutdown
)

func (o Outcome) String() string {
	switch o {
	case OutcomeRetry:
		return "retry"
	case OutcomePublished:
		return "published"
	case OutcomeFailed:
		return "failed"
	case OutcomeRevoked:
		return "revoked"
	case OutcomeShutdown:
		return "shutdown"
	default:
		return fmt.Sprintf("Outcome(%d)", uint8(o))
	}
}

// Disposal reports what the configured policy does at an outcome. A retry is
// not configurable and always retains.
func (r Retention) Disposal(o Outcome) Disposal {
	switch o {
	case OutcomePublished:
		return r.OnSuccess
	case OutcomeFailed:
		return r.OnFailure
	case OutcomeRevoked:
		return r.OnRevoked
	case OutcomeShutdown:
		return r.OnShutdown
	}
	return DisposalRetain
}

// Complete applies the configured disposal to a claim's workspace, and refuses
// to touch it while the run's termination is unconfirmed.
//
// The gate is remote.MayReuse and it is not optional. `prev` is the last status
// observed for the claim's run; only an explicit domain-quiet observation from a
// reachable backend authorizes suspending or deleting the tree a possibly-live
// foreign process may still be writing to (SPEC §9.8). "The stream ended" and
// "the backend did not answer" are both one step from correct and both fail
// open, which is why neither is spelled here.
func (s *Substrate) Complete(
	ctx context.Context, claim remote.Claim, outcome Outcome, prev remote.Status,
) (Disposal, error) {
	disposal := s.retain.Disposal(outcome)
	if disposal == DisposalRetain {
		return disposal, nil
	}
	if !remote.MayReuse(prev) {
		return disposal, fmt.Errorf("%w: %s is %s and %s was requested", remote.ErrNotQuiet, claim, prev.Phase, disposal)
	}
	switch disposal {
	case DisposalSuspend:
		return disposal, s.Workspaces.Suspend(ctx, claim)
	case DisposalDelete:
		// Through remote.Dispose rather than straight to Delete: the gate is
		// applied at the boundary as well as here, so a future caller that
		// reached the backend directly still cannot remove a workspace out from
		// under a run.
		return disposal, remote.Dispose(ctx, s.Workspaces, claim, prev)
	}
	return disposal, fmt.Errorf("airlock: %s is not a disposal this backend implements", disposal)
}

// ClaimState is one claim cycle as startup reconciliation found it: what BEN's
// two durable records say, what Airlock says, and what the pair costs against
// the concurrency cap.
//
// Err is a field rather than a return, because reconciliation is a survey and
// one unreadable record must not hide the rest. A state carrying an error has
// been *reported*, never resolved: nothing in this package acts on it, and the
// daemon parks it.
type ClaimState struct {
	Claim remote.Claim
	// Record is the sandbox-record basename. It identifies a corrupt entry
	// whose Claim could not be decoded, so reconciliation can report that entry
	// individually instead of failing or silently skipping it.
	Record   string
	Identity remote.Identity
	// Sandbox is Airlock's current state for the recorded sandbox, or "" when
	// there is no sandbox to ask about.
	Sandbox SandboxState
	// ActiveRunID is the run holding the sandbox's single active slot, or "".
	// Read for the survey only: BEN addresses its own run through the journal,
	// and a slot held by something it cannot name is exactly what parks a claim.
	ActiveRunID string
	// Dispatched, Cursor and Terminal come from remote.Record: whether a start
	// was attempted, how far the event log was durably consumed, and whether a
	// normalized outcome was already accepted.
	Dispatched bool
	Cursor     int64
	Terminal   bool
	// StartUnresolved means the write-ahead start fence exists but no permanent
	// run id was learned. It is an expected recovery state, not a readiness
	// failure: this claim remains occupied when the read-only startup survey
	// cannot recover a request-bound active run. Exact replay belongs behind the
	// orchestrator's tracker, §9.7 orphan/backoff and approval-cycle gates.
	StartUnresolved bool
	// Status is fresh evidence for the journal's run, when one can be named. The
	// zero value is an unreachable backend and reports unconfirmed.
	Status remote.Status
	// Lease is what this claim costs against `limits.max_concurrent_agents` on
	// this substrate (remote.LeaseStateOf).
	Lease remote.LeaseState
	Err   error
}

// Reconcile surveys every retained claim before ordinary dispatch resumes.
//
// It is a read, and deliberately only a read. §9.10's rule is that the tracker
// and git are authority and a daemon reconstructs rather than assumes; what this
// adds is the two questions only Airlock can answer — does the recorded sandbox
// still exist and belong to us, and is its single active slot held — and the two
// only BEN's journal can — was a start attempted, and how far was its event log
// consumed. Deciding what to *do* with the answer is the orchestrator's, because
// the decision needs the tracker's view of the claim and this package has none.
func (s *Substrate) Reconcile(ctx context.Context, journals remote.Store) ([]ClaimState, error) {
	claims, err := s.store.Claims()
	if err != nil {
		return nil, err
	}
	states := make([]ClaimState, 0, len(claims))
	for _, claim := range claims {
		if claim.Err != nil {
			// The unreadable record may name a live sandbox or run. It cannot say
			// which, but both cost one slot; treating corruption as LeaseNone would
			// turn a failed survey into new dispatch capacity.
			states = append(states, ClaimState{
				Claim: claim.Claim, Record: claim.Record, Lease: remote.LeaseHeld, Err: claim.Err,
			})
			continue
		}
		state := s.reconcileClaim(ctx, journals, claim.Claim)
		state.Record = claim.Record
		states = append(states, state)
	}
	return states, nil
}

// reconcileClaim surveys one claim, and the order of its reads is the whole
// design: the two local records come first and the two backend reads after.
//
// That is what makes the cost correct when the survey fails. The lease is
// established from BEN's own durable state against the *zero* Status — which
// reports unconfirmed — before Airlock is asked anything, so a claim whose
// control plane is unreachable costs what it might cost rather than nothing. A
// survey that freed capacity by failing would let an unreachable backend
// dissolve `limits.max_concurrent_agents`.
func (s *Substrate) reconcileClaim(ctx context.Context, journals remote.Store, claim remote.Claim) ClaimState {
	// Listing proved a retained record exists. Hold its slot until a readable
	// record proves a narrower state; a read failure must not free capacity.
	state := ClaimState{Claim: claim, Lease: remote.LeaseHeld}
	rec, err := loadBoundSandbox(s.store, s.binding, claim)
	if err != nil {
		// No readable record: this daemon can name nothing to release, but the
		// retained entry may still represent a live allocation, so the
		// conservative lease established above stands.
		state.Err = err
		return state
	}
	state.Identity = rec.Identity()

	journal, journalErr := journals.Load(claim)
	switch {
	case journalErr == nil:
		state.Dispatched, state.Cursor, state.Terminal = journal.Dispatched, journal.Cursor, journal.Terminal
	case errors.Is(journalErr, remote.ErrNoRecord):
		// A workspace with no dispatch journal: acquired, and nothing was ever
		// started in it. It holds a lease and no run.
		journal = remote.Record{Identity: state.Identity}
	default:
		state.Err = journalErr
		journal = remote.Record{Identity: state.Identity}
	}
	journal.Identity = state.Identity
	state.Lease = remote.LeaseStateOf(journal, remote.Status{})

	if rec.SandboxID != "" {
		sandbox, err := s.Workspaces.get(ctx, rec.SandboxID)
		switch {
		case hasCode(err, CodeNotFound), hasCode(err, CodeForbidden):
			// A record naming something this principal cannot act on. Reported,
			// never repaired: deleting the record would silently release a
			// workspace that may be somebody else's, and adopting the sandbox
			// would be worse.
			state.setErr(s.Workspaces.classify(rec, err))
			return state
		case err != nil:
			state.setErr(err)
			return state
		}
		state.Sandbox = sandbox.State
		if sandbox.ActiveRunID != nil {
			state.ActiveRunID = *sandbox.ActiveRunID
		}
		if err := s.Workspaces.checkPin(rec, rec.ProfileRevision, sandbox); err != nil {
			state.setErr(err)
			return state
		}
	}

	if journal.Dispatched {
		st, err := s.Processes.Status(ctx, journal.ProcessRef())
		switch {
		case errors.Is(err, remote.ErrNoProcess):
			// The Airlock start-binding fence is absent, so the request never
			// crossed it. remotews will retire this journal from the same fact.
		case errors.Is(err, remote.ErrProcessUnresolved):
			// A retained claim in this state must not refuse unrelated work at
			// daemon startup. Its zero Status keeps the lease running and its run
			// marker keeps this workspace occupied until recovery wins.
			state.StartUnresolved = true
		case err != nil:
			// Deliberately not fatal to the survey, and deliberately not
			// downgraded to "gone": the lease established above stands.
			state.setErr(err)
			return state
		default:
			state.Status = st
		}
	}
	state.Lease = remote.LeaseStateOf(journal, state.Status)
	return state
}

// setErr keeps the first refusal. A later one is a consequence of it, and
// reporting the consequence is how an operator ends up chasing the wrong thing.
func (c *ClaimState) setErr(err error) {
	if c.Err == nil {
		c.Err = err
	}
}

// Active is the v2 concurrency count over a reconciliation survey: the number of
// backend runs and leases this daemon holds.
//
// A held lease costs even with no run in it — a suspended workspace is still a
// reservation — and a dispatched run costs until its termination is *confirmed*.
// That is v1's exclusion read honestly on a substrate where the proof is domain
// quiet rather than ESRCH (remote.LeaseState).
func Active(states []ClaimState) int {
	leases := make([]remote.LeaseState, 0, len(states))
	for _, st := range states {
		leases = append(leases, st.Lease)
	}
	return remote.Active(leases)
}
