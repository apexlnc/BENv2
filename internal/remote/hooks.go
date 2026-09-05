package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The §5.2.6 lifecycle hooks, executed through the backend instead of through
// `sh -c` on the daemon's host.
//
// What changes is only *where* the shell runs. The ordering, the shared timeout
// and the per-hook failure containment are the workflow's contract, not the
// substrate's, so they are restated here as a table rather than reimplemented at
// four call sites — and the table is what a test can hold the remote path to,
// against the same rules internal/workspace applies locally.
//
// One thing genuinely does change, and it changes in the safe direction. Locally
// a hook's environment is core.EnvAllowlist and nothing else, because the daemon
// has secrets a repo-authored script must not reach (workspace.hookEnv). A
// backend sandbox is not the daemon's host and holds none of them, so the rule
// is stronger here rather than weaker: this package composes no environment at
// all and the backend runs the script with whatever the profile defines.

// HookPhase is the closed set of §5.2.6 lifecycle hooks, in the order they fire.
type HookPhase string

const (
	HookAfterCreate  HookPhase = "after_create"
	HookBeforeRun    HookPhase = "before_run"
	HookAfterRun     HookPhase = "after_run"
	HookBeforeRemove HookPhase = "before_remove"

	// HookRestore is **BEN's own** script, not a §5.2.6 lifecycle hook, and it
	// is kept out of hookOrder deliberately: HookPhases() is the workflow's
	// contract — four phases, in that order, with that containment — and a fifth
	// entry there would make a workflow-shaped promise about something no
	// workflow can configure.
	//
	// It is a phase at all because the durable machinery is the same: a hook
	// firing is addressed by (claim, id) and fingerprinted by (identity, phase,
	// attempt, script, timeout), so a BEN-owned script needs a phase to be
	// distinguishable from the operator's under that fingerprint. Its containment
	// is ContainmentOf's default, which is abort — a restore that did not run
	// leaves the tree in whatever state the previous attempt left it, and that is
	// exactly what the restore exists to refuse (see remotews).
	HookRestore HookPhase = "ben_restore"
	// HookGitPrepare and HookGitPublish are BEN-owned trusted commands. They
	// reuse the hook executor's durable exact-once journal, but are deliberately
	// excluded from HookPhases: no workflow can configure or suppress them.
	HookGitPrepare HookPhase = "ben_git_prepare"
	HookGitPublish HookPhase = "ben_git_publish"
)

// Containment is what a hook's failure costs, per SPEC §5.2.6. The two values
// are the two the local provider already implements, and they are stated as data
// so the remote path cannot quietly pick a third.
type Containment uint8

const (
	// ContainmentAbort — the failure aborts what the hook was gating:
	// after_create aborts workspace creation, before_run aborts the attempt.
	// Reported as ErrHookFailed.
	ContainmentAbort Containment = iota
	// ContainmentIgnore — the failure is reported to the caller's log and
	// nothing else. after_run fires after every attempt whatever its outcome,
	// and before_remove must not be able to prevent a disposal.
	ContainmentIgnore
)

// hookOrder is the four hooks with their containment, in firing order.
//
// A package-level table, unexported, for the reason config.deploymentModes is:
// an exported slice is a mutable global, and this one decides whether a failing
// hook aborts an attempt.
var hookOrder = [4]struct {
	Phase       HookPhase
	Containment Containment
}{
	{HookAfterCreate, ContainmentAbort},
	{HookBeforeRun, ContainmentAbort},
	{HookAfterRun, ContainmentIgnore},
	{HookBeforeRemove, ContainmentIgnore},
}

// HookPhases returns the four phases in firing order. A copy, so a caller cannot
// reorder the sequence the rules are stated over.
func HookPhases() []HookPhase {
	out := make([]HookPhase, 0, len(hookOrder))
	for _, h := range hookOrder {
		out = append(out, h.Phase)
	}
	return out
}

