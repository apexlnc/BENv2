package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Workspaces is an in-memory workspace provider. It creates nothing on disk;
// what matters to the orchestrator is which workspace it got, whether Prepare
// failed, and whether Dispose was told to keep it.
type Workspaces struct {
	mu sync.Mutex

	// FailPrepare, when set, is returned by the next Prepare call for the
	// matching issue. A nil func means every Prepare succeeds.
	FailPrepare func(identifier string, attempt int) error

	// Prepared records every Prepare, in order.
	Prepared []PrepareCall
	// Disposed records every Dispose, in order.
	Disposed []DisposeCall

	gate             func()
	failDispose      error
	failDisposeOnce  bool
	prepareErrWithWS error
	prepareFacts     func(ws core.Workspace) (core.LocalBranchFacts, error)
	facts            func(ws core.Workspace) (core.PublishFacts, error)
	attemptFacts     func(ws core.Workspace) (core.AttemptFacts, error)
	claimBases       map[string]core.ClaimBase
	legacyBases      map[string]string
	heads            map[string]string
	priorWork        map[string]bool

	failClaimBase      error
	failClaimBaseState core.ClaimBase

	failBeginClaim  error
	claimBaseGate   func()
	ClaimBaseBegins []BeginClaimBaseCall
	// defaultBase is the head a fresh workspace pins to. See SetDefaultBase.
	defaultBase string
	// defaultTarget is the target selector a fresh assignment records atomically
	// with defaultBase. Existing pins retain their value when it moves.
	defaultTarget string

	// markers is the §9.10 run marker store, keyed by workspace key.
	markers map[string]core.RunMarker
	// MarkerWrites and MarkerClears record the marker lifecycle in order, so a
	// test can assert the one ordering that matters: the marker is written before
	// the launch and removed only on confirmed-gone.
	MarkerWrites []string
	MarkerClears []string
	// markerClearAttempts counts every removal call, failed ones included. See
	// MarkerClearAttempts.
	markerClearAttempts int
	clearGate           func()
	failMarker          error
	failMarkerClear     error
	failMarkerRead      error
	failResolve         error
	failList            error
	// dirs are the workspaces that exist, and owners maps each to the issue it was
	// prepared for. Two maps rather than one because they have different lifetimes:
	// Dispose removes both, but an owner record can be dropped on its own (see
	// ForgetOwner), which is a state the sweep must handle.
	dirs   map[string]bool
	owners map[string]string
}

// DefaultBaseSHA is the head every fresh workspace pins to — this fake's
// stand-in for origin's default branch.
//
// One value for every issue, deliberately. With no local or remote issue
// branch, the real provider pins a new claim epoch to the fetched default head,
// so two issues prepared from an unchanged
// default branch pin to the same commit. A BaseSHA is a commit, not an issue
// identity, and a fake that handed out a distinct one per issue would let code
// that confused the two pass here and fail in production.
const DefaultBaseSHA = "1111111111111111111111111111111111111111"

// DefaultTargetBranch is the target a fresh fake provider selects.
const DefaultTargetBranch = "main"

type PrepareCall struct {
	Identifier string
	Attempt    int
	Epoch      int64
	// BaseSHA is the claim-time base this prepare reported, so a test spanning
	// attempts can assert it did not move (SPEC §6.2).
	BaseSHA string
}

type BeginClaimBaseCall struct {
	Identifier string
	Epoch      int64
}

type DisposeCall struct {
	Key  string
	Keep bool
}

// pathsFor is this fake's layout (SPEC §6.1, §6.2). Nothing here exists on
// disk — Path never did either — but the *shape* is the real provider's,
// because two properties of it are contractual rather than incidental and code
// under test may rely on them: the private dir is outside the worktree
// (SPEC §6.3), and the shared git dir is per-workflow rather than per-issue, so
// two workspaces report the same one. A fake that made all three per-issue
// siblings would let code that assumed a private shared git dir pass here.
func pathsFor(key string) core.WorkspacePaths {
	return core.WorkspacePaths{
		Path:         "/fake/workspaces/" + key,
		SharedGitDir: "/fake/base.git",
		PrivateDir:   "/fake/private/" + key,
	}
}

