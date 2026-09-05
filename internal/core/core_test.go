package core

import (
	"context"
	"testing"
)

type trackerContractProbe struct {
	TrackerAdapter
	ready  bool
	branch string
}

func (p *trackerContractProbe) Ready(_ context.Context) error {
	p.ready = true
	return nil
}

func (p *trackerContractProbe) FindPR(_ context.Context, _ Issue, branch string) (*PR, error) {
	p.branch = branch
	return nil, nil
}

func TestTrackerAdapterReadinessAndBranchContract(t *testing.T) {
	probe := &trackerContractProbe{}
	var adapter TrackerAdapter = probe
	if err := adapter.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.FindPR(context.Background(), Issue{Identifier: "7"}, "ben/7"); err != nil {
		t.Fatal(err)
	}
	if !probe.ready || probe.branch != "ben/7" {
		t.Fatalf("contract arguments not delivered: ready=%v branch=%q", probe.ready, probe.branch)
	}
}

type trackerKindProbe struct {
	cfg  TrackerConfig
	opts TrackerOptions
}

func (p *trackerKindProbe) Structural(cfg TrackerConfig) error { p.cfg = cfg; return nil }

func (p *trackerKindProbe) New(opts TrackerOptions) (TrackerAdapter, error) {
	p.opts = opts
	return &trackerContractProbe{}, nil
}

func (p *trackerKindProbe) CredentialRefs(map[string]any) CredentialRefs {
	return CredentialRefs{Fields: [][]string{{"token"}}}
}

func (p *trackerKindProbe) SensitiveFields(map[string]any) [][]string {
	return [][]string{{"token"}}
}

// Structural receives the opaque provider block and the core-owned fields in
// one value (SPEC §5.7): validating a new block against a previous reload's
// core fields would be a silent hot-reload bug.
func TestTrackerKindStructuralTakesTheWholeConfig(t *testing.T) {
	probe := &trackerKindProbe{}
	var kind TrackerKind = probe
	cfg := TrackerConfig{
		Provider:       map[string]any{"repo": "acme/widgets"},
		RequiredLabels: []string{"ben-queue"},
		WorkflowKey:    "ben-1a2b3c4d",
	}
	if err := kind.Structural(cfg); err != nil {
		t.Fatal(err)
	}
	if probe.cfg.Provider["repo"] != "acme/widgets" || len(probe.cfg.RequiredLabels) != 1 || probe.cfg.WorkflowKey != "ben-1a2b3c4d" {
		t.Fatalf("Structural did not receive the whole config: %+v", probe.cfg)
	}
	// New takes the *compiled* options, not the file as written: the credential
	// is a source by then, and the promoted keys are fields (SPEC §8,
	// amendment 9).
	opts := TrackerOptions{
		Provider:       map[string]any{"repo": "acme/widgets"},
		RequiredLabels: []string{"ben-queue"},
		WorkflowKey:    "ben-1a2b3c4d",
		ClaimAssignee:  "ben-bot",
		Credential:     staticProbeSource{},
	}
	if _, err := kind.New(opts); err != nil {
		t.Fatal(err)
	}
	if probe.opts.ClaimAssignee != "ben-bot" || probe.opts.Credential == nil {
		t.Fatalf("New did not receive the compiled options: %+v", probe.opts)
	}
	if _, present := probe.opts.Provider["token"]; present {
		t.Error("the construction block still carries a credential")
	}
}

// staticProbeSource is the smallest thing satisfying the credential seam: what
// this file asserts is the *shape* of the construction boundary, not any
// source's behaviour.
type staticProbeSource struct{}

func (staticProbeSource) Fetch(context.Context, Purpose) (Token, error) {
	return Token{Value: "probe"}, nil
}

func (staticProbeSource) Descriptor() SourceDescriptor {
	return SourceDescriptor{Kind: "static", Authority: "env:PROBE", BindingKey: "env:PROBE"}
}

// The static retryable verdict table (SPEC §7.3) — the orchestrator's whole
// retry policy hangs on these, so the mapping is pinned here.
func TestFailureReasonRetryable(t *testing.T) {
	verdicts := map[FailureReason]bool{
		FailureCrashed:        true,
		FailureStalled:        true,
		FailureTimeout:        true,
		FailureRateLimited:    true,
		FailureAuth:           false,
		FailureLaunchError:    false,
		FailureKilled:         false,
		FailureBudgetExceeded: false,
		// The harness's own verdict on a line past the scanner ceiling (#235):
		// not transient, so a retry would reproduce it.
		FailureOutputOverflow: false,
		// `credential` names a *transient* credential failure specifically
		// (SPEC §7.3, amendment 8), which is what makes it retryable. An unknown
		// or permanent one is not a run failure at all — it parks — so it never
		// arrives with a §7.3 reason attached.
		FailureCredential: true,
	}
	for reason, want := range verdicts {
		if got := reason.Retryable(); got != want {
			t.Errorf("%s.Retryable() = %v, want %v", reason, got, want)
		}
	}
	// Unknown reasons fail closed: not retryable.
	if FailureReason("mystery").Retryable() {
		t.Error("unknown reasons must not be retryable")
	}
}

// SPEC §9.8: an unconfirmed termination retains the claim, so the value nobody
// stated has to be the unconfirmed one. Confirmed-as-zero would make workspace
// reuse safe by the caller's diligence rather than by the type — and the shape
// that actually reaches production is not a variable somebody forgot to assign
// but a struct field somebody forgot to fill (orchestrator's signal carries one),
// which is why that shape is checked here too.
func TestTerminationZeroValueIsUnconfirmed(t *testing.T) {
	var declared Termination
	if declared != TerminationUnconfirmed {
		t.Errorf("zero Termination = %v, want unconfirmed", declared)
	}
	var carrier struct{ Termination Termination }
	if carrier.Termination != TerminationUnconfirmed {
		t.Errorf("unfilled struct field = %v, want unconfirmed", carrier.Termination)
	}
	if TerminationConfirmed == TerminationUnconfirmed {
		t.Fatal("the two terminations must be distinguishable")
	}

	for term, want := range map[Termination]string{
		TerminationUnconfirmed: "unconfirmed",
		TerminationConfirmed:   "confirmed",
		Termination(7):         "Termination(7)",
	} {
		if got := term.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestClaimBaseStateIsClosedAndZeroNonAuthorizing(t *testing.T) {
	states := []struct {
		state ClaimBaseState
		name  string
	}{
		{ClaimBaseUnknown, "unknown"},
		{ClaimBaseAbsent, "absent"},
		{ClaimBasePending, "pending"},
		{ClaimBasePinned, "pinned"},
	}
	// Independent numeric boundary: a declaration-driven table alone would not
	// notice one of the closed states being deleted from both production and test.
	if ClaimBaseUnknown != 0 || ClaimBasePinned != 3 || len(states) != 4 {
		t.Fatalf("claim-base state boundary changed: unknown=%d pinned=%d count=%d",
			ClaimBaseUnknown, ClaimBasePinned, len(states))
	}
	var zero ClaimBaseState
	if zero != ClaimBaseUnknown || zero == ClaimBaseAbsent || zero == ClaimBasePending || zero == ClaimBasePinned {
		t.Fatalf("zero ClaimBaseState = %v; it must authorize no durable reading", zero)
	}
	for _, tt := range states {
		if got := tt.state.String(); got != tt.name {
			t.Errorf("%d.String() = %q, want %q", tt.state, got, tt.name)
		}
	}
	if got := ClaimBaseState(9).String(); got != "ClaimBaseState(9)" {
		t.Errorf("unknown state String() = %q", got)
	}
}
