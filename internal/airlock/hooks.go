package airlock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// HookShell is the argv[0] a lifecycle hook's script is executed by.
//
// Airlock executes argv directly and never inserts a shell — a caller that wants
// shell semantics passes one and owns the quoting. `/bin/sh` by absolute path
// rather than by name, because direct execution is not a PATH search, and POSIX
// puts the shell there. The local provider's `sh -c <script>` is the same
// contract with the same quoting rules (workspace.runHook); a §5.2.6 script that
// works locally has to work here, or the substrate has changed the workflow's
// meaning.
const HookShell = "/bin/sh"

// hookOutputLimit bounds how much of a hook's combined output is retained in
// remote.HookResult.Output. The tail is what matters — shells and git put the
// cause last — but the head is what a bounded read gets, so this reads the whole
// window and keeps the tail.
const hookOutputLimit = 8 << 10

// Hooks is remote.HookExec over Airlock runs: one lifecycle hook firing is one
// run in the claim's sandbox.
//
// Nothing about §5.2.6's contract moves. The four phases fire in the same order,
// share the same timeout, and keep the same per-phase containment — that table
// lives in internal/remote and is deliberately not restated here, because it is
// the *workflow's* contract rather than the substrate's.
//
// One thing genuinely changes, in the safe direction: this type composes no
// environment at all. Locally a hook gets core.EnvAllowlist and nothing else,
// because the daemon's host holds secrets a repo-authored script must not reach.
// A backend sandbox holds none of them, so the profile defines what a script
// sees and BEN adds nothing.
type Hooks struct {
	client   *client
	store    Store
	timeouts Timeouts
	binding  SubstrateBinding
}

// StartScript dispatches one hook firing, idempotently for the exact reference
// and request.
//
// A lost response is resolved by replaying this exact call inside Airlock's
// bounded idempotency window. The write-ahead StartBinding fences that window;
// once a RunID is known it is read directly, and once an unanswered start is too
// old it is parked rather than executing a side-effecting hook again.
func (h *Hooks) StartScript(ctx context.Context, ref remote.HookRef, spec remote.HookSpec) (remote.HookStatus, error) {
	if !ref.Complete() {
		return remote.HookStatus{}, fmt.Errorf("%w: incomplete hook reference", remote.ErrHookMismatch)
	}
	if spec.Identity != ref.Identity || spec.Phase != ref.Phase || spec.Attempt != ref.Attempt {
		return remote.HookStatus{}, fmt.Errorf("%w: %s names a different firing than its request", remote.ErrHookMismatch, ref.Key())
	}
	if spec.Timeout <= 0 {
		return remote.HookStatus{}, fmt.Errorf("%w: %s has no timeout", remote.ErrHookFailed, ref.Phase)
	}
	if err := spec.Git.Validate(); err != nil {
		return remote.HookStatus{}, fmt.Errorf("%w: %s has invalid Git scope: %v", remote.ErrHookMismatch, ref.Phase, err)
	}
	if (strings.TrimSpace(spec.Script) == "") == (len(spec.Argv) == 0) {
		return remote.HookStatus{}, fmt.Errorf("%w: %s must supply exactly one of script or argv", remote.ErrHookMismatch, ref.Phase)
	}
	digest, err := remote.HookRequestDigest(spec)
	if err != nil {
		return remote.HookStatus{}, fmt.Errorf("%w: %s: %v", remote.ErrHookMismatch, ref.Key(), err)
	}
	if digest != ref.RequestDigest {
		return remote.HookStatus{}, fmt.Errorf("%w: %s carries a different request digest", remote.ErrHookMismatch, ref.Key())
	}
	if _, err := loadBoundSandbox(h.store, h.binding, ref.Identity.Claim); err != nil {
		return remote.HookStatus{}, err
	}
	binding, auth, replayCtx, cancel, err := prepareStart(ctx, h.client, h.store, h.binding, hookAddress(ref))
	if err != nil {
		return remote.HookStatus{}, err
	}
	defer cancel()
	if binding.RunID != "" {
		run, err := h.get(ctx, ref, binding.RunID)
		if err != nil {
			return remote.HookStatus{}, err
		}
		return h.statusFrom(ctx, ref, run)
	}

	argv := []string{HookShell, "-c", spec.Script}
	if len(spec.Argv) > 0 {
		argv = append([]string(nil), spec.Argv...)
	}
	labels := map[string]string{
		"ben.claim": ref.Identity.Claim.String(),
		"ben.hook":  string(ref.Phase),
	}
	addGitLabels(labels, spec.Git)

	var run Run
	err = h.client.do(replayCtx, request{
		method:    "POST",
		path:      "/v2/sandboxes/" + url.PathEscape(ref.Identity.SandboxID) + "/runs",
		idem:      hookKey(ref),
		authToken: auth.token,
		body: startRunRequest{
			Argv:     argv,
			Stdin:    &runStdinRequest{Mode: StdinClosed},
			Timeouts: &runTimeoutsRequest{HardSeconds: seconds(spec.Timeout)},
			Labels:   labels,
		},
		out: &run,
	})
	if hasCode(err, CodeIdempotencyKeyConflict) {
		return remote.HookStatus{}, fmt.Errorf("%w: %s: %w", remote.ErrHookMismatch, ref.Key(), err)
	}
	if err != nil {
		return remote.HookStatus{}, err
	}
	if run.RunID == "" {
		return remote.HookStatus{}, fmt.Errorf("%w: %s answered with no run id", ErrUnexpectedRun, ref.Key())
	}
	if err := h.store.SaveBinding(hookAddress(ref), h.binding, run.RunID); err != nil {
		return remote.HookStatus{}, err
	}
	return h.statusFrom(ctx, ref, run)
}