func NewWorkspaces() *Workspaces {
	return &Workspaces{
		claimBases:    map[string]core.ClaimBase{},
		legacyBases:   map[string]string{},
		heads:         map[string]string{},
		priorWork:     map[string]bool{},
		defaultBase:   DefaultBaseSHA,
		defaultTarget: DefaultTargetBranch,
		markers:       map[string]core.RunMarker{},
		dirs:          map[string]bool{},
		owners:        map[string]string{},
	}
}

// BeginClaimBase mirrors the concrete provider's durable pending write. The
// call is recorded before an injected failure, because the real provider has
// attempted the write by the time it reports one.
func (w *Workspaces) BeginClaimBase(_ context.Context, issue core.Issue, epoch int64) error {
	w.mu.Lock()
	gate := w.claimBaseGate
	w.mu.Unlock()
	if gate != nil {
		gate()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ClaimBaseBegins = append(w.ClaimBaseBegins, BeginClaimBaseCall{Identifier: issue.Identifier, Epoch: epoch})
	if w.failBeginClaim != nil {
		return w.failBeginClaim
	}
	if issue.Identifier == "" || epoch <= 0 {
		return fmt.Errorf("fake: claim base requires an issue and positive epoch")
	}
	key := "issue-" + issue.Identifier
	state, ok := w.claimBases[key]
	if !ok {
		state = core.ClaimBase{State: core.ClaimBaseAbsent}
	}
	switch state.State {
	case core.ClaimBaseAbsent:
		w.claimBases[key] = core.ClaimBase{
			State: core.ClaimBasePending, Epoch: epoch,
			OutgoingBaseSHA: w.legacyBases[key],
		}
		return nil
	case core.ClaimBasePending:
		if state.Epoch != epoch {
			return fmt.Errorf("fake: claim base pending for epoch %d, not %d", state.Epoch, epoch)
		}
		return nil
	case core.ClaimBasePinned:
		if state.Epoch == epoch {
			return nil
		}
		w.claimBases[key] = core.ClaimBase{
			State: core.ClaimBasePending, Epoch: epoch,
			OutgoingEpoch: state.Epoch, OutgoingBaseSHA: state.BaseSHA,
			OutgoingTargetBranch: state.TargetBranch,
		}
		return nil
	default:
		return fmt.Errorf("fake: non-authorizing claim-base state %s", state.State)
	}
}

func (w *Workspaces) ClaimBase(_ context.Context, issue core.Issue) (core.ClaimBase, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failClaimBase != nil {
		return w.failClaimBaseState, w.failClaimBase
	}
	state, ok := w.claimBases["issue-"+issue.Identifier]
	if !ok {
		return core.ClaimBase{State: core.ClaimBaseAbsent}, nil
	}
	return state, nil
}

// AbandonPendingClaimBase mirrors the concrete provider's quiet-workspace
// rollback. It retains a real outgoing epoch, while a fresh or legacy pending
// transition becomes absent so the next assignment may begin cleanly.
func (w *Workspaces) AbandonPendingClaimBase(_ context.Context, issue core.Issue) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := "issue-" + issue.Identifier
	state, ok := w.claimBases[key]
	if !ok || state.State != core.ClaimBasePending {
		return nil
	}
	if state.OutgoingEpoch > 0 {
		w.claimBases[key] = core.ClaimBase{
			State: core.ClaimBasePinned, Epoch: state.OutgoingEpoch, BaseSHA: state.OutgoingBaseSHA,
			TargetBranch: state.OutgoingTargetBranch,
		}
		return nil
	}
	delete(w.claimBases, key)
	return nil
}

