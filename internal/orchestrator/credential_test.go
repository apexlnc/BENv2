package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// Decision 1's routing table, as a test table. A credential source can fail at
// five places, and the class it reports controls the route at exactly **two** of
// them: §9.8's automatic attempt retry at `Prepare` and `Start`, and §9.7's
// verification routing (SPEC §9.7, §9.8, amendments 12–14).
//
// Everywhere else the class is *read* — for log severity — and routes nothing.
// Reporting on a class is not routing by it, and the three easy ones to get
// wrong are all here: a retry in `verifying` that records no attempt, an owed
// write that stays owed, and a park that does not spend the retry budget.

const testAuthority = "octo:https://octo.example#srhg-ai-7cef3f93#ben-tracker"

func credErr(class core.CredentialErrorClass) error {
	return &core.CredentialError{
		Class:     class,
		Authority: testAuthority,
		Err:       errors.New("the issuer would not say"),
	}
}

// unclassified is an error constructed **without** a class: the zero value, which
// is what a new kind or a new error path produces by omission (mutation 14).
func unclassified() error {
	return &core.CredentialError{Authority: testAuthority, Err: errors.New("nobody said")}
}

// classCases is decision 1's table of classes. Every routing test runs all
// three, because both halves of the contract are claims about the *set*: the
// zero class must park where transient retries, and must retry where transient
// does — and the second half is what rev 6's prose got wrong.
var classCases = []struct {
	name string
	err  error
}{
	{"transient", credErr(core.CredentialTransient)},
	{"permanent", credErr(core.CredentialPermanent)},
	{"the zero class", unclassified()},
}

// A credential failure preparing the workspace routes by class: transient
// retries through §9.8's backoff as `FailureCredential`, unknown and permanent
// park through the new `preparing → needs-review` edge (amendment 12).
func TestPrepareCredentialFailureRoutesByClass(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		want   State
		reason core.FailureReason
	}{
		{"transient retries as FailureCredential", credErr(core.CredentialTransient), StateBackoff, core.FailureCredential},
		{"permanent parks", credErr(core.CredentialPermanent), StateNeedsReview, ""},
		{"the zero class parks", unclassified(), StateNeedsReview, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues:     []core.Issue{fake.Issue("1", epoch)},
				prepareErr: tt.err,
			})
			h.WaitState("1", tt.want)

			// Either way the attempt is ended and recorded: a dispatched
			// preparation *is* an attempt in the §9.6 accounting, so a park does
			// not un-count it.
			waitFor(t, "the attempt outcome to be recorded", func() bool {
				return len(h.o.Attempts.For("1")) == 1
			})
			// `launch_error` is what a *launch* that failed is. A transient
			// credential failure is `credential` — the new §7.3 reason, retryable
			// — and a park carries no §7.3 reason at all: the agent did not fail,
			// it never ran, and `credential` in that taxonomy means transient
			// specifically (amendment 8).
			if got := h.o.Attempts.For("1")[0].FailureReason; got != tt.reason {
				t.Errorf("attempt reason = %q, want %q", got, tt.reason)
			}
		})
	}
}

// A park does not spend the remaining automatic retry budget: a misconfigured
// trust policy fails identically every time, so retrying it would reach the same
// park three attempts later with less to say about it (SPEC §9.8, amendment 14).
//
// Asserted as an absence over several ticks — the workspace is never prepared
// again — rather than off the attempt counter, which a record sitting in
// needs-review would satisfy whether or not the loop meant to stop.
func TestACredentialParkDoesNotSpendTheRetryBudget(t *testing.T) {
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		prepareErr: credErr(core.CredentialPermanent),
	})
	h.WaitState("1", StateNeedsReview)
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Fatalf("prepared %d times before the park, want the one dispatch", got)
	}

	for range 5 {
		h.Tick()
	}
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Errorf("prepared %d times, want the park to leave the retry budget unspent", got)
	}
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q, want the record still parked for a human", got)
	}
	if got := h.statusFor("1").Attempt; got != 1 {
		t.Errorf("attempt = %d, want the park to have consumed only the dispatched one", got)
	}
}