func (h *Hooks) StatusScript(ctx context.Context, ref remote.HookRef) (remote.HookStatus, error) {
	runID, err := h.resolve(ref)
	if err != nil {
		return remote.HookStatus{}, err
	}
	run, err := h.get(ctx, ref, runID)
	if err != nil {
		return remote.HookStatus{}, err
	}
	return h.statusFrom(ctx, ref, run)
}

// WaitScript blocks until the firing is finished or ctx ends.
func (h *Hooks) WaitScript(ctx context.Context, ref remote.HookRef) (remote.HookStatus, error) {
	runID, err := h.resolve(ref)
	if err != nil {
		return remote.HookStatus{}, err
	}
	for {
		var run Run
		err := h.client.do(ctx, request{
			method: "POST",
			path:   h.runPath(ref, runID) + "/wait",
			body:   waitForRunRequest{WaitSeconds: int(h.timeouts.PollWait / time.Second)},
			long:   true,
			out:    &run,
		})
		if hasCode(err, CodeNotFound) {
			return remote.HookStatus{}, fmt.Errorf("%w: %s: %w", remote.ErrNoHook, ref.Key(), err)
		}
		if err != nil {
			return remote.HookStatus{}, err
		}
		if run.State.Terminal() {
			return h.statusFrom(ctx, ref, run)
		}
		if err := ctx.Err(); err != nil {
			return remote.HookStatus{}, err
		}
	}
}

// statusFrom translates one run into the hook vocabulary.
//
// The exit code is the load-bearing translation, and the interesting case is a
// terminal run that has none. `failed` and `lost` carry no process wait status —
// a pre-spawn cancellation, an infrastructure failure, a runner Airlock can no
// longer observe — and every one of them is a hook that did not succeed. They
// report a nonzero exit rather than zero, because zero is the value §5.2.6 reads
// as "this hook is not a reason to abort", and a hook nobody watched run must
// not be able to authorize an attempt.
func (h *Hooks) statusFrom(ctx context.Context, ref remote.HookRef, run Run) (remote.HookStatus, error) {
	if !run.State.Terminal() {
		return remote.HookStatus{State: remote.HookStateRunning, Domain: domainStateOf(run.Termination.DomainQuiet)}, nil
	}
	result := remote.HookResult{TimedOut: run.Termination.Reason.TimedOut()}
	switch {
	case run.ExitCode != nil:
		result.ExitCode = *run.ExitCode
	case run.Signal != nil && *run.Signal != "":
		// Killed by a signal. POSIX shells spell this 128+n; the exact number is
		// diagnosis, and what matters is that it is not zero.
		result.ExitCode = 128
	default:
		result.ExitCode = 1
	}
	output, err := h.output(ctx, ref, run.RunID)
	if err != nil {
		// Diagnosis, not the verdict. A hook whose exit status is known but whose
		// output could not be read is still a decided hook, and losing the
		// decision over a missing explanation would be the worse trade.
		result.Output = fmt.Sprintf("(hook output unavailable: %v)", err)
		return remote.HookStatus{State: remote.HookStateFinished, Result: result, Domain: domainStateOf(run.Termination.DomainQuiet)}, nil
	}
	result.Output = output
	return remote.HookStatus{State: remote.HookStateFinished, Result: result, Domain: domainStateOf(run.Termination.DomainQuiet)}, nil
}