// Prepare implements the closed core strategy seam: only an already pinned
// claim is enough when no tracker epoch is supplied by the caller.
func (w *Workspaces) Prepare(ctx context.Context, issue core.Issue, attempt int) (core.Workspace, error) {
	state, err := w.ClaimBase(ctx, issue)
	if err != nil {
		return core.Workspace{}, err
	}
	if state.State != core.ClaimBasePinned || state.Epoch <= 0 {
		return core.Workspace{}, fmt.Errorf("fake: no established positive claim base for %s", issue.Identifier)
	}
	ws, _, err := w.PrepareClaim(ctx, issue, attempt, state.Epoch)
	return ws, err
}

// PrepareClaim mirrors the concrete pending→pinned boundary. Prior-work facts
// are required only when an outgoing pin exists; without one, inventing an
// observation would give a genuinely fresh branch an evidence-derived floor.
func (w *Workspaces) PrepareClaim(_ context.Context, issue core.Issue, attempt int, epoch int64) (core.Workspace, core.LocalBranchFacts, error) {
	w.mu.Lock()
	gate := w.gate
	w.mu.Unlock()
	if gate != nil {
		gate()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	key := "issue-" + issue.Identifier

	// The real Prepare has a pending→pinned line through it, and the two failure
	// seams here are the two sides of it. Everything before that transition —
	// containment, ensuring and fetching the base repo, reading the default
	// branch, fetching the remote issue branch — returns a workspace with no
	// BaseSHA and mints nothing; everything after it returns one whose base is
	// already resolved (workspace Prepare, SPEC §6.2).
	//
	// FailPrepare is the pre-pin side. Like the concrete provider, it still
	// returns the workspace identity it computed before touching Git, but no
	// epoch/base pair and no claim pin. Returning zero here used to hide a retry
	// bug: the loop behaved differently only when the real provider retained the
	// path after a pre-pin failure.
	if w.FailPrepare != nil {
		if err := w.FailPrepare(issue.Identifier, attempt); err != nil {
			w.Prepared = append(w.Prepared, PrepareCall{Identifier: issue.Identifier, Attempt: attempt, Epoch: epoch})
			return core.Workspace{
				WorkspacePaths: pathsFor(key), Key: key, Branch: "ben/" + key,
			}, core.LocalBranchFacts{}, err
		}
	}

	state, ok := w.claimBases[key]
	if !ok || state.Epoch != epoch ||
		(state.State != core.ClaimBasePending && state.State != core.ClaimBasePinned) {
		return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("fake: claim base for %s is %+v, expected epoch %d", issue.Identifier, state, epoch)
	}
	var prior core.LocalBranchFacts
	if state.State == core.ClaimBasePending {
		head := w.heads[key]
		if head == "" {
			head = w.defaultBase
		}
		if state.OutgoingBaseSHA != "" {
			if w.prepareFacts == nil {
				w.Prepared = append(w.Prepared, PrepareCall{Identifier: issue.Identifier, Attempt: attempt, Epoch: epoch})
				return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("fake: no pre-repin local evidence scripted for workspace %s", key)
			}
			probe := core.Workspace{
				WorkspacePaths: pathsFor(key), Key: key, Branch: "ben/" + key,
				ClaimEpoch: state.OutgoingEpoch, BaseSHA: state.OutgoingBaseSHA,
				TargetBranch: state.OutgoingTargetBranch,
			}
			var err error
			prior, err = w.prepareFacts(probe)
			if err != nil {
				w.Prepared = append(w.Prepared, PrepareCall{Identifier: issue.Identifier, Attempt: attempt, Epoch: epoch})
				return core.Workspace{}, core.LocalBranchFacts{}, err
			}
			if prior.BaseSHA == "" {
				prior.BaseSHA = state.OutgoingBaseSHA
			}
			if prior.BaseSHA != state.OutgoingBaseSHA || prior.Head == "" {
				w.Prepared = append(w.Prepared, PrepareCall{Identifier: issue.Identifier, Attempt: attempt, Epoch: epoch})
				return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("fake: pre-repin evidence %+v does not describe outgoing base %s", prior, state.OutgoingBaseSHA)
			}
			head = prior.Head
		}
		state = core.ClaimBase{
			State: core.ClaimBasePinned, Epoch: epoch, BaseSHA: head,
			TargetBranch: w.defaultTarget,
		}
		w.claimBases[key] = state
		w.heads[key] = head
	}
	base := state.BaseSHA
	w.dirs[key] = true
	w.owners[key] = issue.Identifier
	w.Prepared = append(w.Prepared, PrepareCall{Identifier: issue.Identifier, Attempt: attempt, Epoch: epoch, BaseSHA: base})
	ws := core.Workspace{
		WorkspacePaths: pathsFor(key),
		Key:            key,
		Branch:         "ben/" + key,
		ClaimEpoch:     epoch,
		BaseSHA:        base,
		TargetBranch:   state.TargetBranch,
		PriorWork:      w.priorWork[issue.Identifier],
	}
	if w.prepareErrWithWS != nil {
		// The post-pin side: the worktree exists and is kept for forensics
		// (SPEC §6.6) — an after_create abort, a hook failure, a worktree the
		// provider refuses to touch. All of them are past the pin, so the
		// workspace carries its base, and the pin stands for the next attempt.
		return ws, prior, w.prepareErrWithWS
	}
	ws.CreatedNow = attempt == 1
	return ws, prior, nil
}

// PrepareWithLocalFacts is the compatibility seam for a base that is already
// pinned. Production orchestration uses PrepareClaim so pending initialization
// cannot lose the expected tracker epoch.
func (w *Workspaces) PrepareWithLocalFacts(ctx context.Context, issue core.Issue, attempt int) (core.Workspace, core.LocalBranchFacts, error) {
	state, err := w.ClaimBase(ctx, issue)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}
	if state.State != core.ClaimBasePinned || state.Epoch <= 0 {
		return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("fake: no established positive claim base for %s", issue.Identifier)
	}
	return w.PrepareClaim(ctx, issue, attempt, state.Epoch)
}