// A pre-launch credential park has no run outcome. If an older attempt did
// run, its outcome and account must not be stamped with the parked attempt's
// number or carried across a human re-queue as though they described that park.
func TestACredentialParkDoesNotRelabelAnOlderRunOutcome(t *testing.T) {
	h := start(t, harnessOpts{
		issues:    []core.Issue{fake.Issue("1", epoch)},
		failStart: credErr(core.CredentialTransient),
	})
	h.WaitState("1", StateBackoff)

	// Attempt 1 has a real prompt-facing outcome. Attempt 2 is refused before
	// launch and parks, which is the crossing where the old value was reused.
	h.Runner.SetFailStart(credErr(core.CredentialPermanent))
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateNeedsReview)
	h.Stop()

	r, ok := h.o.records["1"]
	if !ok {
		t.Fatal("the parked record is gone")
	}
	if r.Attempt != 2 {
		t.Fatalf("attempt = %d, want the second dispatch parked", r.Attempt)
	}
	if r.lastOutcome != "" {
		t.Errorf("previous outcome = %q, want none for an attempt whose agent never started", r.lastOutcome)
	}
	if r.previousAttempt != "" {
		t.Errorf("previous-attempt account was fabricated for the credential park:\n%s", r.previousAttempt)
	}
}

// A transient failure minting the publish credential is refused **before the
// launch**, so no `launch_error` is recorded and no agent runs; an unknown or
// permanent one parks through the same new edge.
func TestStartCredentialFailureRoutesByClass(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		want   State
		reason core.FailureReason
	}{
		{"transient retries as FailureCredential", credErr(core.CredentialTransient), StateBackoff, core.FailureCredential},
		{"permanent parks", credErr(core.CredentialPermanent), StateNeedsReview, ""},
		{"the zero class parks", unclassified(), StateNeedsReview, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues:    []core.Issue{fake.Issue("1", epoch)},
				failStart: tt.err,
			})
			h.WaitState("1", tt.want)

			// No run: the fake's refusal stands in for the mint that `Start`
			// refuses before launching, and StartCount counts launches rather
			// than calls. The absence that matters for the *adapter* — the agent
			// is never launched after a mint failure — is asserted on the runner
			// seam itself, in the conformance suite.
			if got := h.Runner.StartCount(); got != 0 {
				t.Errorf("%d runs were launched, want none behind a refused credential", got)
			}
			waitFor(t, "the attempt outcome to be recorded", func() bool {
				return len(h.o.Attempts.For("1")) == 1
			})
			got := h.o.Attempts.For("1")[0]
			if got.FailureReason != tt.reason {
				t.Errorf("attempt reason = %q, want %q", got.FailureReason, tt.reason)
			}
			// Nothing ran: mislabelling a credential refused before the launch as
			// a launch that failed would put a non-retryable reason on a
			// retryable failure.
			if got.Ran {
				t.Error("ran = true, but the credential was refused before any agent was launched")
			}
		})
	}
}

// mutableVerifier is a verifier whose answer a test can change mid-run, and
// which counts the checks it was asked for.
//
// The count is the assertion behind "once per poll tick": a retry that rides the
// tick is only distinguishable from a record that stopped being looked at by the
// check actually being re-issued.
type mutableVerifier struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (v *mutableVerifier) Verify(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.err != nil {
		return VerifyResult{}, v.err
	}
	return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
}

