// Package remotetest is the scripted fake behind the v2 boundary: one in-memory
// backend implementing every seam internal/remote declares, driven from a test
// (#192).
//
// It is a fake in the sense AGENTS.md means — its contract is read off the
// interfaces it stands in for, and it deliberately cannot describe a world a
// real backend could not produce. Three of those refusals are the point:
//
//   - **A disconnect is not a cancel.** Disconnect makes the in-flight Events
//     call fail, exactly as a dropped HTTP connection would, and touches nothing
//     else: the run stays in its phase, no signal is recorded, and the next
//     Events call with the same cursor resumes. A fake that quietly ended the run
//     would let a consumer built on the wrong assumption pass.
//   - **An interrupt request is not a termination.** An unconfirmed interrupt
//     moves a run to PhaseSignaled and leaves it live. Only domain quiet permits
//     workspace reuse.
//   - **A start is remembered.** Start on a run id already dispatched returns the
//     dispatch that exists rather than creating a second one. StartCalls counts
//     keyed requests (including required replay); RunCreations counts effects,
//     so "no duplicate dispatch" remains a number a test reads.
//
// The event payloads are a stand-in for a provider harness's native stream:
// Translate is what a real adapter's `translate(line []byte) []core.Event` would
// be, and it lives here rather than in internal/remote for the reason that
// division exists at all — the substrate must never parse a provider payload.
package remotetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Backend is the scripted in-memory backend. The zero value is not usable; call
// New.
type Backend struct {
	mu sync.Mutex

	workspaces map[string]*workspace
	runs       map[string]*run
	// starts is the fake's write-ahead acceptance fence. An entry without a
	// bound run is the same unresolved-start state the real Airlock adapter can
	// report after losing a response.
	starts   map[string]remote.ProcessRef
	bindings map[string]string

	// startCalls counts every Start attempt, landed or deduplicated. It is what
	// "without duplicate dispatch" is asserted on.
	startCalls int
	// runCreations counts actual durable run creation, separately from exact
	// keyed request replay.
	runCreations int
	// attachCalls counts resource-id attachment separately from keyed Start
	// replay, so ambiguous-start tests can prove they use the latter.
	attachCalls int
	// startFaults make Start responses fail either before or after the
	// backend creates the run, exercising ambiguous launch recovery.
	startFaults []*startFault
	// startRefusals make Start answer with a definite pre-claim refusal, and
	// refusals retains each one by address: nothing is created, the same
	// address answers the same way on every later read, and only a different
	// request under it goes out (remote.ErrProcessRefused).
	startRefusals []*remote.ProcessRefusal
	refusals      map[string]*remote.ProcessRefusal
	// acquires counts Acquire calls, so idempotence is a number rather than an
	// impression.
	acquires int
	// unreachable makes every read answer as an unreachable backend: an error
	// from Events and Status, and a Status whose Reachable is false. It is how a
	// test reaches the ambiguous-termination path without inventing a phase.
	unreachable bool
	// eventsFault fails every Events call until it is cleared, and nothing else.
	// Distinct from unreachable because that is the distinction the read path
	// rests on: an API pod that went away takes the event stream with it while
	// the run keeps executing, so Status, Wait and Stop must go on answering
	// (#275).
	eventsFault error
	// eventsFaulted counts the reads eventsFault refused, so a reconnect is a
	// number rather than an impression.
	eventsFaulted int
	// hooks records every script the backend was asked to run, in order.
	hooks []HookCall
	// hookResult decides what the next StartScript reports.
	hookResult     func(script string) (remote.HookResult, error)
	hookSpecResult func(remote.HookSpec) (remote.HookResult, error)
	hookRuns       map[string]*hookRun
	hookFaults     []*startFault

	// profileRevision is what a fresh Acquire pins.
	profileRevision string
	// nextSandbox numbers sandboxes, so two claims cannot share one by accident.
	nextSandbox int
}

type workspace struct {
	identity  remote.Identity
	suspended bool
	deleted   bool
}