// SetDefaultBase moves the head a fresh workspace pins to — this fake's
// stand-in for origin's default branch advancing between prepares.
//
// Pins already minted keep their value, which is the durability §6.2 asks of
// them: one claim epoch retains the head its first prepare observed, not the
// newest commit on the default branch. It is also how a fixture that
// needs two issues on different bases gets them, without the fake pretending
// that bases are unique per issue.
func (w *Workspaces) SetDefaultBase(sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultBase = sha
}

// SetDefaultTarget changes the selector used only when a later pending claim
// becomes pinned. It does not rewrite existing claim authority.
func (w *Workspaces) SetDefaultTarget(branch string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultTarget = branch
}

// SetLegacyBasePin installs a pre-epoch pin. BeginClaimBase may use it only as
// the outgoing comparison fact of a newly created epoch.
func (w *Workspaces) SetLegacyBasePin(identifier, sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.legacyBases["issue-"+identifier] = sha
}

// SetBranchHead controls the head the next pending→pinned prepare observes when
// no outgoing comparison is required.
func (w *Workspaces) SetBranchHead(identifier, sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heads["issue-"+identifier] = sha
}

// SetClaimBase installs provider-owned state for recovery fixtures. Unlike a
// convenient default, this makes every test that wants authorizing evidence say
// which epoch and base it is claiming exists.
func (w *Workspaces) SetClaimBase(identifier string, state core.ClaimBase) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.claimBases["issue-"+identifier] = state
	if state.State == core.ClaimBasePinned {
		w.heads["issue-"+identifier] = state.BaseSHA
	}
}

func (w *Workspaces) SetFailBeginClaimBase(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failBeginClaim = err
}

func (w *Workspaces) SetFailClaimBase(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failClaimBase = err
	w.failClaimBaseState = core.ClaimBase{}
}

// SetClaimBaseError installs the state-plus-error shape a validated legacy
// provider record returns. Ordinary read failures use SetFailClaimBase and
// carry no state.
func (w *Workspaces) SetClaimBaseError(state core.ClaimBase, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failClaimBaseState = state
	w.failClaimBase = err
}

