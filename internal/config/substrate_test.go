package config

import (
	"errors"
	"strings"
	"testing"
)

// The strict v2 substrate section (#194). Every case below is a *load* refusal:
// a substrate is authored once and hits every run, which is the same reasoning
// §7.6's `BEN_` reservation and §10.2's split check are refused at load for.

// validAirlock is a workflow whose attempts run on a v2 backend, with the
// credential named indirectly — the only spelling this section accepts.
const validAirlock = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $TRACKER_PAT
    claim_assignee: ben-bot
  required_labels: ["ben"]
agent:
  kind: claude-code
credential_sources:
  airlock:
    kind: static
    value: $AIRLOCK_TOKEN
substrate:
  kind: airlock
  airlock:
    base_url: https://airlock.internal
    profile: ben-agent
    auth_source: airlock
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`

func loadSubstrate(t *testing.T, content string) *WorkflowDefinition {
	t.Helper()
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return def
}

func refuseSubstrate(t *testing.T, content, wantField string) error {
	t.Helper()
	_, err := Load(writeWorkflow(t, content))
	if err == nil {
		t.Fatal("Load accepted an invalid substrate")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("got %v, want a *ValidationError", err)
	}
	if verr.Field != wantField {
		t.Fatalf("refused %q, want %q (%v)", verr.Field, wantField, err)
	}
	return err
}

func TestAirlockSubstrateResolvesItsDefaults(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	def := loadSubstrate(t, validAirlock)

	sub := def.Config.Substrate
	if !sub.Remote() || sub.Kind != SubstrateKindAirlock {
		t.Fatalf("substrate = %+v", sub)
	}
	a := sub.Airlock
	want := AirlockConfig{
		BaseURL: "https://airlock.internal", Profile: "ben-agent", AuthSource: "airlock",
		RequestTimeoutMS: DefaultAirlockRequestTimeoutMS,
		PollTimeoutMS:    DefaultAirlockPollTimeoutMS,
		PollWaitMS:       DefaultAirlockPollWaitMS,
		SettleTimeoutMS:  DefaultAirlockSettleTimeoutMS,
		MaxRetries:       DefaultAirlockMaxRetries,
		OnSuccess:        DefaultAirlockOnSuccess,
		OnFailure:        DefaultAirlockOnFailure,
		OnRevoked:        DefaultAirlockOnRevoked,
		OnShutdown:       DefaultAirlockOnShutdown,
	}
	if a != want {
		t.Fatalf("airlock = %+v\nwant     %+v", a, want)
	}
	// Every default is visible as a default, not as something somebody wrote.
	for _, path := range []string{
		"substrate.airlock.request_timeout_ms", "substrate.airlock.on_success",
		"substrate.airlock.idle_suspend_ms",
	} {
		if got := def.Provenance[path].Source; got != SourceDefault {
			t.Fatalf("%s provenance is %q, want %q", path, got, SourceDefault)
		}
	}
	if got := def.Provenance["substrate.airlock.base_url"].Source; got != SourceFile {
		t.Fatalf("base_url provenance is %q, want %q", got, SourceFile)
	}
	// And the substrate credential is resolved, not merely referenced: the
	// authority comparison below depends on it.
	if !def.Config.Credentials.Substrate.Configured() {
		t.Fatal("the substrate credential was not resolved")
	}
	if def.Config.Credentials.Substrate.Name != "airlock" {
		t.Fatalf("the substrate credential names %q", def.Config.Credentials.Substrate.Name)
	}
}

func TestAirlockSubstrateResolvesAProjectedOIDCPrincipal(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	content := strings.Replace(validAirlock, `  airlock:
    kind: static
    value: $AIRLOCK_TOKEN
`, `  airlock:
    kind: projected_oidc
    issuer: https://oidc.eks.example/id/cluster
    audience: airlock-api
    tenant_claim: sub
    tenant_id: system:serviceaccount:ben:ben
    subject: system:serviceaccount:ben:ben
    token_path: /var/run/secrets/airlock/token
    min_ttl_ms: 300000