type run struct {
	ref       remote.ProcessRef
	backendID string
	spec      remote.ProcessSpec
	// unavailable keeps an accepted identity while making every operation fail
	// to retrieve its permanent resource.
	unavailable bool

	// log is the whole event stream this run will ever produce, sequences from
	// 1. published is how much of it a consumer can see; a test advances it.
	log       []remote.Envelope
	published int
	complete  bool

	phase   remote.Phase
	stream  remote.StreamState
	process remote.ProcessState
	domain  remote.DomainState
	stops   []remote.StopRequest
	// stopCalls counts Stop calls.
	stopCalls int
	// confirmable says a Stop reaches domain quiet. False models a run the
	// backend cannot prove is gone — SPEC §9.8's retained claim.
	confirmable bool

	// floor, when positive, is the oldest sequence this run still retains: a
	// cursor below the one before it is answered with a measured
	// remote.RetentionGap, and everything under it is no longer served. It
	// models Airlock's `cursor_too_old` (airlocktest.Server.ExpireEvents) —
	// including its self-describing details, because an expiry BEN cannot
	// measure is a different refusal and Disconnect is the seam for that one.
	floor int64

	// disconnect, when non-nil, fails the next Events call and is then cleared.
	disconnect error
	// replayFrom, when positive, makes the next Events call serve the log from
	// that sequence regardless of the cursor asked for. It is how a reconnecting
	// backend's overlap is scripted.
	replayFrom int64

	// changed is closed and replaced whenever anything above moves, so a blocked
	// Events call wakes without polling.
	changed chan struct{}
}

type startFault struct {
	err    error
	landed bool
}

type hookRun struct {
	ref    remote.HookRef
	spec   remote.HookSpec
	status remote.HookStatus
}

// New builds a backend pinned to one profile revision.
func New(profileRevision string) *Backend {
	return &Backend{
		workspaces:      map[string]*workspace{},
		runs:            map[string]*run{},
		starts:          map[string]remote.ProcessRef{},
		bindings:        map[string]string{},
		refusals:        map[string]*remote.ProcessRefusal{},
		hookRuns:        map[string]*hookRun{},
		profileRevision: profileRevision,
	}
}

// --- WorkspaceBackend (internal/remote) ---

// Workspaces is the WorkspaceBackend view of the backend.
//
// A view rather than more methods on Backend, because the two seams both declare
// an `Attach` and they attach to different things — a workspace by claim cycle, a
// run by run id. Go's answer to that collision is two receivers, and it is the
// right answer beyond the compiler: a caller holding one seam cannot reach the
// other's lifecycle, which is the division internal/remote draws between them.
type Workspaces struct{ b *Backend }

// Workspaces returns the workspace seam. It shares all state with the backend.
func (b *Backend) Workspaces() *Workspaces { return &Workspaces{b: b} }

func (w *Workspaces) Acquire(ctx context.Context, req remote.AcquireRequest) (remote.Identity, error) {
	if err := ctx.Err(); err != nil {
		return remote.Identity{}, err
	}
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acquires++
	if !req.Claim.Valid() {
		return remote.Identity{}, fmt.Errorf("remotetest: %s is not a complete claim cycle", req.Claim)
	}
	key := req.Claim.String()
	if ws, ok := b.workspaces[key]; ok && !ws.deleted {
		// Idempotent per claim cycle: the same request returns the same
		// identity, resuming a suspended workspace rather than rebuilding it.
		if req.Branch != ws.identity.Branch || req.BaseSHA != ws.identity.BaseSHA {
			return remote.Identity{}, fmt.Errorf("%w: %s is recorded against a different branch or base",
				remote.ErrClaimMismatch, req.Claim)
		}
		if req.ProfileRevision != "" && req.ProfileRevision != ws.identity.ProfileRevision {
			return remote.Identity{}, fmt.Errorf("%w: profile revision %q was pinned but this workspace is %q",
				remote.ErrClaimMismatch, req.ProfileRevision, ws.identity.ProfileRevision)
		}
		ws.suspended = false
		return ws.identity, nil
	}
	if req.ProfileRevision != "" && req.ProfileRevision != b.profileRevision {
		// A pinned revision the backend can no longer serve is refused, never
		// substituted: substituting is the silent form of the mutable-profile
		// hazard remote.Identity.ProfileRevision exists to close.
		return remote.Identity{}, fmt.Errorf("remotetest: profile revision %q is no longer available "+
			"(current is %q)", req.ProfileRevision, b.profileRevision)
	}
	b.nextSandbox++
	id := remote.Identity{
		Claim:           req.Claim,
		Branch:          req.Branch,
		BaseSHA:         req.BaseSHA,
		SandboxID:       fmt.Sprintf("sandbox-%d", b.nextSandbox),
		ProfileRevision: b.profileRevision,
	}
	b.workspaces[key] = &workspace{identity: id}
	return id, nil
}