func (w *Workspaces) SetClaimBaseGate(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.claimBaseGate = fn
}

func (w *Workspaces) ClaimBaseBeginCount(identifier string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, call := range w.ClaimBaseBegins {
		if call.Identifier == identifier {
			n++
		}
	}
	return n
}

func (w *Workspaces) Dispose(_ context.Context, ws core.Workspace, keep bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Disposed = append(w.Disposed, DisposeCall{ws.Key, keep})
	if w.failDispose != nil {
		err := w.failDispose
		if w.failDisposeOnce {
			w.failDispose = nil
		}
		return err
	}
	if !keep {
		// The directory and its owner record go together, as they do in the real
		// provider. A kept workspace keeps both: §6.4 retains it for a human, and a
		// later sweep has to be able to ask whose it is.
		delete(w.dirs, ws.Key)
		delete(w.owners, ws.Key)
	}
	return nil
}

// SetFailDispose makes Dispose return err. once=true clears it after the first
// call, which is the shape a retry test wants: the effect fails, is retried from
// the owed queue, and lands.
//
// Recorded before the failure is applied, deliberately — the real provider has
// tried by the time it reports one, so a fixture that skipped the record would
// let a caller that never retried look identical to one that did.
func (w *Workspaces) SetFailDispose(err error, once bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failDispose, w.failDisposeOnce = err, once
}

// PublishFacts is the §9.7 git evidence a run left behind — the seam
// internal/verify declares for itself rather than taking from core's
// three-method provider interface, which the worktree provider satisfies
// structurally and this one does too.
//
// There is no on-disk repository here, so the facts are scripted: SetFacts is
// how a test says what the agent did with the workspace it was given.
func (w *Workspaces) PublishFacts(_ context.Context, ws core.Workspace) (core.PublishFacts, error) {
	w.mu.Lock()
	facts := w.facts
	state, ok := w.claimBases[ws.Key]
	w.mu.Unlock()
	if !ok || state.State != core.ClaimBasePinned || state.Epoch != ws.ClaimEpoch ||
		state.BaseSHA != ws.BaseSHA || state.TargetBranch != ws.TargetBranch {
		return core.PublishFacts{}, fmt.Errorf("fake: workspace epoch/base/target %d/%s/%s does not match provider state %+v",
			ws.ClaimEpoch, ws.BaseSHA, ws.TargetBranch, state)
	}
	if facts == nil {
		// Fail closed rather than answer the zero value. core.PublishFacts{} is
		// "the branch does not exist", which is a verdict of its own — a test
		// that forgot to script evidence would get a contradiction it never
		// asked for, and would pass for the wrong reason.
		return core.PublishFacts{}, fmt.Errorf("fake: no publish evidence scripted for workspace %s", ws.Key)
	}
	return facts(ws)
}