`, 1)
	def := loadSubstrate(t, content)
	credential := def.Config.Credentials.Substrate
	if credential.Kind != "projected_oidc" || credential.Name != "airlock" || !credential.Bounded() {
		t.Fatalf("substrate credential = %+v", credential)
	}
	if credential.PrincipalKey != "airlock-owner:system:serviceaccount:ben:ben#system:serviceaccount:ben:ben" {
		t.Errorf("PrincipalKey = %q, want Airlock's tenant/subject ownership tuple", credential.PrincipalKey)
	}

	effective := EffectiveText(def)
	for _, want := range []string{
		"kind: projected_oidc", "issuer: https://oidc.eks.example/id/cluster",
		"audience: airlock-api", "tenant_claim: sub",
		"tenant_id: system:serviceaccount:ben:ben", "subject: system:serviceaccount:ben:ben",
		"token_path: /var/run/secrets/airlock/token", "min_ttl_ms: 300000",
	} {
		if !strings.Contains(effective, want) {
			t.Errorf("EffectiveText is missing %q:\n%s", want, effective)
		}
	}
}

// The mixed-configuration refusal, in both directions. A block nobody reads is a
// setting an operator believes is in effect; a missing block is three fields
// with no defaults.
func TestSubstrateRefusesMixedLocalAndRemoteConfiguration(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")

	localWithBackend := strings.Replace(validAirlock, "  kind: airlock\n", "  kind: local\n", 1)
	err := refuseSubstrate(t, localWithBackend, "substrate.airlock")
	if !strings.Contains(err.Error(), "local") {
		t.Fatalf("the refusal does not name the kind that made it one: %v", err)
	}

	remoteWithoutBackend := `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $TRACKER_PAT
    claim_assignee: ben-bot
  required_labels: ["ben"]
agent:
  kind: claude-code
substrate:
  kind: airlock
deployment:
  mode: attended