func (w *Workspaces) Attach(ctx context.Context, claim remote.Claim) (remote.Identity, error) {
	if err := ctx.Err(); err != nil {
		return remote.Identity{}, err
	}
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	ws, ok := b.workspaces[claim.String()]
	if !ok || ws.deleted {
		return remote.Identity{}, fmt.Errorf("%w: %s", remote.ErrNoWorkspace, claim)
	}
	return ws.identity, nil
}

func (w *Workspaces) Suspend(_ context.Context, claim remote.Claim) error {
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if ws, ok := b.workspaces[claim.String()]; ok && !ws.deleted {
		ws.suspended = true
	}
	return nil
}

func (w *Workspaces) Delete(_ context.Context, claim remote.Claim) error {
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if ws, ok := b.workspaces[claim.String()]; ok {
		ws.deleted = true
	}
	return nil
}

// Suspended reports whether a claim's workspace is suspended.
func (b *Backend) Suspended(claim remote.Claim) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ws, ok := b.workspaces[claim.String()]
	return ok && ws.suspended && !ws.deleted
}

// Deleted reports whether a claim's workspace has been deleted.
func (b *Backend) Deleted(claim remote.Claim) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ws, ok := b.workspaces[claim.String()]
	return ok && ws.deleted
}

// Acquires counts Acquire calls.
func (b *Backend) Acquires() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquires
}

// --- ProcessBackend (internal/remote) ---