// SetFacts installs the evidence PublishFacts reports. It takes the workspace
// so a fixture can answer from the claim-time BaseSHA, which is what "the run
// added no commits" is measured against.
func (w *Workspaces) SetFacts(fn func(ws core.Workspace) (core.PublishFacts, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.facts = fn
}

// AttemptFacts is the git account of what an attempt left on its branch, which
// the loop composes into the next attempt's prompt (SPEC §9.6, #61).
//
// Unscripted answers the zero value, and the standard is deliberately *not*
// PublishFacts's. There, the zero value is "the branch does not exist", which is
// a routing verdict — a test that forgot to script evidence would get a
// contradiction it never asked for. Here the zero value is the honest account of
// an attempt that committed nothing, no decision reads it, and the common case in
// the loop's own tests is exactly that: no agent, no branch, nothing committed.
// Refusing would make every retry test script an account it does not care about,
// and a fake that demands more than the real provider does is its own kind of
// infidelity.
//
// The one distinction that *is* load-bearing is preserved: an error is an error.
// SetAttemptFacts can return one, and the loop must report the branch as unread
// rather than as empty (see attemptAccount).
func (w *Workspaces) AttemptFacts(_ context.Context, ws core.Workspace) (core.AttemptFacts, error) {
	w.mu.Lock()
	facts := w.attemptFacts
	w.mu.Unlock()
	if facts == nil {
		return core.AttemptFacts{}, nil
	}
	return facts(ws)
}

// SetAttemptFacts installs the account AttemptFacts reports.
func (w *Workspaces) SetAttemptFacts(fn func(ws core.Workspace) (core.AttemptFacts, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attemptFacts = fn
}

// SetPrepareFacts installs the local branch evidence captured atomically by
// PrepareWithLocalFacts. It is independent from SetFacts because a hook or run
// may legitimately change the branch between the two observations.
func (w *Workspaces) SetPrepareFacts(fn func(ws core.Workspace) (core.LocalBranchFacts, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepareFacts = fn
}

// SetPriorWork scripts the trusted provider-owned prior-work fact returned by
// PrepareClaim. It is separate from SetPrepareFacts because a remote provider
// has no local branch observation and folds the prior publication into its new
// claim-time base.
func (w *Workspaces) SetPriorWork(identifier string, prior bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.priorWork[identifier] = prior
}

// PrepareCount reports how many attempts were prepared for an issue.
func (w *Workspaces) PrepareCount(identifier string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.Prepared {
		if c.Identifier == identifier {
			n++
		}
	}
	return n
}

// Prepares returns the prepare calls recorded for an issue, in order. Taken
// under the lock: the orchestrator prepares from a worker goroutine, so a test
// reading the slice directly would be racing it.
func (w *Workspaces) Prepares(identifier string) []PrepareCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []PrepareCall
	for _, c := range w.Prepared {
		if c.Identifier == identifier {
			out = append(out, c)
		}
	}
	return out
}

// Disposals returns the dispose calls recorded for an issue.
func (w *Workspaces) Disposals(identifier string) []DisposeCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []DisposeCall
	for _, c := range w.Disposed {
		if c.Key == "issue-"+identifier {
			out = append(out, c)
		}
	}
	return out
}

// SetGate installs a function called at the top of every Prepare, so a test
// can hold one open while the orchestrator decides to exit.
func (w *Workspaces) SetGate(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gate = fn
}

// HookedWorkspaces is a provider that also has the optional §6.5 after-run
// hook, mirroring the concrete worktree provider (B05 keeps AfterRun off the
// three-method core interface).
type HookedWorkspaces struct {
	*Workspaces
	mu       sync.Mutex
	afterRun map[string]int
}

func NewHookedWorkspaces(w *Workspaces) *HookedWorkspaces {
	return &HookedWorkspaces{Workspaces: w, afterRun: map[string]int{}}
}

func (h *HookedWorkspaces) AfterRun(_ context.Context, ws core.Workspace) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterRun[ws.Key]++
}

// AfterRunCount reports how many times the hook fired for an issue.
func (h *HookedWorkspaces) AfterRunCount(identifier string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.afterRun["issue-"+identifier]
}

// SetPrepareErrorWithWorkspace makes Prepare fail but still hand back the
// workspace it created — what the worktree provider does when it keeps one
// for forensics (SPEC §6.6).
func (w *Workspaces) SetPrepareErrorWithWorkspace(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepareErrWithWS = err
}

// The run marker store, and its fidelity rules read off the real provider
// (internal/workspace/marker.go).
//
// Two of them matter more than the storage. BeginRunMarkerFor records a marker
// with **no evidence** — the upgrade is the runner's, through
// core.RunEvidenceSink, and a fake that filled evidence in here would model a
// daemon in which the fork→record window does not exist, letting code that
// depends on its absence pass. And ClearRunMarkerFor is not an error on an absent
// marker: recovery and the run-time path can reach the same conclusion about the
// same workspace.