func (v *mutableVerifier) setErr(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

func (v *mutableVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// A **transient** credential failure reading publish evidence is retried in
// `verifying`, once per poll tick. The attempt is neither ended nor recorded and
// no verdict is routed until one is final (SPEC §9.7, amendment 13;
// mutations 27, 28).
//
// §9.7's fail-closed rule covers evidence that contradicts or cannot be
// established; a credential that could not be obtained establishes neither, and
// the evidence itself is unchanged on git and the tracker.
func TestATransientVerificationCredentialFailureRetriesInVerifying(t *testing.T) {
	v := &mutableVerifier{err: credErr(core.CredentialTransient)}
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}, verifier: v})
	h.WaitState("1", StateVerifying)

	// The retry rides the poll tick, and the record stays where it is with
	// nothing recorded and nothing routed while it does.
	before := v.Calls()
	h.tickUntil("verification is re-issued on a later tick", func() bool { return v.Calls() > before })
	if got := h.stateOf("1"); got != StateVerifying {
		t.Fatalf("state = %q, want the record still in verifying (path: %v)",
			got, h.o.Transitions.Path("1"))
	}
	if got := len(h.o.Attempts.For("1")); got != 0 {
		t.Errorf("recorded %d attempts while a verdict is still pending, want none", got)
	}

	// Once the credential comes back, the verdict routes as it always would —
	// and the attempt is recorded exactly once, not once per retry.
	v.setErr(nil)
	h.tickUntil("the verdict routes once the credential returns", func() bool {
		return h.stateOf("1") == StateDone || h.stateOf("1") == ""
	})
	waitFor(t, "the finished attempt to be recorded", func() bool {
		return len(h.o.Attempts.For("1")) == 1
	})
}

func TestAPendingPublishApprovalRetriesInVerifying(t *testing.T) {
	v := &mutableVerifier{err: core.ErrPublishApprovalPending}
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}, verifier: v})
	h.WaitState("1", StateVerifying)

	before := v.Calls()
	h.tickUntil("publication is retried while approval is pending", func() bool { return v.Calls() > before })
	if got := h.stateOf("1"); got != StateVerifying {
		t.Fatalf("state = %q, want verifying while approval is pending (path: %v)",
			got, h.o.Transitions.Path("1"))
	}
	if got := len(h.o.Attempts.For("1")); got != 0 {
		t.Errorf("recorded %d attempts while publication approval is pending, want none", got)
	}

	v.setErr(nil)
	h.tickUntil("the approved publication routes", func() bool {
		return h.stateOf("1") == StateDone || h.stateOf("1") == ""
	})
	waitFor(t, "the approved attempt to be recorded", func() bool {
		return len(h.o.Attempts.For("1")) == 1
	})
}

// An unknown or permanent verification credential failure parks, exactly as
// §9.7's fail-closed rule already did.
func TestANonTransientVerificationCredentialFailureParks(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"permanent", credErr(core.CredentialPermanent)},
		{"the zero class", unclassified()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues:   []core.Issue{fake.Issue("1", epoch)},
				verifier: &mutableVerifier{err: tt.err},
			})
			h.WaitState("1", StateNeedsReview)
			waitFor(t, "the attempt outcome to be recorded", func() bool {
				return len(h.o.Attempts.For("1")) == 1
			})
			// Neither a §7.3 reason nor a verdict: the agent did not fail, and
			// nothing was concluded about what it produced.
			got := h.o.Attempts.For("1")[0]
			if got.FailureReason != "" || got.Verdict != VerdictUnknown {
				t.Errorf("got %q/%s, want neither a reason nor a verdict", got.FailureReason, got.Verdict)
			}
		})
	}
}

// A tracker **read** that fails on a credential is an ordinary read failure,
// retried on the next tick, whatever the class says (SPEC §9.8, amendment 14;
// mutation 16).
//
// The loop cannot park what it has not claimed, and a read it stopped retrying
// is a daemon that has stopped noticing the world. Readiness is a startup
// verdict; a blip is not evidence of misconfiguration.
func TestATrackerReadIgnoresTheCredentialClass(t *testing.T) {
	for _, tt := range classCases {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				hang:   true,
				script: startedOnly,
			})
			h.WaitState("1", StateRunning)

			h.Tracker.SetFailGet(tt.err)
			h.Tick()
			if got := h.stateOf("1"); got != StateRunning {
				t.Errorf("state = %q after a failed refresh, want the run left alone", got)
			}

			before := h.Tracker.GetReads()
			h.tickUntil("the refresh is retried", func() bool { return h.Tracker.GetReads() > before })
			if got := h.stateOf("1"); got != StateRunning {
				t.Errorf("state = %q after the retry, want the run still left alone", got)
			}
		})
	}
}