// ContainmentOf reports what a phase's failure costs. An unknown phase is
// ContainmentAbort: a hook nobody classified must not be the one whose failure
// is ignored.
func ContainmentOf(phase HookPhase) Containment {
	for _, h := range hookOrder {
		if h.Phase == phase {
			return h.Containment
		}
	}
	return ContainmentAbort
}

func validHookPhase(phase HookPhase) bool {
	if phase == HookRestore || phase == HookGitPrepare || phase == HookGitPublish {
		return true
	}
	for _, h := range hookOrder {
		if h.Phase == phase {
			return true
		}
	}
	return false
}

// Hooks are the four scripts and their shared timeout, mirroring
// workspace.Hooks. Scripts are opaque shell text; $VAR indirection deliberately
// does not apply — expansion is the shell's job, wherever the shell runs.
type Hooks struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	// Timeout bounds each hook run. ≤0 means the caller supplied no bound, and
	// RunHook refuses rather than running unbounded: a hook with no timeout on a
	// backend BEN cannot signal is a claim held forever.
	Timeout time.Duration
}

// Script returns the text for a phase, and whether there is one to run.
func (h Hooks) Script(phase HookPhase) (string, bool) {
	var s string
	switch phase {
	case HookAfterCreate:
		s = h.AfterCreate
	case HookBeforeRun:
		s = h.BeforeRun
	case HookAfterRun:
		s = h.AfterRun
	case HookBeforeRemove:
		s = h.BeforeRemove
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// HookResult is what a backend reports about one hook run.
type HookResult struct {
	// ExitCode is the script's exit status; zero is success.
	ExitCode int
	// Output is the tail of the combined streams, bounded by the backend. It is
	// diagnosis, and it is where shells and git put the cause.
	Output string
	// TimedOut says the backend stopped the hook at the deadline rather than
	// the script exiting. Kept apart from a nonzero exit for the reason the
	// local path keeps them apart: the operator-facing cause is different, and
	// only one of them means "your script is slower than the bound".
	TimedOut bool
}

// HookInvocation supplies the stable identity of one hook firing. ID is unique
// per phase and attempt; it is chosen and persisted by BEN before dispatch.
type HookInvocation struct {
	Identity Identity
	ID       HookID
	Phase    HookPhase
	Attempt  int
	Hooks    Hooks
}

// RunHook fires one lifecycle hook through the backend and applies §5.2.6's
// containment for that phase.
//
// nil means the hook is absent or succeeded. A non-nil error under
// ContainmentAbort wraps ErrHookFailed and the caller must abort; under
// ContainmentIgnore the caller logs it and continues — the error is returned
// rather than swallowed here so the containment decision stays with the caller
// that has the logger, exactly as workspace.AfterRun keeps it.
func RunHook(ctx context.Context, exec HookExec, store HookStore, in HookInvocation) error {
	script, ok := in.Hooks.Script(in.Phase)
	if !ok {
		return nil
	}
	return RunScript(ctx, exec, store, ScriptInvocation{
		Identity: in.Identity, ID: in.ID, Phase: in.Phase, Attempt: in.Attempt,
		Script: script, Timeout: in.Hooks.Timeout,
	})
}

// ScriptInvocation is one script firing with its text already chosen — what
// RunHook resolves a HookInvocation into, and the shape a BEN-owned script
// command is submitted in.
//
// A second entry point rather than a fifth workflow hook, and the same
// implementation underneath: the recovery protocol below is the delicate part —
// dispatch mark before the call, exact-request replay for a lost response, the
// terminal result durable before the caller may proceed — and a second copy of
// it for BEN's own scripts is a second thing to get wrong on a restart.
type ScriptInvocation struct {
	Identity Identity
	ID       HookID
	Phase    HookPhase
	Attempt  int
	Script   string
	// Argv is the direct-exec alternative used by BEN-owned trusted commands.
	// Exactly one of Script and Argv must be present.
	Argv []string
	Git  GitScope
	// Timeout bounds the run. ≤0 is refused rather than run unbounded: a script
	// with no timeout on a backend BEN cannot signal is a claim held forever.
	Timeout time.Duration
}

// CommandInvocation is a fixed direct-exec command owned by BEN rather than a
// repository-authored lifecycle hook. Git is trusted scope and never comes
// from the command's environment or arguments.
type CommandInvocation struct {
	Identity Identity
	ID       HookID
	Phase    HookPhase
	Attempt  int
	Argv     []string
	Git      GitScope
	Timeout  time.Duration
}

// RunCommand executes one trusted command through the same crash-safe journal
// as hooks and returns its bounded combined output.
func RunCommand(ctx context.Context, exec HookExec, store HookStore, in CommandInvocation) (HookResult, error) {
	return runScript(ctx, exec, store, ScriptInvocation{
		Identity: in.Identity, ID: in.ID, Phase: in.Phase, Attempt: in.Attempt,
		Argv: append([]string(nil), in.Argv...), Git: in.Git, Timeout: in.Timeout,
	})
}

// RunScript fires one script through the backend and reports its result, under
// the containment its phase declares (ContainmentOf).
func RunScript(ctx context.Context, exec HookExec, store HookStore, in ScriptInvocation) error {
	_, err := runScript(ctx, exec, store, in)
	return err
}

func runScript(ctx context.Context, exec HookExec, store HookStore, in ScriptInvocation) (HookResult, error) {
	script := strings.TrimSpace(in.Script)
	if script == "" && len(in.Argv) == 0 {
		return HookResult{}, nil
	}
	if script != "" && len(in.Argv) != 0 {
		return HookResult{}, fmt.Errorf("%w: %s supplies both script and argv", ErrHookFailed, in.Phase)
	}
	if len(in.Argv) > 0 && strings.TrimSpace(in.Argv[0]) == "" {
		return HookResult{}, fmt.Errorf("%w: %s has an empty executable", ErrHookFailed, in.Phase)
	}
	if err := in.Git.Validate(); err != nil {
		return HookResult{}, fmt.Errorf("%w: %s: %v", ErrHookFailed, in.Phase, err)
	}
	if in.Timeout <= 0 {
		return HookResult{}, fmt.Errorf("%w: %s has no timeout, and an unbounded hook on a backend BEN cannot "+
			"signal holds the claim forever", ErrHookFailed, in.Phase)
	}
	if exec == nil {
		return HookResult{}, fmt.Errorf("%w: %s cannot run — this substrate has no hook executor", ErrHookFailed, in.Phase)
	}
	if store == nil {
		return HookResult{}, fmt.Errorf("%w: %s cannot run — this substrate has no durable hook store", ErrHookFailed, in.Phase)
	}
	spec := HookSpec{
		Identity: in.Identity, Phase: in.Phase, Attempt: in.Attempt,
		Script: script, Argv: append([]string(nil), in.Argv...), Git: in.Git, Timeout: in.Timeout,
	}
	digest, err := HookRequestDigest(spec)
	if err != nil {
		return HookResult{}, fmt.Errorf("%w: %s: %w", ErrHookFailed, in.Phase, err)
	}
	ref := HookRef{
		Identity: in.Identity, ID: in.ID, Phase: in.Phase,
		Attempt: in.Attempt, RequestDigest: digest,
	}
	if !ref.Complete() {
		return HookResult{}, fmt.Errorf("%w: %s has an incomplete durable identity: %w", ErrHookFailed, in.Phase, ErrHookMismatch)
	}

	journal, err := OpenHookJournal(store, ref.Key())
	switch {
	case err == nil:
		if journal.Record().Ref != ref {
			return HookResult{}, fmt.Errorf("%w: %s: %w", ErrHookFailed, in.Phase, ErrHookMismatch)
		}
	case errors.Is(err, ErrNoRecord):
		journal, err = ReserveHook(ctx, store, ref)
		if err != nil {
			return HookResult{}, fmt.Errorf("%w: reserving %s: %w", ErrHookFailed, in.Phase, err)
		}
	default:
		return HookResult{}, fmt.Errorf("%w: opening %s: %w", ErrHookFailed, in.Phase, err)
	}

	_, dispatched, result := journal.Resume()
	if result != nil {
		return *result, hookResultError(in.Phase, in.Timeout, *result)
	}

	var status HookStatus
	if dispatched {
		status, err = exec.StartScript(ctx, ref, spec)
		if err != nil {
			return HookResult{}, fmt.Errorf("%w: %s start replay remains ambiguous: %w", ErrHookFailed, in.Phase, err)
		}
	}
	if !dispatched {
		status, err = journal.Dispatch(ctx, func(ctx context.Context, ref HookRef) (HookStatus, error) {
			return exec.StartScript(ctx, ref, spec)
		})
	}
	if err != nil {
		startErr := err
		status, err = exec.StartScript(ctx, ref, spec)
		if err != nil {
			return HookResult{}, fmt.Errorf("%w: %s start was ambiguous (%v) and exact replay could not resolve it: %w",
				ErrHookFailed, in.Phase, startErr, err)
		}
	}
	status, err = waitHookQuiet(ctx, exec, ref, status)
	if err != nil {
		return HookResult{}, fmt.Errorf("%w: reconciling %s to domain quiet: %w", ErrHookFailed, in.Phase, err)
	}
	if err := journal.Complete(context.WithoutCancel(ctx), status.Result); err != nil {
		return HookResult{}, fmt.Errorf("%w: recording %s result: %w", ErrHookFailed, in.Phase, err)
	}
	return status.Result, hookResultError(in.Phase, in.Timeout, status.Result)
}

const hookQuietPoll = 100 * time.Millisecond

// waitHookQuiet keeps terminal process state and domain state independent. A
// backend may reap the direct hook process before it has proved every
// descendant gone; that is an intermediate observation, not a failed hook and
// not permission to enter the next phase.
func waitHookQuiet(ctx context.Context, exec HookExec, ref HookRef, status HookStatus) (HookStatus, error) {
	for {
		switch status.State {
		case HookStateUnknown:
			var err error
			status, err = exec.StatusScript(ctx, ref)
			if err != nil {
				return HookStatus{}, err
			}
		case HookStateRunning:
			var err error
			status, err = exec.WaitScript(ctx, ref)
			if err != nil {
				return HookStatus{}, err
			}
		case HookStateFinished:
			if status.Domain == DomainStateQuiet {
				return status, nil
			}
			timer := time.NewTimer(hookQuietPoll)
			select {
			case <-ctx.Done():
				timer.Stop()
				return HookStatus{}, ctx.Err()
			case <-timer.C:
			}
			var err error
			status, err = exec.StatusScript(ctx, ref)
			if err != nil {
				return HookStatus{}, err
			}
		default:
			return HookStatus{}, fmt.Errorf("backend returned invalid hook state %d", status.State)
		}
	}
}

func hookResultError(phase HookPhase, timeout time.Duration, res HookResult) error {
	switch {
	case res.TimedOut:
		return fmt.Errorf("%w: %s timed out after %s: %s", ErrHookFailed, phase, timeout, strings.TrimSpace(res.Output))
	case res.ExitCode != 0:
		return fmt.Errorf("%w: %s exited %d: %s", ErrHookFailed, phase, res.ExitCode, strings.TrimSpace(res.Output))
	}
	return nil
}

// Aborts reports whether a RunHook error must abort what the hook was gating.
// It reads the phase's containment rather than the error, because the error says
// what went wrong and only the phase says what that costs.
func Aborts(phase HookPhase, err error) bool {
	return err != nil && errors.Is(err, ErrHookFailed) && ContainmentOf(phase) == ContainmentAbort
}