// BeginRunMarkerFor records that a run may be live in this issue's workspace.
func (w *Workspaces) BeginRunMarkerFor(issue core.Issue) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failMarker != nil {
		return w.failMarker
	}
	key := "issue-" + issue.Identifier
	w.MarkerWrites = append(w.MarkerWrites, key)
	// An unconditional replace, discarding any evidence already there — because
	// that is what the real BeginRun is: writeMarker(key, markerFile{}), a
	// whole-file write with no evidence in it (workspace marker.go).
	//
	// The temptation is to preserve existing evidence "so a stray second call
	// cannot lose it". That would be the fake inventing a guarantee, and it hides
	// exactly one bug: a marker written *after* the launch overwrites the upgrade
	// the launch just recorded, leaving unknown_launch and parking an issue that
	// should have resumed. With this faithful, that ordering error is visible.
	w.markers[key] = core.RunMarker{State: core.RunMarkerUnknownLaunch}
	return nil
}

// ReadRunMarkerFor reports what the previous tenure left behind.
func (w *Workspaces) ReadRunMarkerFor(issue core.Issue) (core.RunMarker, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failMarkerRead != nil {
		return core.RunMarker{}, w.failMarkerRead
	}
	m, ok := w.markers["issue-"+issue.Identifier]
	if !ok {
		return core.RunMarker{State: core.RunMarkerAbsent}, nil
	}
	return m, nil
}

// ClearRunMarkerFor frees the workspace.
func (w *Workspaces) ClearRunMarkerFor(issue core.Issue) error {
	w.mu.Lock()
	gate := w.clearGate
	w.mu.Unlock()
	if gate != nil {
		// Outside the lock, as Prepare's gate is. The real removal takes no lock this
		// provider's other operations need, so holding one here would make a wedged
		// clear block a launch's own marker write — and a test asserting that the
		// *orchestrator* orders those two would pass on the fixture instead.
		gate()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// Two seams, because the real provider has two operations: BeginRun writes a
	// file and ClearRun removes one, and they fail independently. A single knob
	// would make "the clear failed" untestable — arming it before a launch stops the
	// launch instead, which is a different path with a different outcome.
	// Counted before the failure check, and separately from MarkerClears, because the
	// real provider has *attempted* the unlink by the time it reports one — so a caller
	// that retried in a tight loop and one that retried once per tick are
	// indistinguishable on a counter that only records successes.
	w.markerClearAttempts++
	if err := cmpOr(w.failMarkerClear, w.failMarker); err != nil {
		return err
	}
	key := "issue-" + issue.Identifier
	w.MarkerClears = append(w.MarkerClears, key)
	delete(w.markers, key)
	return nil
}

// RecordRunEvidence is the upgrade the runner's evidence sink performs once the
// run exists (core.RunEvidenceSink). Separate from BeginRunMarkerFor for the
// reason above: the two are distinct writes with a crash window between them,
// and §9.10 reads the difference.
func (w *Workspaces) RecordRunEvidence(issue core.Issue, evidence core.RunEvidence) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.markers["issue-"+issue.Identifier] = core.RunMarker{
		State: core.RunMarkerIdentified, Evidence: evidence,
	}
}

// SetRunMarker installs a marker directly, for a fixture describing what a
// previous process left behind before this one ever ran.
func (w *Workspaces) SetRunMarker(identifier string, m core.RunMarker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.markers["issue-"+identifier] = m
}

// RunMarkerFor reports the stored marker, for assertions.
func (w *Workspaces) RunMarkerFor(identifier string) (core.RunMarker, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, ok := w.markers["issue-"+identifier]
	return m, ok
}

// SetFailMarker makes the marker writes fail until cleared; SetFailMarkerRead
// does the same for the read, which §9.10 must treat as "we could not look"
// rather than as a free workspace.
func (w *Workspaces) SetFailMarker(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failMarker = err
}

// SetFailMarkerClear makes only the *removal* fail, leaving launches able to write
// their own marker — the state a pending clear has to survive (SPEC §9.10).
func (w *Workspaces) SetFailMarkerClear(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failMarkerClear = err
}