func (b *Backend) Start(ctx context.Context, ref remote.ProcessRef, spec remote.ProcessSpec) (remote.Status, error) {
	if err := ctx.Err(); err != nil {
		return remote.Status{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startCalls++
	if !ref.Complete() || spec.Identity != ref.Identity {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		return remote.Status{}, err
	}
	if digest != ref.RequestDigest {
		return remote.Status{}, fmt.Errorf("%w: digest %s does not name %s", remote.ErrProcessMismatch, ref.RequestDigest, digest)
	}
	var fault *startFault
	if len(b.startFaults) > 0 {
		fault = b.startFaults[0]
		b.startFaults = b.startFaults[1:]
	}
	key := processKey(ref)
	if attempted, ok := b.starts[key]; ok && attempted != ref {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	if refused, ok := b.refusals[key]; ok {
		// The real backend answers an unchanged body from its recorded refusal
		// without a request; a changed body is a different digest and therefore
		// a different address here too.
		return remote.Status{}, refused
	}
	if len(b.startRefusals) > 0 {
		refused := b.startRefusals[0]
		b.startRefusals = b.startRefusals[1:]
		b.starts[key] = ref
		b.refusals[key] = refused
		return remote.Status{}, refused
	}
	if backendID, ok := b.bindings[key]; ok {
		r, exists := b.runs[key]
		if !exists || r.unavailable || r.backendID != backendID {
			return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
		}
		if r.ref != ref {
			return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
		}
		if fault != nil {
			return b.statusOf(r), fault.err
		}
		return b.statusOf(r), nil
	}
	if r, ok := b.runs[key]; ok {
		if r.ref != ref {
			return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
		}
		if r.unavailable {
			return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
		}
		if fault != nil {
			return b.statusOf(r), fault.err
		}
		b.bindings[key] = r.backendID
		return b.statusOf(r), nil
	}
	if fault != nil && !fault.landed {
		b.starts[key] = ref
		return remote.Status{}, fault.err
	}
	r := &run{
		ref:         ref,
		backendID:   fmt.Sprintf("backend-%s-%s", ref.Identity.SandboxID, ref.RunID),
		spec:        spec,
		phase:       remote.PhaseRunning,
		stream:      remote.StreamStateOpen,
		process:     remote.ProcessStateRunning,
		domain:      remote.DomainStateActive,
		confirmable: true,
		changed:     make(chan struct{}),
	}
	b.starts[key] = ref
	b.runs[key] = r
	b.runCreations++
	if fault != nil {
		return b.statusOf(r), fault.err
	}
	b.bindings[key] = r.backendID
	return b.statusOf(r), nil
}

func (b *Backend) Attach(ctx context.Context, ref remote.ProcessRef, backendRunID string) (remote.Status, error) {
	if err := ctx.Err(); err != nil {
		return remote.Status{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attachCalls++
	if b.unreachable {
		return remote.Status{}, fmt.Errorf("remotetest: backend unreachable")
	}
	if backendRunID == "" {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessUnresolved, ref)
	}
	key := processKey(ref)
	if attempted, ok := b.starts[key]; ok && attempted != ref {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	if bound, ok := b.bindings[key]; ok && bound != backendRunID {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	// The real adapter persists this permanent id before asking whether the
	// resource exists. Retain the same unavailable state after a failed attach.
	b.starts[key] = ref
	b.bindings[key] = backendRunID
	r, ok := b.runs[key]
	if !ok || r.backendID != backendRunID {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
	}
	if r.unavailable {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
	}
	if r.ref != ref {
		return remote.Status{}, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	return b.statusOf(r), nil
}

func (b *Backend) Status(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	if err := ctx.Err(); err != nil {
		return remote.Status{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unreachable {
		return remote.Status{}, fmt.Errorf("remotetest: backend unreachable")
	}
	r, err := b.processLocked(ref)
	if errors.Is(err, remote.ErrProcessUnresolved) {
		key := processKey(ref)
		if pending, ok := b.runs[key]; ok {
			// The fake's workspace also has one active run. Status adopts that
			// exact ref just as the Airlock backend does from active_run_id and
			// its immutable process label.
			b.bindings[key] = pending.backendID
			return b.statusOf(pending), nil
		}
		// No active run is a snapshot, not proof that the unanswered Start
		// cannot still commit. The exact Start body is deliberately outside this
		// backend-only seam, so its caller must arrange replay.
		return remote.Status{}, err
	}
	if err != nil {
		return remote.Status{}, err
	}
	return b.statusOf(r), nil
}

// processLocked resolves the three shared absence states while b.mu is held.
func (b *Backend) processLocked(ref remote.ProcessRef) (*run, error) {
	key := processKey(ref)
	if attempted, ok := b.starts[key]; ok && attempted != ref {
		return nil, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	r, ok := b.runs[key]
	if ok && r.ref != ref {
		return nil, fmt.Errorf("%w: %s", remote.ErrProcessMismatch, ref)
	}
	if backendID, bound := b.bindings[key]; bound {
		if !ok || r.unavailable || r.backendID != backendID {
			return nil, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
		}
		return r, nil
	}
	if ok && r.unavailable {
		return nil, fmt.Errorf("%w: %s", remote.ErrProcessUnavailable, ref)
	}
	if ok {
		return nil, fmt.Errorf("%w: %s", remote.ErrProcessUnresolved, ref)
	}
	if refused, ok := b.refusals[key]; ok {
		return nil, refused
	}
	if _, attempted := b.starts[key]; attempted {
		return nil, fmt.Errorf("%w: %s", remote.ErrProcessUnresolved, ref)
	}
	return nil, fmt.Errorf("%w: %s", remote.ErrNoProcess, ref)
}

// statusOf is called with the lock held.
func (b *Backend) statusOf(r *run) remote.Status {
	return remote.Status{
		Phase: r.phase, Stream: r.stream, Process: r.process, Domain: r.domain,
		BackendRunID: r.backendID, Reachable: true,
	}
}

func (b *Backend) Events(ctx context.Context, ref remote.ProcessRef, after remote.Cursor) ([]remote.Envelope, error) {
	for {
		b.mu.Lock()
		if b.unreachable {
			b.mu.Unlock()
			return nil, fmt.Errorf("remotetest: backend unreachable")
		}
		if err := b.eventsFault; err != nil {
			b.eventsFaulted++
			b.mu.Unlock()
			return nil, err
		}
		r, err := b.processLocked(ref)
		if err != nil {
			b.mu.Unlock()
			return nil, err
		}
		if err := r.disconnect; err != nil {
			// A dropped connection and nothing else: the run's phase, signals
			// and log are untouched, so the next call resumes.
			r.disconnect = nil
			b.mu.Unlock()
			return nil, err
		}
		if r.floor > 0 && int64(after) < r.floor-1 {
			// Self-describing, like the contract's own answer: the cursor the
			// statement is about and the oldest sequence still held. Those two
			// numbers are what make the loss a range BEN can record rather than
			// a hole it can only refuse.
			floor := r.floor
			b.mu.Unlock()
			return nil, &remote.RetentionGap{RequestedAfter: int64(after), OldestAvailable: floor}
		}
		from := int64(after)
		if r.replayFrom > 0 {
			from = r.replayFrom - 1
			r.replayFrom = 0
		}
		var out []remote.Envelope
		for _, env := range r.log[:r.published] {
			if env.Seq > from && env.Seq >= r.floor {
				out = append(out, env)
			}
		}
		if len(out) > 0 {
			b.mu.Unlock()
			return out, nil
		}
		if r.complete {
			b.mu.Unlock()
			return nil, nil
		}
		changed := r.changed
		b.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			// A consumer disconnect. The run is left exactly as it was.
			return nil, ctx.Err()
		}
	}
}

func (b *Backend) Stdin(_ context.Context, ref remote.ProcessRef, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.processLocked(ref)
	if err != nil {
		return err
	}
	r.spec.Stdin = append(r.spec.Stdin, data...)
	return nil
}

func (b *Backend) Stop(ctx context.Context, ref remote.ProcessRef, req remote.StopRequest) (remote.Status, error) {
	if err := ctx.Err(); err != nil {
		return remote.Status{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unreachable {
		return remote.Status{}, fmt.Errorf("remotetest: backend unreachable")
	}
	r, err := b.processLocked(ref)
	if err != nil {
		return remote.Status{}, err
	}
	r.stopCalls++
	r.stops = append(r.stops, req)
	if r.confirmable {
		r.phase = remote.PhaseQuiet
		r.process = remote.ProcessStateReaped
		r.domain = remote.DomainStateQuiet
		r.stream = remote.StreamStateSealed
		r.complete = true
	} else {
		r.phase = remote.PhaseSignaled
	}
	r.notify()
	return b.statusOf(r), nil
}

func (b *Backend) Wait(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	for {
		b.mu.Lock()
		if b.unreachable {
			b.mu.Unlock()
			return remote.Status{}, fmt.Errorf("remotetest: backend unreachable")
		}
		r, err := b.processLocked(ref)
		if err != nil {
			b.mu.Unlock()
			return remote.Status{}, err
		}
		if r.process == remote.ProcessStateReaped {
			st := b.statusOf(r)
			b.mu.Unlock()
			return st, nil
		}
		changed := r.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return remote.Status{}, ctx.Err()
		}
	}
}

// --- HookExec (internal/remote) ---

// HookCall is one script the backend was asked to run.
type HookCall struct {
	Claim   remote.Claim
	ID      remote.HookID
	Phase   remote.HookPhase
	Attempt int
	Script  string
	Argv    []string
	Git     remote.GitScope
	Timeout time.Duration
}

func (b *Backend) StartScript(ctx context.Context, ref remote.HookRef, spec remote.HookSpec) (remote.HookStatus, error) {
	if err := ctx.Err(); err != nil {
		return remote.HookStatus{}, err
	}
	b.mu.Lock()
	key := hookProcessKey(ref)
	digest, err := remote.HookRequestDigest(spec)
	if !ref.Complete() || spec.Identity != ref.Identity || spec.Phase != ref.Phase ||
		spec.Attempt != ref.Attempt || err != nil || digest != ref.RequestDigest {
		b.mu.Unlock()
		return remote.HookStatus{}, fmt.Errorf("%w: %s", remote.ErrHookMismatch, ref.Key())
	}
	var fault *startFault
	if len(b.hookFaults) > 0 {
		fault = b.hookFaults[0]
		b.hookFaults = b.hookFaults[1:]
	}
	if existing, ok := b.hookRuns[key]; ok {
		if existing.ref != ref {
			b.mu.Unlock()
			return remote.HookStatus{}, fmt.Errorf("%w: %s", remote.ErrHookMismatch, ref.Key())
		}
		status := existing.status
		if fault != nil {
			b.mu.Unlock()
			return status, fault.err
		}
		b.mu.Unlock()
		return status, nil
	}
	if fault != nil && !fault.landed {
		b.mu.Unlock()
		return remote.HookStatus{}, fault.err
	}
	b.hooks = append(b.hooks, HookCall{
		Claim: ref.Identity.Claim, ID: ref.ID, Phase: ref.Phase, Attempt: ref.Attempt,
		Script: spec.Script, Argv: append([]string(nil), spec.Argv...), Git: spec.Git, Timeout: spec.Timeout,
	})
	result := b.hookResult
	specResult := b.hookSpecResult
	var hookResult remote.HookResult
	if specResult != nil {
		hookResult, err = specResult(spec)
		if err != nil {
			b.mu.Unlock()
			return remote.HookStatus{}, err
		}
	} else if result != nil {
		hookResult, err = result(spec.Script)
		if err != nil {
			b.mu.Unlock()
			return remote.HookStatus{}, err
		}
	}
	if spec.Git.Phase == remote.GitPhasePrepare && hookResult.ExitCode == 0 && hookResult.Output == "" {
		encoded, encodeErr := json.Marshal(map[string]string{
			"repository": spec.Git.Repository,
			"branch":     spec.Git.Branch,
			"head_sha":   spec.Git.CheckoutCommit,
		})
		if encodeErr != nil {
			b.mu.Unlock()
			return remote.HookStatus{}, encodeErr
		}
		hookResult.Output = string(encoded)
	}
	status := remote.HookStatus{State: remote.HookStateFinished, Result: hookResult, Domain: remote.DomainStateQuiet}
	b.hookRuns[key] = &hookRun{ref: ref, spec: spec, status: status}
	b.mu.Unlock()
	if fault != nil {
		return status, fault.err
	}
	return status, nil
}

func (b *Backend) StatusScript(ctx context.Context, ref remote.HookRef) (remote.HookStatus, error) {
	if err := ctx.Err(); err != nil {
		return remote.HookStatus{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unreachable {
		return remote.HookStatus{}, fmt.Errorf("remotetest: backend unreachable")
	}
	r, ok := b.hookRuns[hookProcessKey(ref)]
	if !ok {
		return remote.HookStatus{}, fmt.Errorf("%w: %s", remote.ErrNoHook, ref.Key())
	}
	if r.ref != ref {
		return remote.HookStatus{}, fmt.Errorf("%w: %s", remote.ErrHookMismatch, ref.Key())
	}
	return r.status, nil
}

func (b *Backend) WaitScript(ctx context.Context, ref remote.HookRef) (remote.HookStatus, error) {
	return b.StatusScript(ctx, ref)
}

func (b *Backend) SetHookStartFault(err error, landed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hookFaults = append(b.hookFaults, &startFault{err: err, landed: landed})
}

func hookProcessKey(ref remote.HookRef) string {
	return ref.Identity.SandboxID + "\x00" + string(ref.ID)
}

// SetHookResult decides what subsequent StartScript calls report. nil restores
// success.
func (b *Backend) SetHookResult(fn func(script string) (remote.HookResult, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hookResult = fn
}

// SetHookSpecResult is SetHookResult for typed direct commands whose argv and
// Git scope, rather than a shell script, determine the fake result.
func (b *Backend) SetHookSpecResult(fn func(remote.HookSpec) (remote.HookResult, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hookSpecResult = fn
}

// Hooks returns every script the backend was asked to run, in order.
func (b *Backend) Hooks() []HookCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]HookCall(nil), b.hooks...)
}

// --- scripting ---

// Append adds payloads to a run's log without publishing them, and returns the
// sequence of the last one.
func (b *Backend) Append(id remote.RunID, payloads ...[]byte) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	for _, p := range payloads {
		r.log = append(r.log, remote.Envelope{Seq: int64(len(r.log)) + 1, Stream: remote.StreamStdout, Payload: p})
	}
	return int64(len(r.log))
}

// Publish makes the first n entries of a run's log visible to a consumer.
func (b *Backend) Publish(id remote.RunID, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	if n > len(r.log) {
		n = len(r.log)
	}
	if n > r.published {
		r.published = n
		r.notify()
	}
}

// Emit appends payloads and publishes them in one step, which is the ordinary
// case.
func (b *Backend) Emit(id remote.RunID, payloads ...[]byte) {
	b.Append(id, payloads...)
	b.mu.Lock()
	r := b.mustRun(id)
	r.published = len(r.log)
	r.notify()
	b.mu.Unlock()
}

// EmitEnvelopes publishes caller-supplied sequences verbatim. It is a fault
// injection seam for gap/conflict tests; ordinary scripts use Emit, which owns
// contiguous numbering.
func (b *Backend) EmitEnvelopes(id remote.RunID, envelopes ...remote.Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	for _, env := range envelopes {
		env.Payload = append([]byte(nil), env.Payload...)
		r.log = append(r.log, env)
	}
	r.published = len(r.log)
	r.notify()
}

// Complete says the run's stream has ended: a consumer reading past the last
// published event gets the empty answer that means "no more".
func (b *Backend) Complete(id remote.RunID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.complete = true
	r.stream = remote.StreamStateSealed
	r.notify()
}

// Quiet moves a run to domain quiet — the only phase that confirms a
// termination.
func (b *Backend) Quiet(id remote.RunID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.phase = remote.PhaseQuiet
	r.stream = remote.StreamStateSealed
	r.process = remote.ProcessStateReaped
	r.domain = remote.DomainStateQuiet
	r.complete = true
	r.notify()
}

// Reap marks only the direct process reaped. It intentionally leaves the event
// stream and descendant domain unchanged.
func (b *Backend) Reap(id remote.RunID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.process = remote.ProcessStateReaped
	r.notify()
}

// SetDomainQuiet changes only descendant-domain evidence.
func (b *Backend) SetDomainQuiet(id remote.RunID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.domain = remote.DomainStateQuiet
	r.notify()
}

// SetPhase forces a run's phase, for the cases whose subject is the mapping onto
// core.Termination.
func (b *Backend) SetPhase(id remote.RunID, p remote.Phase) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.phase = p
	if p == remote.PhaseQuiet {
		r.domain = remote.DomainStateQuiet
	}
	r.notify()
}

// SetConfirmable decides whether a Stop can reach quiet. False is the run
// whose disappearance the backend cannot prove (SPEC §9.8).
func (b *Backend) SetConfirmable(id remote.RunID, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mustRun(id).confirmable = on
}

// Disconnect fails the next Events call with err, modelling a dropped
// connection. Nothing else about the run changes — which is the fake's whole
// statement about what a disconnect is.
func (b *Backend) Disconnect(id remote.RunID, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.disconnect = err
	r.notify()
}

// SetEventsFault fails every Events call with err until it is cleared with nil,
// modelling a read path that stays down — a restarted API pod, a partitioned
// load balancer — for longer than one call.
//
// Only the read path. The run goes on executing, its log goes on accumulating,
// and Status, Wait and Stop go on answering, because that is precisely the
// situation the fake exists to let a test drive: an outage on BEN's side of the
// connection is not evidence about the process (SPEC §9.8). Disconnect is the
// one-shot form; SetUnreachable is the different fault where nothing answers.
func (b *Backend) SetEventsFault(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.eventsFault = err
	for _, r := range b.runs {
		r.notify()
	}
}

// EventsFaults counts the Events calls SetEventsFault refused, which is how many
// times a reader came back while the read path was down.
func (b *Backend) EventsFaults() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.eventsFaulted
}

// ExpireEvents raises a run's retention floor: sequences below floor are gone,
// and a cursor that has not reached floor-1 is answered with the measured
// remote.RetentionGap. A floor of one past the last sequence is a fully expired
// log.
func (b *Backend) ExpireEvents(id remote.RunID, floor int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.floor = floor
	r.notify()
}

// ReplayFrom makes the next Events call serve the log from seq regardless of the
// cursor asked for — the overlap a reconnecting backend serves, and the input
// the dedupe rule exists for.
func (b *Backend) ReplayFrom(id remote.RunID, seq int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.replayFrom = seq
}

// Rewrite replaces the payload already served under a sequence, which is the
// conflicting-duplicate a dedupe rule must not absorb.
func (b *Backend) Rewrite(id remote.RunID, seq int64, payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	env := r.log[seq-1]
	env.Payload = payload
	r.log[seq-1] = env
}

// SetStartFault makes the next Start fail. landed says whether the run was
// created before the response was lost.
func (b *Backend) SetStartFault(err error, landed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startFaults = append(b.startFaults, &startFault{err: err, landed: landed})
}

// SetStartRefusal makes the next Start answer with a definite pre-claim
// refusal: nothing is created, and the address stays refused on every later
// read (remote.ProcessRefusal).
func (b *Backend) SetStartRefusal(code, message string, limitBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startRefusals = append(b.startRefusals, &remote.ProcessRefusal{
		Code: code, Message: message, LimitBytes: limitBytes,
		Cause: fmt.Errorf("remotetest: the backend refused the start body"),
	})
}

// SetUnreachable makes every read answer as a backend that cannot be reached:
// the ambiguous case, which must map to an unconfirmed termination.
func (b *Backend) SetUnreachable(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unreachable = on
	for _, r := range b.runs {
		r.notify()
	}
}

// SetProcessUnavailable keeps an accepted process's identity while making its
// permanent backend resource inaccessible. It models a known-run 404: unlike a
// never-accepted reference, it is not evidence that the domain is quiet.
func (b *Backend) SetProcessUnavailable(id remote.RunID, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.mustRun(id)
	r.unavailable = on
	r.notify()
}

// StartCalls counts every dispatch attempt the backend received.
func (b *Backend) StartCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startCalls
}

// RunCreations counts durable process effects, excluding idempotent Start
// replays that return an existing run.
func (b *Backend) RunCreations() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runCreations
}

// AttachCalls counts attachment by a known backend resource id.
func (b *Backend) AttachCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attachCalls
}

// StopCalls counts Stop calls for a run.
func (b *Backend) StopCalls(id remote.RunID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.findRun(id)
	if r == nil {
		return 0
	}
	return r.stopCalls
}

// StopRequests reports the mode and grace delivered to the backend.
func (b *Backend) StopRequests(id remote.RunID) []remote.StopRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.findRun(id)
	if r == nil {
		return nil
	}
	return append([]remote.StopRequest(nil), r.stops...)
}

// Spec returns the ProcessSpec a run was dispatched with.
func (b *Backend) Spec(id remote.RunID) (remote.ProcessSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.findRun(id)
	if r == nil {
		return remote.ProcessSpec{}, false
	}
	return r.spec, true
}

// Live reports whether the backend still holds a run that is not quiet — the
// fact "a disconnect is not a cancel" is asserted on.
func (b *Backend) Live(id remote.RunID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.findRun(id)
	return r != nil && r.domain != remote.DomainStateQuiet
}

func (b *Backend) mustRun(id remote.RunID) *run {
	r := b.findRun(id)
	if r == nil {
		panic("remotetest: no run " + string(id) + " — script a Start first")
	}
	return r
}

func (b *Backend) findRun(id remote.RunID) *run {
	var found *run
	for _, r := range b.runs {
		if r.ref.RunID != id {
			continue
		}
		if found != nil {
			panic("remotetest: run id " + string(id) + " is ambiguous across sandboxes")
		}
		found = r
	}
	return found
}

func processKey(ref remote.ProcessRef) string {
	return ref.Identity.SandboxID + "\x00" + string(ref.RunID)
}

// notify is called with the backend lock held.
func (r *run) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// --- the stand-in provider adapter ---

// line is the fake harness's native stream shape. It stands in for
// claude-code's and codex-exec's, and its only job is to be something only
// Translate understands — so a substrate that started parsing payloads would
// fail rather than coincidentally work.
type line struct {
	Kind    string  `json:"kind"`
	Session string  `json:"session,omitempty"`
	Text    string  `json:"text,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Input   int64   `json:"input,omitempty"`
	Output  int64   `json:"output,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
}

// Translate is the provider adapter's function, in the signature both v1
// adapters already have: one raw payload in, normalized events out. A payload it
// does not understand yields no events — activity, not an error (SPEC §7.2).
func Translate(payload []byte) []core.Event {
	var l line
	if err := json.Unmarshal(payload, &l); err != nil {
		return nil
	}
	switch l.Kind {
	case "init":
		return []core.Event{{Type: core.EventStarted, SessionID: l.Session, Continuation: l.Session}}
	case "text":
		return []core.Event{{Type: core.EventProgress, Text: l.Text}}
	case "usage":
		return []core.Event{{Type: core.EventUsage, Usage: &core.Usage{
			InputTokens: l.Input, OutputTokens: l.Output, CostUSD: l.Cost,
		}}}
	case "success":
		return []core.Event{{Type: core.EventSucceeded}}
	case "failure":
		return []core.Event{{Type: core.EventFailed, Reason: core.FailureReason(l.Reason)}}
	default:
		return nil
	}
}

// Init, Text, Usage, Success and Failure build the payloads Translate reads.
func Init(session string) []byte { return mustLine(line{Kind: "init", Session: session}) }
func Text(text string) []byte    { return mustLine(line{Kind: "text", Text: text}) }
func Usage(in, out int64, cost float64) []byte {
	return mustLine(line{Kind: "usage", Input: in, Output: out, Cost: cost})
}
func Success() []byte { return mustLine(line{Kind: "success"}) }
func Failure(reason core.FailureReason) []byte {
	return mustLine(line{Kind: "failure", Reason: string(reason)})
}

// Private is a payload with no normalized meaning: the private line every real
// harness has, which the transcript keeps and the event stream drops.
func Private() []byte { return mustLine(line{Kind: "private"}) }

func mustLine(l line) []byte {
	body, err := json.Marshal(l)
	if err != nil {
		panic("remotetest: encoding a stream line: " + err.Error())
	}
	return append(body, '\n')
}