---
Do the work.
`
	refuseSubstrate(t, remoteWithoutBackend, "substrate.airlock")
}

// A written-but-null key is indistinguishable from an omitted one by the strict
// pass, and the distinction is exactly what the refusal above rests on.
func TestSubstrateRefusesExplicitNulls(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	for _, tc := range []struct {
		name    string
		content string
		field   string
	}{
		{
			"null substrate",
			strings.Replace(validMinimal, "agent:\n", "substrate:\nagent:\n", 1),
			"substrate",
		},
		{
			"null backend block",
			strings.Replace(validAirlock,
				"  airlock:\n    base_url: https://airlock.internal\n    profile: ben-agent\n    auth_source: airlock\n",
				"  airlock:\n", 1),
			"substrate.airlock",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuseSubstrate(t, tc.content, tc.field)
		})
	}
}

func TestSubstrateRefusesAnUnknownKind(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	content := strings.Replace(validMinimal, "agent:\n", "substrate:\n  kind: kubernetes\nagent:\n", 1)
	err := refuseSubstrate(t, content, "substrate.kind")
	for _, kind := range SubstrateKinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Fatalf("the refusal does not list %q: %v", kind, err)
		}
	}
}

// The section is closed, not opaque: a typo in an endpoint or a retention
// policy is not something to discover at the first dispatched claim.
func TestSubstrateRefusesAnUnknownKey(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	content := strings.Replace(validAirlock, "    profile: ben-agent\n", "    profile: ben-agent\n    profil: typo\n", 1)
	if _, err := Load(writeWorkflow(t, content)); err == nil || !strings.Contains(err.Error(), "profil") {
		t.Fatalf("Load with an unknown backend key = %v, want a strict refusal naming it", err)
	}
}

func TestAirlockValueRules(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	for _, tc := range []struct {
		name  string
		from  string
		to    string
		field string
	}{
		{"no endpoint", "    base_url: https://airlock.internal\n", "", "substrate.airlock.base_url"},
		{"plain http", "base_url: https://airlock.internal", "base_url: http://airlock.internal", "substrate.airlock.base_url"},
		{"credentials in the endpoint", "base_url: https://airlock.internal", "base_url: https://user:tok@airlock.internal", "substrate.airlock.base_url"},
		{"query in the endpoint", "base_url: https://airlock.internal", "base_url: https://airlock.internal?token=tok", "substrate.airlock.base_url"},
		{"fragment in the endpoint", "base_url: https://airlock.internal", "base_url: https://airlock.internal#tok", "substrate.airlock.base_url"},
		{"no host", "base_url: https://airlock.internal", "base_url: https:///v2", "substrate.airlock.base_url"},
		{"no profile", "    profile: ben-agent\n", "", "substrate.airlock.profile"},
		{"no auth source", "    auth_source: airlock\n", "", "substrate.airlock.auth_source"},
		{"auth source names nothing", "auth_source: airlock", "auth_source: nowhere", "substrate.airlock.auth_source"},
		{"non-positive timeout", "profile: ben-agent", "profile: ben-agent\n    request_timeout_ms: 0", "substrate.airlock.request_timeout_ms"},
		{"non-positive retries", "profile: ben-agent", "profile: ben-agent\n    max_retries: 0", "substrate.airlock.max_retries"},
		{"long poll outlives its request bound", "profile: ben-agent", "profile: ben-agent\n    poll_wait_ms: 70000", "substrate.airlock.poll_wait_ms"},
		{"idle window under the backend floor", "profile: ben-agent", "profile: ben-agent\n    idle_suspend_ms: 30000", "substrate.airlock.idle_suspend_ms"},
		{"delete window under the backend floor", "profile: ben-agent", "profile: ben-agent\n    delete_after_idle_ms: 1000", "substrate.airlock.delete_after_idle_ms"},
		{"unknown disposal", "profile: ben-agent", "profile: ben-agent\n    on_failure: archive", "substrate.airlock.on_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(validAirlock, tc.from, tc.to, 1)
			if content == validAirlock {
				t.Fatalf("the fixture does not contain %q", tc.from)
			}
			refuseSubstrate(t, content, tc.field)
		})
	}
}

// A rejected endpoint is the one field here that could carry a secret, and the
// refusal for that case is precisely the one an operator would paste into an
// issue.
func TestARejectedEndpointIsNeverEchoed(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	for _, endpoint := range []string{
		"https://user:hunter2@airlock.internal",
		"https://airlock.internal?token=hunter2",
		"https://airlock.internal#hunter2",
	} {
		content := strings.Replace(validAirlock,
			"base_url: https://airlock.internal", "base_url: "+endpoint, 1)
		_, err := Load(writeWorkflow(t, content))
		if err == nil {
			t.Fatalf("Load accepted rejected endpoint shape %q", endpoint)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the refusal echoed the credential: %v", err)
		}
	}
}

// The §10.2 rule applied to a wider blast radius: a token that can create and
// destroy execution environments must not also be one scoped to the forge.
func TestSubstrateCredentialMustNotBeATrackerOrPublishCredential(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		consumer string
	}{
		{
			name: "shared with the tracker",
			content: strings.Replace(validAirlock,
				"    token: $TRACKER_PAT\n", "    credential_source: airlock\n", 1),
			consumer: "tracker",
		},
		{
			name: "shared with the publisher",
			content: strings.Replace(validAirlock,
				"substrate:\n", "publish:\n  kind: source\n  env: GH_TOKEN\n  source: airlock\nsubstrate:\n", 1),
			consumer: "publish",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRACKER_PAT", "tracker-secret")
			_, err := Load(writeWorkflow(t, tc.content))
			if err == nil {
				t.Fatal("Load accepted a shared substrate credential")
			}
			if !errors.Is(err, ErrCredentialAuthorityShared) {
				t.Fatalf("got %v, want the shared-authority refusal", err)
			}
			var shared *SubstrateCredentialError
			if !errors.As(err, &shared) {
				t.Fatalf("got %v, want a *SubstrateCredentialError", err)
			}
			if shared.Consumer != tc.consumer {
				t.Fatalf("the refusal names consumer %q, want %q", shared.Consumer, tc.consumer)
			}
			if strings.Contains(err.Error(), "tracker-secret") {
				t.Fatalf("the refusal echoed a credential: %v", err)
			}
		})
	}
}

// A local substrate resolves no backend credential at all: there is nothing to
// authenticate to.
func TestALocalSubstrateResolvesNoBackendCredential(t *testing.T) {
	def := loadSubstrate(t, validMinimal)
	if def.Config.Credentials.Substrate.Configured() {
		t.Fatalf("a local substrate resolved a backend credential: %+v", def.Config.Credentials.Substrate)
	}
}

// The whole section renders, and none of it is redacted: every field is a name,
// a keyword or a number, and hiding an endpoint would conceal exactly what an
// operator needs when the backend is unreachable.
func TestEffectiveOutputRendersTheBackendInFull(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	def := loadSubstrate(t, validAirlock)
	text := EffectiveText(def)
	for _, want := range []string{
		"kind: airlock", "base_url: https://airlock.internal", "profile: ben-agent",
		"auth_source: airlock", "on_success: " + DefaultAirlockOnSuccess,
		"idle_suspend_ms: (profile default)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("EffectiveText is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, Redacted+"\n") && strings.Contains(text, "base_url: "+Redacted) {
		t.Fatalf("EffectiveText redacted the endpoint:\n%s", text)
	}
}

func TestSubstrateKindsAndDisposalsAreClosedAndCopied(t *testing.T) {
	kinds := SubstrateKinds()
	kinds[0] = "mutated"
	if SubstrateKinds()[0] != SubstrateKindLocal {
		t.Fatal("SubstrateKinds returned the package-level array rather than a copy")
	}
	disposals := Disposals()
	disposals[0] = "mutated"
	if Disposals()[0] != DisposalRetain {
		t.Fatal("Disposals returned the package-level array rather than a copy")
	}
	if len(SubstrateKinds()) != 2 || len(Disposals()) != 3 {
		t.Fatal("the closed sets changed size")
	}
}