func cmpOr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// SetMarkerClearGate installs a function called at the top of every
// ClearRunMarkerFor, so a test can hold a removal open.
//
// The real removal is a file unlink plus a directory fsync (workspace ClearRun), so
// it is I/O that can take arbitrarily long on a wedged filesystem — which is what
// makes "where does it run" a property worth pinning, and what this gate exists to
// stand in for. It also pins the ordering against a launch's own marker write, the
// one interleaving that costs a live agent its marker.
func (w *Workspaces) SetMarkerClearGate(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clearGate = fn
}

// MarkerWriteCount reports how many marker writes have been recorded. Taken under
// the lock: the orchestrator writes markers from a worker goroutine, so a test
// reading the slice directly would be racing it.
func (w *Workspaces) MarkerWriteCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.MarkerWrites)
}

// MarkerClearCount reports how many removals have been recorded, for the same
// reason.
func (w *Workspaces) MarkerClearCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.MarkerClears)
}

// MarkerClearAttempts reports how many removals were *attempted*, failures included.
// It is what tells a caller retrying once per tick from one retrying on the instant
// its removal failed.
func (w *Workspaces) MarkerClearAttempts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.markerClearAttempts
}

func (w *Workspaces) SetFailMarkerRead(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failMarkerRead = err
}

// ResolveWorkspace names the workspace an issue's work lives in and reports its
// pinned claim epoch/base/target tuple, preparing nothing (SPEC §9.10).
//
// `false` means no authorizing pinned pair stands. ClaimBase separately exposes
// absent versus pending so recovery cannot turn that absence into evidence.
func (w *Workspaces) ResolveWorkspace(_ context.Context, issue core.Issue) (core.Workspace, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failResolve != nil {
		return core.Workspace{}, false, w.failResolve
	}
	key := "issue-" + issue.Identifier
	state, ok := w.claimBases[key]
	if !ok || state.State != core.ClaimBasePinned {
		return core.Workspace{}, false, nil
	}
	return core.Workspace{
		WorkspacePaths: pathsFor(key),
		Key:            key,
		Branch:         "ben/" + key,
		ClaimEpoch:     state.Epoch,
		BaseSHA:        state.BaseSHA,
		TargetBranch:   state.TargetBranch,
	}, true, nil
}

// ListWorkspaces reports every workspace this provider has prepared and the issue
// it belongs to (SPEC §9.10 step 5).
//
// Owners are recorded at Prepare and cleared at Dispose, as the real provider does
// (workspace owners.go) — and a workspace whose owner record was dropped comes back
// with an empty Identifier rather than being omitted, because "this exists and
// nothing says whose it is" is the case the sweep has to be told about.
func (w *Workspaces) ListWorkspaces(context.Context) ([]core.WorkspaceRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failList != nil {
		return nil, w.failList
	}
	var out []core.WorkspaceRef
	for key := range w.dirs {
		out = append(out, core.WorkspaceRef{
			Key: key, Path: pathsFor(key).Path, Identifier: w.owners[key],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ForgetOwner drops a workspace's owner record while leaving the directory, which
// is what a workspace from an older BEN — or an interrupted disposal — looks like.
func (w *Workspaces) ForgetOwner(identifier string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.owners, "issue-"+identifier)
}

// ForgetBasePin drops a workspace's claim-base record while leaving the
// directory — out-of-band surgery, or a provider store restored from a backup
// older than the worktree. Recovery must epoch-fault the standing claim.
func (w *Workspaces) ForgetBasePin(identifier string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.claimBases, "issue-"+identifier)
}

// SetFailListWorkspaces makes the §9.10 step 5 listing fail until cleared.
func (w *Workspaces) SetFailListWorkspaces(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failList = err
}

// SetFailResolveWorkspace makes the §9.10 workspace resolution fail until
// cleared.
func (w *Workspaces) SetFailResolveWorkspace(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failResolve = err
}