func addGitLabels(labels map[string]string, scope remote.GitScope) {
	if scope.Empty() {
		return
	}
	labels["airlock.git.phase"] = string(scope.Phase)
	labels["airlock.git.repository"] = scope.Repository
	labels["airlock.git.branch"] = scope.Branch
	labels["airlock.git.base_commit"] = scope.BaseCommit
	labels["airlock.git.base_branch"] = scope.BaseBranch
	if scope.CheckoutCommit != "" {
		labels["airlock.git.checkout_commit"] = scope.CheckoutCommit
	}
	if scope.Operation != "" {
		labels["airlock.git.operation"] = scope.Operation
	}
}

// output reads the firing's combined streams, bounded, keeping the tail.
func (h *Hooks) output(ctx context.Context, ref remote.HookRef, runID string) (string, error) {
	var b strings.Builder
	after := int64(0)
	for {
		query := url.Values{}
		query.Set("after", strconv.FormatInt(after, 10))
		query.Set("limit", strconv.Itoa(eventPageLimit))
		var page runEventPage
		if err := h.client.do(ctx, request{
			method: "GET", path: h.runPath(ref, runID) + "/events", query: query, out: &page,
		}); err != nil {
			return "", err
		}
		for _, item := range page.Events {
			var ev runEvent
			if err := json.Unmarshal(item, &ev); err != nil {
				return "", err
			}
			if ev.Kind != EventOutput {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(ev.DataB64)
			if err != nil {
				return "", err
			}
			b.Write(data)
		}
		if !page.HasMore || page.Cursor <= after {
			break
		}
		after = page.Cursor
	}
	out := b.String()
	if len(out) > hookOutputLimit {
		out = out[len(out)-hookOutputLimit:]
	}
	return out, nil
}

func (h *Hooks) get(ctx context.Context, ref remote.HookRef, runID string) (Run, error) {
	var run Run
	err := h.client.do(ctx, request{method: "GET", path: h.runPath(ref, runID), out: &run})
	if hasCode(err, CodeNotFound) {
		return Run{}, fmt.Errorf("%w: %s: %w", remote.ErrNoHook, ref.Key(), err)
	}
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

func (h *Hooks) resolve(ref remote.HookRef) (string, error) {
	if _, err := loadBoundSandbox(h.store, h.binding, ref.Identity.Claim); err != nil {
		return "", err
	}
	binding, err := h.store.LoadBinding(hookAddress(ref))
	if errors.Is(err, ErrNoRunBinding) {
		return "", fmt.Errorf("%w: %s: %w", remote.ErrNoHook, ref.Key(), err)
	}
	if err != nil {
		return "", err
	}
	if err := requireSubstrateBinding(binding.Substrate, h.binding, hookAddress(ref)); err != nil {
		return "", err
	}
	if binding.RunID == "" {
		return "", fmt.Errorf("%w: %s has an unanswered start: %w", remote.ErrNoHook, ref.Key(), ErrNoRunBinding)
	}
	return binding.RunID, nil
}

func (h *Hooks) runPath(ref remote.HookRef, runID string) string {
	return "/v2/sandboxes/" + url.PathEscape(ref.Identity.SandboxID) + "/runs/" + url.PathEscape(runID)
}

// hookAddress is the binding-store key for one firing. The request digest is
// part of it for HookRef.Complete's reason: reusing a hook id for a different
// script or timeout is a different firing, and a binding that ignored the digest
// would let the second one attach to the first one's run.
func hookAddress(ref remote.HookRef) string {
	return "hook/" + ref.Key().String() + "@" + ref.RequestDigest
}