// A tracker **write** owed by a standing claim stays owed across every class
// (SPEC §9.8, amendment 14; mutation 29).
//
// A permanent credential error must not discard it: the claim it protects is
// still standing, and dropping the write leaves assigned-with-no-state-label,
// which §9.10 step 3 never revisits.
func TestAnOwedTrackerWriteStaysOwedAcrossEveryCredentialClass(t *testing.T) {
	for _, tt := range classCases {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				hang:   true,
				script: startedOnly,
				beforeStart: func(tr *fake.Tracker) {
					// ben:claimed still lands — the record cannot reach `running`
					// without it — and ben:running never does, so it stays at the
					// head of the queue with the claim standing behind it.
					tr.FailLabel = func(_ string, label core.StateLabel) error {
						if label == core.StateLabelRunning {
							return tt.err
						}
						return nil
					}
				},
			})
			h.WaitState("1", StateRunning)

			// Several ticks: each one re-drives the queue, and a class that routed
			// here would have discarded the write on one of them.
			for range 5 {
				h.Tick()
			}
			if owed := h.owedAfterStop("1"); len(owed) == 0 {
				t.Error("the owed queue is empty; a credential error must not discard a write " +
					"whose claim is still standing")
			}
		})
	}
}

// A non-transient credential failure is logged at **error** naming the
// authority, wherever it is read — including the routes it does not govern, so
// an operator reads a wrong trust policy off the log instead of inferring it
// from a silent stall (SPEC §9.8, amendment 14; mutation 16).
//
// Both severities are pinned. An error level on a blip is an operator woken for
// weather; a warning on a misconfiguration is the silent stall this exists to
// prevent.
func TestATrackerCredentialFailureLogsAtTheSeverityItsClassEarns(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		level slog.Level
	}{
		{"transient warns", credErr(core.CredentialTransient), slog.LevelWarn},
		{"permanent errors", credErr(core.CredentialPermanent), slog.LevelError},
		{"the zero class errors", unclassified(), slog.LevelError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				hang:   true,
				script: startedOnly,
			})
			h.WaitState("1", StateRunning)

			h.Tracker.SetFailGet(tt.err)
			var got []logRecord
			h.tickUntil("the credential failure is logged", func() bool {
				got = h.Logs.find("refreshing tracked issue: credential failure")
				return len(got) > 0
			})

			if got[0].Level != tt.level {
				t.Errorf("logged at %v, want %v", got[0].Level, tt.level)
			}
			// The authority, never the token: it is what names the misconfigured
			// trust policy, and it is non-secret by construction.
			if got[0].Attrs["authority"] != testAuthority {
				t.Errorf("authority attribute = %q, want %q; the operator has only a silent stall "+
					"to go on without it", got[0].Attrs["authority"], testAuthority)
			}
		})
	}
}

// Claim failures route the same way for every credential class — an adapter
// that proves it never wrote is forgotten and retried on a later poll — but the
// class still earns its tracker-path log severity (SPEC §9.8, amendment 14).
func TestAClaimCredentialFailureLogsAtTheSeverityItsClassEarns(t *testing.T) {
	classes := []struct {
		name  string
		err   error
		level slog.Level
	}{
		{"transient warns", credErr(core.CredentialTransient), slog.LevelWarn},
		{"permanent errors", credErr(core.CredentialPermanent), slog.LevelError},
		{"the zero class errors", unclassified(), slog.LevelError},
	}
	for _, path := range []struct {
		name         string
		claimError   func(error) error
		message      string
		wantReleases int
	}{
		{
			"before assignment",
			func(err error) error { return errors.Join(core.ErrClaimNotAttempted, err) },
			"claim was refused before any assignment; leaving the issue queued",
			0,
		},
		{
			"after possible assignment",
			func(err error) error { return err },
			"claim failed; releasing any assignment it may have left",
			1,
		},
	} {
		for _, tt := range classes {
			t.Run(path.name+"/"+tt.name, func(t *testing.T) {
				h := start(t, harnessOpts{
					issues: []core.Issue{fake.Issue("1", epoch)},
					beforeStart: func(tr *fake.Tracker) {
						tr.SetClaimError(path.claimError(tt.err))
					},
				})
				h.WaitGone("1")

				got := h.Logs.find(path.message)
				if len(got) != 1 {
					t.Fatalf("credential claim logs = %d, want 1", len(got))
				}
				if got[0].Level != tt.level {
					t.Errorf("logged at %v, want %v", got[0].Level, tt.level)
				}
				if got[0].Attrs["authority"] != testAuthority {
					t.Errorf("authority attribute = %q, want %q", got[0].Attrs["authority"], testAuthority)
				}
				if n := h.Tracker.ReleaseCount("1"); n != path.wantReleases {
					t.Errorf("released %d times, want %d; credential severity must not change Claim routing",
						n, path.wantReleases)
				}
			})
		}
	}
}

