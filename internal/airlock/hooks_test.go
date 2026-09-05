package airlock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// Lifecycle hooks through the backend keep §5.2.6's contract exactly: the same
// phases, the same shared timeout, the same per-phase containment. What changes
// is where the shell runs — and, in the safe direction, that BEN composes no
// environment for it at all.

func hookRef(t *testing.T, id remote.Identity, phase remote.HookPhase, script string, timeout time.Duration) (remote.HookRef, remote.HookSpec) {
	t.Helper()
	hookSpec := remote.HookSpec{Identity: id, Phase: phase, Attempt: 1, Script: script, Timeout: timeout}
	digest, err := remote.HookRequestDigest(hookSpec)
	if err != nil {
		t.Fatalf("HookRequestDigest: %v", err)
	}
	return remote.HookRef{
		Identity: id, ID: remote.HookID("hook-" + string(phase)), Phase: phase,
		Attempt: 1, RequestDigest: digest,
	}, hookSpec
}

func TestHookRunsAsAShellInTheSandboxAndReportsItsExit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "git fetch --prune", time.Minute)

	status, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec)
	if err != nil {
		t.Fatalf("StartScript: %v", err)
	}
	if status.State != remote.HookStateRunning {
		t.Fatalf("a fresh hook is %s, want running", status.State)
	}

	runID := f.srv.RunIDs()[0]
	f.srv.Emit(runID, "stderr", []byte("fatal: not a git repository"))
	f.srv.Terminate(runID, airlocktest.Exited(128))

	done, err := f.sub.Hooks.WaitScript(ctx, ref)
	if err != nil {
		t.Fatalf("WaitScript: %v", err)
	}
	if done.State != remote.HookStateFinished {
		t.Fatalf("hook is %s, want finished", done.State)
	}
	if done.Result.ExitCode != 128 {
		t.Fatalf("exit code %d, want 128", done.Result.ExitCode)
	}
	if !strings.Contains(done.Result.Output, "not a git repository") {
		t.Fatalf("output %q lost the cause", done.Result.Output)
	}

	// The script is executed by an explicit shell BEN passes as argv[0]: Airlock
	// never inserts one, so a §5.2.6 script that works locally has to be given
	// the same quoting rules here.
	var sent bool
	for _, req := range f.srv.Requests() {
		if req.Method == "POST" && contains(req.Body, HookShell) && contains(req.Body, "git fetch --prune") {
			sent = true
		}
	}
	if !sent {
		t.Fatalf("the hook script was not dispatched through %s", HookShell)
	}
}

func TestGitPrepareRunsDirectlyWithControlPlaneScopeLabels(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.acquire(context.Background())
	scope := remote.GitScope{
		Phase: remote.GitPhasePrepare, Repository: testClaim.Repository,
		Branch: testBranch, BaseCommit: testBaseSHA, BaseBranch: "main",
		CheckoutCommit: testBaseSHA,
	}
	spec := remote.HookSpec{
		Identity: id, Phase: remote.HookGitPrepare, Attempt: 1,
		Argv: []string{"/usr/local/bin/airlock-git", "prepare"}, Git: scope, Timeout: time.Minute,
	}
	digest, err := remote.HookRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.HookRef{
		Identity: id, ID: "prepare-1", Phase: remote.HookGitPrepare,
		Attempt: 1, RequestDigest: digest,
	}
	if _, err := f.sub.Hooks.StartScript(context.Background(), ref, spec); err != nil {
		t.Fatalf("StartScript: %v", err)
	}
	var body startRunRequest
	for _, sent := range f.srv.Requests() {
		if sent.Method == "POST" && sent.Path == "/v2/sandboxes/"+id.SandboxID+"/runs" {
			if err := json.Unmarshal(sent.Body, &body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(body.Argv) != 2 || body.Argv[0] != "/usr/local/bin/airlock-git" || body.Argv[1] != "prepare" {
		t.Fatalf("prepare argv = %v", body.Argv)
	}
	for key, want := range map[string]string{
		"airlock.git.phase":           "prepare",
		"airlock.git.repository":      testClaim.Repository,
		"airlock.git.branch":          testBranch,
		"airlock.git.base_commit":     testBaseSHA,
		"airlock.git.base_branch":     "main",
		"airlock.git.checkout_commit": testBaseSHA,
	} {
		if got := body.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	if body.Env != nil || body.Labels["airlock.git.operation"] != "" {
		t.Fatalf("prepare request gained environment or publish authority: %+v", body)
	}
}

// §5.2.6 hook timeouts are ordinarily well under a minute, and the ticket's rule
// is that a hook keeps its existing timeout domain when it runs remotely. The
// sandbox idle-window floor is not this field's: a 30-second hook that Airlock
// was told to allow 60 would outlive the containment BEN promised the claim.
func TestAHookKeepsItsOwnTimeoutDomain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make ready", 30*time.Second)

	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("StartScript: %v", err)
	}

	var sent bool
	for _, req := range f.srv.Requests() {
		if req.Method == "POST" && contains(req.Body, "make ready") {
			sent = true
			if !contains(req.Body, `"hard_seconds":30`) {
				t.Fatalf("hook body %s does not carry its own 30s timeout", req.Body)
			}
		}
	}
	if !sent {
		t.Fatalf("the hook script was not dispatched")
	}
}

// A hook that ended without a process wait status did not succeed. Zero is what
// §5.2.6 reads as "not a reason to abort", and a hook nobody watched run must
// not be able to authorize an attempt.
func TestAHookWithNoWaitStatusIsNotASuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		terminal airlocktest.Terminal
		timedOut bool
	}{
		{"lost", airlocktest.Lost(), false},
		{"hard timeout", airlocktest.TimedOut(), true},
		{"failed with no exit code", airlocktest.Terminal{State: "failed", Reason: "unknown"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			ref, hookSpec := hookRef(t, id, remote.HookAfterCreate, "setup.sh", time.Minute)
			if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
				t.Fatalf("StartScript: %v", err)
			}
			f.srv.Terminate(f.srv.RunIDs()[0], tc.terminal)

			done, err := f.sub.Hooks.WaitScript(ctx, ref)
			if err != nil {
				t.Fatalf("WaitScript: %v", err)
			}
			if done.Result.ExitCode == 0 {
				t.Fatalf("%s reported success", tc.name)
			}
			if done.Result.TimedOut != tc.timedOut {
				t.Fatalf("TimedOut is %v, want %v", done.Result.TimedOut, tc.timedOut)
			}
		})
	}
}