// The tracker contract includes auxiliary reads that do not drive the ordinary
// reconciliation path. They still read credential class for severity while
// leaving their routing unchanged (SPEC §9.8, amendment 14).
func TestAnApprovalReadCredentialFailureLogsAtTheSeverityItsClassEarns(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		level slog.Level
	}{
		{"transient warns", credErr(core.CredentialTransient), slog.LevelWarn},
		{"permanent errors", credErr(core.CredentialPermanent), slog.LevelError},
		{"the zero class errors", unclassified(), slog.LevelError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				definition: contentDefinition(t),
				issues:     []core.Issue{fake.Issue("1", epoch)},
				beforeStart: func(tr *fake.Tracker) {
					tr.SetFailContentApproval(tt.err)
				},
			})

			var got []logRecord
			h.tickUntil("the approval credential failure is logged", func() bool {
				got = h.Logs.find("reading the approval facts; holding the claim and retrying next tick")
				return len(got) > 0
			})
			if got[0].Level != tt.level {
				t.Errorf("logged at %v, want %v", got[0].Level, tt.level)
			}
			if got[0].Attrs["authority"] != testAuthority {
				t.Errorf("authority attribute = %q, want %q", got[0].Attrs["authority"], testAuthority)
			}
			if state := h.stateOf("1"); state != StateQueued {
				t.Errorf("state = %q, want the unresolved read to retain the queued claim", state)
			}
			if n := h.Tracker.ReleaseCount("1"); n != 0 {
				t.Errorf("released %d times; severity must not route an unresolved read", n)
			}
			h.Tracker.SetFailContentApproval(nil)
		})
	}
}

// A held release is a tracker write outside a run record's ordinary owed queue.
// Its failure keeps the release owed for every class, but a transient issuer
// failure warns while a non-transient one errors and names the authority.
func TestAHeldReleaseCredentialFailureLogsAtTheSeverityItsClassEarns(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		level slog.Level
	}{
		{"transient warns", credErr(core.CredentialTransient), slog.LevelWarn},
		{"permanent errors", credErr(core.CredentialPermanent), slog.LevelError},
		{"the zero class errors", unclassified(), slog.LevelError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := doneHarness(t, 1)
			h.Tracker.SetFailRelease(tt.err)
			h.Tracker.Mutate("1", func(issue *core.Issue) { issue.State = "closed" })
			h.Tick()

			var got []logRecord
			waitFor(t, "the held-release credential failure is logged", func() bool {
				got = h.Logs.find("releasing the retained claim; retrying next tick")
				return len(got) > 0
			})
			if got[0].Level != tt.level {
				t.Errorf("logged at %v, want %v", got[0].Level, tt.level)
			}
			if got[0].Attrs["authority"] != testAuthority {
				t.Errorf("authority attribute = %q, want %q", got[0].Attrs["authority"], testAuthority)
			}
			if got := h.o.HeldCount(); got != 1 {
				t.Errorf("held count = %d, want the failed release to stay owed", got)
			}

			h.Tracker.SetFailRelease(nil)
			h.Tick()
			waitFor(t, "the held release to recover", func() bool { return h.o.HeldCount() == 0 })
		})
	}
}