// Restarting a before_run resolves the same mutation instead of executing it
// again. The key covers the sandbox, the phase, the attempt and the script.
func TestAHookStartIsIdempotentForTheExactFiring(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "touch /workspace/.prepared", time.Minute)

	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("StartScript: %v", err)
	}
	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("replayed StartScript: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("a replayed hook start produced %d runs, want 1: %v", len(got), got)
	}
	posts := 0
	for _, request := range f.srv.Requests() {
		if request.Method == "POST" && request.Path == "/v2/sandboxes/"+id.SandboxID+"/runs" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("a known hook run id caused %d startRun requests, want 1 total", posts)
	}
}

func TestAHookStartRecoversALostResponse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)

	f.srv.DropNextResponse("POST", "/v2/sandboxes/"+id.SandboxID+"/runs")
	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("StartScript past a lost response: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("a lost hook start produced %d runs, want 1: %v", len(got), got)
	}
}

func TestHookStartPersistsTheReplayFenceBeforeDispatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.acquire(context.Background())
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)

	crash := errors.New("disk full")
	f.store.(*memStore).setReserveFault(func(string, time.Time) error { return crash })
	if _, err := f.sub.Hooks.StartScript(context.Background(), ref, hookSpec); !errors.Is(err, crash) {
		t.Fatalf("StartScript past failed replay-fence persistence: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("hook dispatched before its replay fence landed: %v", got)
	}
}

func TestHookStartFencesAnAmbiguousRunAfterIdempotencyExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); !errors.Is(err, crash) {
		t.Fatalf("StartScript past failed run-id persistence: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	f.srv.Terminate(runID, airlocktest.Exited(0))
	store.setBindFault(nil)
	store.setBindingAttemptedAt(hookAddress(ref), time.Now().UTC().Add(-idempotencyReplayWindow-time.Minute))
	path := "/v2/sandboxes/" + id.SandboxID + "/runs"
	f.srv.ForgetIdempotency("POST", path, hookKey(ref))

	_, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec)
	mustBe(t, err, ErrStartReplayExpired)
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("expired hook recovery dispatched %d runs, want the original only: %v", len(got), got)
	}
}

func TestHookStartRefusesToReplayAfterAStaticTokenChangesPrincipal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); !errors.Is(err, crash) {
		t.Fatalf("StartScript past failed run-id persistence: %v", err)
	}
	store.setBindFault(nil)
	before := len(f.srv.Requests())

	const rotatedToken = "airlocktest-other-hook-principal-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "other-client"})
	_, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec)
	mustBe(t, err, ErrSubstrateBinding)
	if got := len(f.srv.Requests()); got != before {
		t.Fatalf("runtime principal refusal sent %d new requests, want 0", got-before)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("static token rotation left %d hook runs, want the ambiguous original: %v", len(got), got)
	}
}

// StatusScript is how a hook whose start was ambiguous is resolved by
// observation rather than by starting it again (remote.runHook). It answers
// from the durable binding alone, so a daemon that restarted between the start
// and the verdict observes the firing it already dispatched instead of firing
// the script a second time — which for a §5.2.6 hook is a side effect, not a
// retry.
func TestStatusScriptObservesAFiringAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)

	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("StartScript: %v", err)
	}
	runID := f.srv.RunIDs()[0]

	restarted := f.rebuild()
	status, err := restarted.Hooks.StatusScript(ctx, ref)
	if err != nil {
		t.Fatalf("StatusScript after restart: %v", err)
	}
	if status.State != remote.HookStateRunning {
		t.Fatalf("hook is %s, want running", status.State)
	}

	f.srv.Emit(runID, "stdout", []byte("deps ok\n"))
	f.srv.Terminate(runID, airlocktest.Exited(0))

	status, err = restarted.Hooks.StatusScript(ctx, ref)
	if err != nil {
		t.Fatalf("StatusScript once terminal: %v", err)
	}
	if status.State != remote.HookStateFinished {
		t.Fatalf("hook is %s, want finished", status.State)
	}
	if status.Result.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0", status.Result.ExitCode)
	}
	if !strings.Contains(status.Result.Output, "deps ok") {
		t.Fatalf("output %q lost the hook's own record", status.Result.Output)
	}
	// Observation is not dispatch: the restarted daemon must not have fired the
	// script again.
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("observing the hook produced %d runs, want 1: %v", len(got), got)
	}
}

func TestHookStartRefusesAMismatchedFiring(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", time.Minute)

	other := hookSpec
	other.Phase = remote.HookAfterRun
	_, err := f.sub.Hooks.StartScript(ctx, ref, other)
	mustBe(t, err, remote.ErrHookMismatch)

	incomplete := ref
	incomplete.RequestDigest = ""
	if _, err := f.sub.Hooks.StartScript(ctx, incomplete, hookSpec); err == nil {
		t.Fatal("an incomplete hook reference was accepted")
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("a refused hook start created a run: %v", got)
	}
}

// The whole §5.2.6 path through remote.RunHook: the ordering table and the
// containment rules are internal/remote's, and this asserts the Airlock
// executor satisfies them unchanged.
func TestRunHookAppliesTheWorkflowsContainmentOverTheBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase  remote.HookPhase
		aborts bool
	}{
		{remote.HookAfterCreate, true},
		{remote.HookBeforeRun, true},
		{remote.HookAfterRun, false},
		{remote.HookBeforeRemove, false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			hooks := remote.Hooks{
				AfterCreate: "a", BeforeRun: "b", AfterRun: "c", BeforeRemove: "d",
				Timeout: time.Minute,
			}
			store := remotetest.NewMemHookStore()

			// The hook run is terminated as soon as it is dispatched, so RunHook's
			// own wait resolves it; a background goroutine is what a real backend's
			// asynchrony looks like from here.
			done := make(chan struct{})
			go func() {
				defer close(done)
				waitFor(t, func() bool { return len(f.srv.RunIDs()) > 0 })
				f.srv.Terminate(f.srv.RunIDs()[0], airlocktest.Exited(3))
			}()

			err := remote.RunHook(ctx, f.sub.Hooks, store, remote.HookInvocation{
				Identity: id, ID: remote.HookID("h1"), Phase: tc.phase, Attempt: 1, Hooks: hooks,
			})
			<-done
			mustBe(t, err, remote.ErrHookFailed)
			if got := remote.Aborts(tc.phase, err); got != tc.aborts {
				t.Fatalf("Aborts(%s) is %v, want %v", tc.phase, got, tc.aborts)
			}
		})
	}
}

// An unbounded hook on a backend BEN cannot signal holds the claim forever, so
// a missing timeout is a refusal rather than a default.
func TestAHookWithoutATimeoutIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookBeforeRun, "make deps", 0)

	_, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec)
	mustBe(t, err, remote.ErrHookFailed)
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("an unbounded hook was dispatched: %v", got)
	}
}

// BEN composes no environment for a remote hook. The sandbox holds none of the
// daemon's secrets, so the profile defines what a script sees — a rule that is
// stronger here than locally, not weaker.
func TestARemoteHookCarriesNoBenComposedEnvironment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref, hookSpec := hookRef(t, id, remote.HookAfterCreate, "printenv", time.Minute)
	if _, err := f.sub.Hooks.StartScript(ctx, ref, hookSpec); err != nil {
		t.Fatalf("StartScript: %v", err)
	}

	for _, req := range f.srv.Requests() {
		if req.Method != "POST" || !contains(req.Body, HookShell) {
			continue
		}
		if contains(req.Body, `"env":`) {
			t.Fatalf("the hook request composed an environment: %s", req.Body)
		}
		if contains(req.Body, "BEN_") {
			t.Fatalf("the hook request carried a BEN-namespaced variable: %s", req.Body)
		}
	}
}
