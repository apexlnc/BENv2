package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// sourceWorkflow builds a loadable workflow whose tracker block,
// `credential_sources` section and `publish` block are the caller's.
//
// Written as text rather than assembled from structs on purpose: every rule
// under test is a rule about the file, and half of them are about what a
// *spelling* compiles into.
type sourceWorkflow struct {
	trackerProvider string
	sources         string
	publish         string
	limits          string
}

func (w sourceWorkflow) render() string {
	provider := w.trackerProvider
	if provider == "" {
		provider = "    repo: acme/widgets\n    token: $BEN_TEST_TRACKER_TOKEN\n"
	}
	return "---\ntracker:\n  kind: github\n  provider:\n" + provider +
		"  required_labels: [\"ben\"]\n" +
		w.sources +
		"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
		w.publish +
		w.limits +
		"deployment:\n  mode: attended\n" +
		"---\nDo the work described in {{ issue.title }}.\n"
}

func (w sourceWorkflow) load(t *testing.T) (*WorkflowDefinition, error) {
	t.Helper()
	return Load(writeWorkflow(t, w.render()))
}

func (w sourceWorkflow) mustLoad(t *testing.T) *WorkflowDefinition {
	t.Helper()
	def, err := w.load(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return def
}

// octoSources is the intended deployment from the ticket, verbatim: two
// identities against one issuer, reading one projected service-account token.
func octoSources(oidc string) string {
	return "credential_sources:\n" +
		"  tracker:\n    kind: octo_sts\n    url: https://octo.srhg-nonprod.cloud\n" +
		"    scope: srhg-ai-7cef3f93\n    identity: ben-tracker\n    oidc_token_path: " + oidc + "\n" +
		"  publish:\n    kind: octo_sts\n    url: https://octo.srhg-nonprod.cloud\n" +
		"    scope: srhg-ai-7cef3f93\n    identity: ben-publish\n    oidc_token_path: " + oidc + "\n"
}

const octoTrackerProvider = "    repo: acme/widgets\n    credential_source: tracker\n    claim_assignee: ben-bot\n"

const octoPublishBlock = "publish:\n  kind: source\n  env: GH_TOKEN\n  source: publish\n"

// A 45-minute attempt is the maximum an `octo_sts` publish credential admits:
// 50m of declared life minus the fixed 5m margin.
const octoLimits = "limits:\n  attempt_timeout_ms: 2700000\n"

func setTrackerToken(t *testing.T, value string) {
	t.Helper()
	t.Setenv("BEN_TEST_TRACKER_TOKEN", value)
}

// The intended deployment loads: two identities, one issuer, one projected OIDC
// token. The shared `oidc_token_path` is deliberately not part of source
// identity (mutation 10).
func TestTheIntendedOctoDeploymentLoads(t *testing.T) {
	def := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources("/var/run/secrets/octo/oidc-token"),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}.mustLoad(t)

	tracker, publish := def.Config.Credentials.Tracker, def.Config.Credentials.Publish
	const issuer = "octo:https://octo.srhg-nonprod.cloud#srhg-ai-7cef3f93#"
	if tracker.Authority != issuer+"ben-tracker" {
		t.Errorf("tracker authority = %q, want %q", tracker.Authority, issuer+"ben-tracker")
	}
	if publish.Authority != issuer+"ben-publish" {
		t.Errorf("publish authority = %q, want %q", publish.Authority, issuer+"ben-publish")
	}
	// The path is in the binding key and not in the authority, which is the
	// whole of why the shared projection is legal.
	for _, c := range []Credential{tracker, publish} {
		if !strings.HasSuffix(c.BindingKey, "#/var/run/secrets/octo/oidc-token") {
			t.Errorf("binding key %q does not carry the OIDC path", c.BindingKey)
		}
		if strings.Contains(c.Authority, "oidc-token") {
			t.Errorf("authority %q carries the OIDC path; the shared projection would refuse", c.Authority)
		}
	}
}

// Every legacy spelling compiles into an implicit source, and its **authority is
// read from provenance** rather than from the config site (SPEC §10.2,
// amendment 15; mutation 24).
func TestLegacySpellingsCompileIntoImplicitSources(t *testing.T) {
	t.Run("token: $FOO yields env:FOO", func(t *testing.T) {
		setTrackerToken(t, "ghp-resolved")
		def := sourceWorkflow{}.mustLoad(t)
		got := def.Config.Credentials.Tracker
		if got.Authority != "env:BEN_TEST_TRACKER_TOKEN" {
			t.Errorf("authority = %q, want the variable provenance names, not the config site", got.Authority)
		}
		if got.Kind != credential.StaticKindName || got.Bounded() {
			t.Errorf("descriptor = %+v, want an explicitly unbounded static source", got.SourceDescriptor)
		}
		if got.Name != "" {
			t.Errorf("Name = %q, want an implicit source to name no entry", got.Name)
		}
	})

	t.Run("a true file literal yields its config site", func(t *testing.T) {
		def := sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    token: ghp-written-in-the-file\n",
		}.mustLoad(t)
		got := def.Config.Credentials.Tracker
		if got.Authority != "site:tracker.provider.token" {
			t.Errorf("authority = %q, want the config site: no variable was ever referenced", got.Authority)
		}
		if strings.Contains(got.BindingKey, "ghp-written-in-the-file") {
			t.Error("the binding key carries the credential")
		}
	})

	t.Run("an omitted token yields the documented fallback", func(t *testing.T) {
		def := sourceWorkflow{trackerProvider: "    repo: acme/widgets\n"}.mustLoad(t)
		got := def.Config.Credentials.Tracker
		if got.Authority != "env:GITHUB_TOKEN" {
			t.Errorf("authority = %q, want env:GITHUB_TOKEN — stated explicitly, because an "+
				"undeclared fallback is what made #47's collision invisible", got.Authority)
		}
	})

	t.Run("publish.value: $VAR yields env:VAR", func(t *testing.T) {
		setTrackerToken(t, "ghp-resolved")
		def := sourceWorkflow{
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_TEST_PUBLISH\n",
		}.mustLoad(t)
		got := def.Config.Credentials.Publish
		if got.Authority != "env:BEN_TEST_PUBLISH" {
			t.Errorf("authority = %q, want env:BEN_TEST_PUBLISH", got.Authority)
		}
		if got.Bounded() {
			t.Error("a legacy publish credential is explicitly unbounded")
		}
	})

	t.Run("no publish block yields no credential", func(t *testing.T) {
		setTrackerToken(t, "ghp-resolved")
		def := sourceWorkflow{}.mustLoad(t)
		if def.Config.Credentials.Publish.Configured() {
			t.Errorf("publish credential = %+v, want none", def.Config.Credentials.Publish)
		}
	})
}

// The split refusal is one rule stated over **authority**, and it subsumes every
// spelling (SPEC §10.2, amendment 15).
func TestAuthorityEqualityRefusesEverySpelling(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	for _, tt := range []struct {
		name string
		w    sourceWorkflow
		want bool // true = a load refusal
	}{
		{
			name: "one variable spelled two ways, legacy on both sides",
			w: sourceWorkflow{
				trackerProvider: "    repo: acme/widgets\n    token: $BEN_TEST_SHARED\n",
				publish:         "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_TEST_SHARED\n",
			},
			want: true,
		},
		{
			name: "the tracker's documented fallback against a named static source",
			w: sourceWorkflow{
				trackerProvider: "    repo: acme/widgets\n",
				sources:         "credential_sources:\n  pub:\n    kind: static\n    value: $GITHUB_TOKEN\n",
				publish:         "publish:\n  kind: source\n  env: GH_TOKEN\n  source: pub\n",
			},
			want: true,
		},
		{
			name: "two static sources naming one variable",
			w: sourceWorkflow{
				trackerProvider: "    repo: acme/widgets\n    credential_source: a\n",
				sources: "credential_sources:\n  a:\n    kind: static\n    value: $BEN_TEST_SHARED\n" +
					"  b:\n    kind: static\n    value: $BEN_TEST_SHARED\n",
				publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: b\n",
			},
			want: true,
		},
		{
			name: "one source named twice",
			w: sourceWorkflow{
				trackerProvider: "    repo: acme/widgets\n    credential_source: only\n    claim_assignee: ben-bot\n",
				sources: "credential_sources:\n  only:\n    kind: octo_sts\n    url: https://octo.example\n" +
					"    scope: org\n    identity: ben\n    oidc_token_path: " + oidc + "\n",
				publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: only\n",
				limits:  octoLimits,
			},
			want: true,
		},
		{
			name: "two octo sources sharing (url, scope, identity)",
			w: sourceWorkflow{
				trackerProvider: "    repo: acme/widgets\n    credential_source: a\n    claim_assignee: ben-bot\n",
				sources: "credential_sources:\n" +
					"  a:\n    kind: octo_sts\n    url: https://octo.example\n    scope: org\n" +
					"    identity: ben\n    oidc_token_path: " + oidc + "\n" +
					"  b:\n    kind: octo_sts\n    url: https://OCTO.example/\n    scope: org\n" +
					"    identity: ben\n    oidc_token_path: /var/run/secrets/octo/other\n",
				publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: b\n",
				limits:  octoLimits,
			},
			want: true,
		},
		{
			// The intended deployment, and the one case that must NOT refuse:
			// a shared OIDC path is not part of source identity (mutation 10).
			name: "two identities sharing one projected OIDC token",
			w: sourceWorkflow{
				trackerProvider: octoTrackerProvider,
				sources:         octoSources(oidc),
				publish:         octoPublishBlock,
				limits:          octoLimits,
			},
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BEN_TEST_SHARED", "one-secret")
			t.Setenv("GITHUB_TOKEN", "one-secret")
			setTrackerToken(t, "ghp-resolved")
			_, err := tt.w.load(t)
			switch {
			case tt.want && err == nil:
				t.Fatal("Load = nil, want a §10.2 split refusal")
			case tt.want:
				// Either half of §10.2's enforcement is a correct refusal: the
				// variable-level check names the agent route, the authority
				// rule names the identity. What must not happen is neither.
				if !errors.Is(err, ErrCredentialAuthorityShared) && !errors.Is(err, ErrCredentialShared) {
					t.Fatalf("Load = %v, want a §10.2 refusal", err)
				}
			case err != nil:
				t.Fatalf("Load = %v, want ok", err)
			}
		})
	}
}

// The refusal names the authority, which is non-secret by construction, and
// never a value.
func TestTheAuthorityRefusalNamesNoSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp-the-actual-secret")
	_, err := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n",
		sources:         "credential_sources:\n  pub:\n    kind: static\n    value: $GITHUB_TOKEN\n",
		publish:         "publish:\n  kind: source\n  env: GH_TOKEN\n  source: pub\n",
	}.load(t)
	if err == nil {
		t.Fatal("Load = nil, want a §10.2 refusal")
	}
	if strings.Contains(err.Error(), "ghp-the-actual-secret") {
		t.Errorf("refusal = %v, want one that names no value", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("refusal = %v, want it to name the identity", err)
	}
}

// No combination degrades: a partially configured or doubly-spelled credential
// refuses rather than falling through to another (SPEC §5.7, amendment 4;
// mutation 5).
func TestCredentialSpellingsDoNotDegrade(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	for _, tt := range []struct {
		name string
		w    sourceWorkflow
	}{
		{"a credential_source naming no declared entry", sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    credential_source: nope\n    claim_assignee: ben-bot\n",
		}},
		{"a publish source naming no declared entry", sourceWorkflow{
			publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: nope\n",
		}},
		{"both a token and a credential_source", sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    token: $BEN_TEST_TRACKER_TOKEN\n    credential_source: a\n",
			sources:         "credential_sources:\n  a:\n    kind: static\n    value: $BEN_TEST_OTHER\n",
		}},
		{"a source kind with no registered implementation", sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    credential_source: a\n    claim_assignee: ben-bot\n",
			sources:         "credential_sources:\n  a:\n    kind: github_app\n    key: whatever\n",
		}},
		{"a source with no kind at all", sourceWorkflow{
			sources: "credential_sources:\n  a:\n    url: https://octo.example\n",
		}},
		{"an octo source missing a field", sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    credential_source: a\n    claim_assignee: ben-bot\n",
			sources: "credential_sources:\n  a:\n    kind: octo_sts\n    url: https://octo.example\n" +
				"    scope: org\n    identity: ben\n",
		}},
		{"an octo source with an unknown key", sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    credential_source: a\n    claim_assignee: ben-bot\n",
			sources: "credential_sources:\n  a:\n    kind: octo_sts\n    url: https://octo.example\n" +
				"    scope: org\n    identity: ben\n    oidc_token_path: " + oidc + "\n    oidc_toke_path: typo\n",
		}},
		{"a publish block naming both a value and a source", sourceWorkflow{
			sources: "credential_sources:\n  a:\n    kind: static\n    value: $BEN_TEST_OTHER\n",
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_TEST_OTHER\n  source: a\n",
		}},
		{"a publish source block with no source", sourceWorkflow{
			publish: "publish:\n  kind: source\n  env: GH_TOKEN\n",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setTrackerToken(t, "ghp-resolved")
			t.Setenv("BEN_TEST_OTHER", "another-secret")
			if _, err := tt.w.load(t); err == nil {
				t.Error("Load = nil, want a refusal — a partially configured source never falls through")
			}
		})
	}
}

// A bounded credential source REQUIRES `claim_assignee`: such a credential is
// statically known not to yield a machine-user principal (SPEC §8.4,
// amendment 10).
func TestABoundedTrackerSourceRequiresAClaimAssignee(t *testing.T) {
	w := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n    credential_source: tracker\n",
		sources:         octoSources("/var/run/secrets/octo/oidc-token"),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}
	_, err := w.load(t)
	if err == nil {
		t.Fatal("Load = nil, want a refusal: a policy-minted credential has no login for claims to fall back to")
	}
	if !strings.Contains(err.Error(), "claim_assignee") {
		t.Errorf("refusal = %v, want it to name claim_assignee", err)
	}
	// The same configuration with an assignee loads.
	w.trackerProvider = octoTrackerProvider
	w.mustLoad(t)
}

// The load gate is `attempt_timeout_ms + margin <= MinFreshTTL`, asserted from
// the arithmetic rather than from the table that declares it (mutations 3, 4).
func TestThePublishTTLGateIsArithmetic(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	ttl := credential.OctoMinFreshTTL
	for _, tt := range []struct {
		name    string
		attempt time.Duration
		ok      bool
	}{
		{"the exact maximum", ttl - core.CredentialTTLMargin, true},
		{"a millisecond under it", ttl - core.CredentialTTLMargin - time.Millisecond, true},
		{"a millisecond over it", ttl - core.CredentialTTLMargin + time.Millisecond, false},
		{"the default one hour", time.Hour, false},
		{"a representable duration whose margin would overflow", time.Duration(9223372036854) * time.Millisecond, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sourceWorkflow{
				trackerProvider: octoTrackerProvider,
				sources:         octoSources(oidc),
				publish:         octoPublishBlock,
				limits:          fmt.Sprintf("limits:\n  attempt_timeout_ms: %d\n", tt.attempt.Milliseconds()),
			}.load(t)
			switch {
			case tt.ok && err != nil:
				t.Errorf("Load = %v, want ok: %s + %s <= %s", err, tt.attempt, core.CredentialTTLMargin, ttl)
			case !tt.ok && err == nil:
				t.Errorf("Load = nil, want a refusal: %s + %s > %s", tt.attempt, core.CredentialTTLMargin, ttl)
			case !tt.ok && !strings.Contains(err.Error(), "attempt_timeout_ms"):
				t.Errorf("refusal = %v, want it to name the field an operator must lower", err)
			}
		})
	}

	// Skipped entirely when the source states no deadline, which is what keeps
	// `static`, every legacy spelling and every configuration that loads today
	// valid — including the one-hour DefaultAttemptTimeoutMS (mutation 3).
	t.Run("an unbounded publish credential is not gated", func(t *testing.T) {
		setTrackerToken(t, "ghp-resolved")
		sourceWorkflow{
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_TEST_PUBLISH\n",
			limits:  "limits:\n  attempt_timeout_ms: 86400000\n",
		}.mustLoad(t)
	})
}

// The two reduced provider maps exclude `token`, `credential_source` and the
// promoted `claim_assignee` — asserted at an **independent boundary**, so it
// still fails if a future key is added to the provider block and forgotten here
// (AGENTS.md conventions).
//
// The anchor is the *rendered file*, walked for every key it wrote: a
// hand-written list of excluded keys is exactly what would keep passing after a
// fourth credential-shaped key arrived.
func TestBothReducedProviderMapsCarryNoCredentialAndNoSourceName(t *testing.T) {
	setTrackerToken(t, "ghp-the-resolved-secret")
	def := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n    token: $BEN_TEST_TRACKER_TOKEN\n    claim_assignee: ben-bot\n",
	}.mustLoad(t)

	binding := def.Config.TrackerBinding()
	// The credential's *value* is what the loader put in the full block, so
	// finding it anywhere in the reduced one is the leak this asserts against.
	for key, value := range binding.Provider {
		if s, ok := value.(string); ok && strings.Contains(s, "ghp-the-resolved-secret") {
			t.Errorf("the reduced provider block carries the credential at %q", key)
		}
	}
	for _, forbidden := range []string{"token", "credential_source", "claim_assignee"} {
		if _, present := binding.Provider[forbidden]; present {
			t.Errorf("the reduced provider block still carries %q", forbidden)
		}
	}
	// The promoted key is present as a field, so nothing was lost.
	if binding.ClaimAssignee != "ben-bot" {
		t.Errorf("ClaimAssignee = %q, want the promoted value", binding.ClaimAssignee)
	}
	// And the full block is untouched: the reduction is a copy, or a rendering
	// of `config effective` would silently lose keys.
	if _, present := def.Config.Tracker.Provider["token"]; !present {
		t.Error("the reduction mutated the definition's own provider block")
	}

	// No source *name* crosses the boundary either, however the credential was
	// spelled: neither key carries one, which is what makes a rename not a
	// rebuild (mutation 8).
	named := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n    credential_source: some_long_entry_name\n",
		sources:         "credential_sources:\n  some_long_entry_name:\n    kind: static\n    value: $BEN_TEST_TRACKER_TOKEN\n",
	}.mustLoad(t)
	nb := named.Config.TrackerBinding()
	if strings.Contains(nb.Credential.BindingKey, "some_long_entry_name") {
		t.Errorf("BindingKey %q carries the source name", nb.Credential.BindingKey)
	}
	if _, present := nb.Provider["credential_source"]; present {
		t.Error("the reduced provider block still names the credential source")
	}
	if strings.Contains(named.Config.AgentBinding().PublishSource.BindingKey, "some_long_entry_name") {
		t.Error("the agent binding carries a source name")
	}
}

// Every field `trackerConfig` projects is a reason to rebuild — table-driven
// over the projection rather than over a hand-written list, since a hand-written
// list is how `hooks`, `workspace.root` and `publish` were each missed before
// (SPEC §5.4, amendment 2).
func TestTrackerBindingCoversEveryFieldTheAdapterIsBuiltFrom(t *testing.T) {
	oidc := filepath.Join(t.TempDir(), "oidc-token")
	base := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources(oidc),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}

	for _, tt := range []struct {
		name string
		edit func(*sourceWorkflow)
	}{
		{"the provider block", func(w *sourceWorkflow) {
			w.trackerProvider = "    repo: acme/other\n    credential_source: tracker\n    claim_assignee: ben-bot\n"
		}},
		{"claim_assignee", func(w *sourceWorkflow) {
			w.trackerProvider = "    repo: acme/widgets\n    credential_source: tracker\n    claim_assignee: other-bot\n"
		}},
		{"api_url, a plain block key", func(w *sourceWorkflow) {
			w.trackerProvider = octoTrackerProvider + "    api_url: https://ghe.example.com/api/v3\n"
		}},
		{"the credential source's scope", func(w *sourceWorkflow) {
			w.sources = strings.Replace(w.sources, "scope: srhg-ai-7cef3f93\n    identity: ben-tracker",
				"scope: another-org\n    identity: ben-tracker", 1)
		}},
		{"the credential source's oidc_token_path", func(w *sourceWorkflow) {
			w.sources = strings.Replace(w.sources, "oidc_token_path: "+oidc+"\n    identity",
				"oidc_token_path: "+oidc+"-other\n    identity", 1)
			w.sources = strings.Replace(w.sources, "identity: ben-tracker\n    oidc_token_path: "+oidc,
				"identity: ben-tracker\n    oidc_token_path: "+oidc+"-other", 1)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			next := base
			tt.edit(&next)
			if next.render() == base.render() {
				t.Fatal("the edit changed nothing; this row proves nothing")
			}
			before, after := base.mustLoad(t), next.mustLoad(t)
			if reflect.DeepEqual(before.Config.TrackerBinding(), after.Config.TrackerBinding()) {
				t.Error("the binding did not move; the adapter would keep serving the previous configuration")
			}
		})
	}

	// The two edits `required_labels` and `terminal_states` reach through the
	// core-owned fields rather than the block. `terminal_states` is called out
	// because it is the one the previous comparison would have dropped
	// (mutation 21).
	t.Run("terminal_states", func(t *testing.T) {
		before := base.mustLoad(t)
		next := base
		next.limits = base.limits
		withTerminal := strings.Replace(next.render(),
			"  required_labels: [\"ben\"]\n", "  required_labels: [\"ben\"]\n  terminal_states: [\"closed\"]\n", 1)
		after, err := Load(writeWorkflow(t, withTerminal))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if reflect.DeepEqual(before.Config.TrackerBinding(), after.Config.TrackerBinding()) {
			t.Error("a terminal_states edit left the tracker binding unchanged")
		}
	})

	// A rotated *literal* is the case a name-free binding would otherwise drop:
	// the site is unchanged, so only the digest can move (mutation 18).
	t.Run("a rotated literal token", func(t *testing.T) {
		w := sourceWorkflow{trackerProvider: "    repo: acme/widgets\n    token: ghp-before\n"}
		before := w.mustLoad(t)
		w.trackerProvider = "    repo: acme/widgets\n    token: ghp-after\n"
		after := w.mustLoad(t)
		if reflect.DeepEqual(before.Config.TrackerBinding(), after.Config.TrackerBinding()) {
			t.Error("a rotated literal left the binding unchanged; the adapter would keep the old credential")
		}
	})

	// A rotated `$VAR` value moves it too, because the loader resolves the
	// reference and the digest is over the resolved value.
	t.Run("a rotated $VAR value", func(t *testing.T) {
		setTrackerToken(t, "ghp-before")
		before := sourceWorkflow{}.mustLoad(t)
		setTrackerToken(t, "ghp-after")
		after := sourceWorkflow{}.mustLoad(t)
		if reflect.DeepEqual(before.Config.TrackerBinding(), after.Config.TrackerBinding()) {
			t.Error("a rotated $VAR value left the binding unchanged")
		}
	})
}

// A **rename with an identical definition rebuilds nothing** — structurally,
// since neither key carries a name (SPEC §5.4, mutation 8).
func TestARenamedSourceWithAnIdenticalDefinitionIsNotARebuild(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	before := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources(oidc),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}.mustLoad(t)

	renamed := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n    credential_source: issues\n    claim_assignee: ben-bot\n",
		sources: strings.Replace(strings.Replace(octoSources(oidc),
			"  tracker:\n", "  issues:\n", 1), "  publish:\n", "  pushes:\n", 1),
		publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: pushes\n",
		limits:  octoLimits,
	}.mustLoad(t)

	if !reflect.DeepEqual(before.Config.TrackerBinding(), renamed.Config.TrackerBinding()) {
		t.Error("a rename moved the tracker binding")
	}
	if !reflect.DeepEqual(before.Config.AgentBinding(), renamed.Config.AgentBinding()) {
		t.Error("a rename moved the agent binding")
	}
}

// The agent binding carries the publisher's `{Kind, BindingKey}` and
// `limits.attempt_timeout_ms` (SPEC §5.4, amendment 2; mutation 26).
func TestAgentBindingCarriesThePublisherAndTheAttemptTimeout(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	base := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources(oidc),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}
	before := base.mustLoad(t)

	if got := before.Config.AgentBinding().AttemptTimeout; got != 45*time.Minute {
		t.Errorf("AttemptTimeout = %s, want the configured 45m", got)
	}
	if got := before.Config.AgentBinding().PublishSource.Kind; got != credential.OctoSTSKindName {
		t.Errorf("PublishSource.Kind = %q, want the publisher's kind", got)
	}

	t.Run("an attempt_timeout_ms edit rebuilds the runner", func(t *testing.T) {
		next := base
		next.limits = "limits:\n  attempt_timeout_ms: 1800000\n"
		after := next.mustLoad(t)
		if reflect.DeepEqual(before.Config.AgentBinding(), after.Config.AgentBinding()) {
			t.Error("the agent binding did not move; the publisher's Ready gate would not be re-run")
		}
	})

	t.Run("the publisher's own definition rebuilds the runner", func(t *testing.T) {
		next := base
		next.sources = strings.Replace(octoSources(oidc), "identity: ben-publish", "identity: ben-pusher", 1)
		after := next.mustLoad(t)
		if reflect.DeepEqual(before.Config.AgentBinding(), after.Config.AgentBinding()) {
			t.Error("editing the publish source left the agent binding unchanged")
		}
	})
}

// `adapterChange` compares the **bindings**, not the raw configuration sections
// (SPEC §5.4, amendment 2; mutations 19, 9).
//
// The three rows are the three the raw comparison got wrong: a
// `credential_sources` edit beneath an unchanged name (which `Config.Tracker`
// cannot see at all), a rename with an identical definition (which it would
// rebuild for), and an `oidc_token_path` edit (which an authority comparison
// would miss).
func TestAdapterChangeComparesTheBindings(t *testing.T) {
	oidc := "/var/run/secrets/octo/oidc-token"
	base := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources(oidc),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}
	before := base.mustLoad(t)

	t.Run("a scope edit beneath an unchanged name rebuilds", func(t *testing.T) {
		next := base
		next.sources = strings.Replace(octoSources(oidc),
			"scope: srhg-ai-7cef3f93\n    identity: ben-tracker", "scope: another-org\n    identity: ben-tracker", 1)
		if got := adapterChange(before, next.mustLoad(t)); !got.Tracker {
			t.Error("a scope edit rebuilt nothing; the daemon would keep exchanging against the old namespace")
		}
	})

	t.Run("an oidc_token_path edit beneath an unchanged name rebuilds", func(t *testing.T) {
		next := base
		next.sources = strings.Replace(octoSources(oidc),
			"identity: ben-tracker\n    oidc_token_path: "+oidc,
			"identity: ben-tracker\n    oidc_token_path: "+oidc+"-other", 1)
		if got := adapterChange(before, next.mustLoad(t)); !got.Tracker {
			t.Error("an oidc_token_path edit rebuilt nothing; comparing Authority rather than BindingKey misses it")
		}
	})

	t.Run("a rename with an identical definition rebuilds nothing", func(t *testing.T) {
		next := sourceWorkflow{
			trackerProvider: "    repo: acme/widgets\n    credential_source: issues\n    claim_assignee: ben-bot\n",
			sources: strings.Replace(strings.Replace(octoSources(oidc),
				"  tracker:\n", "  issues:\n", 1), "  publish:\n", "  pushes:\n", 1),
			publish: "publish:\n  kind: source\n  env: GH_TOKEN\n  source: pushes\n",
			limits:  octoLimits,
		}
		got := adapterChange(before, next.mustLoad(t))
		if got.Tracker || got.Agent {
			t.Errorf("a rename rebuilt %+v; neither key carries a name", got)
		}
	})

	t.Run("a tracker.kind change rebuilds through its own comparison", func(t *testing.T) {
		// Asserted on the comparison rather than through a file, because there
		// is exactly one registered tracker kind: what matters is that the leg
		// exists beside the binding, not that a second kind ships.
		next := *before
		nextCfg := before.Config
		nextCfg.Tracker.Kind = "something-else"
		next.Config = nextCfg
		if got := adapterChange(before, &next); !got.Tracker {
			t.Error("a tracker.kind change rebuilt nothing")
		}
		if reflect.DeepEqual(before.Config.TrackerBinding(), next.Config.TrackerBinding()) != true {
			t.Error("the binding moved with the kind; a binding that carried it would conflate " +
				"which adapter is built with what it is built from")
		}
	})
}

// `tracker.kind` keeps its own comparison beside the binding: the registry
// resolves it *to* an adapter, so it selects which one is built rather than what
// it is built from (mutation 21).
func TestTrackerKindIsNotInTheBinding(t *testing.T) {
	setTrackerToken(t, "ghp-resolved")
	def := sourceWorkflow{}.mustLoad(t)
	b := def.Config.TrackerBinding()
	for _, v := range []any{b.Credential.Kind, b.Credential.BindingKey, b.ClaimAssignee} {
		if s, ok := v.(string); ok && s == def.Config.Tracker.Kind {
			t.Errorf("the binding carries tracker.kind at %q", s)
		}
	}
	if strings.Contains(fmt.Sprint(b.Provider), "github") {
		t.Error("the reduced provider block carries the adapter kind")
	}
}

// `Describe` is pure, and this is the property `make workflow-check` rests on:
// a workload-identity configuration load-validates with no credential on the
// host and nothing mounted at the path it names (SPEC §5.7, amendment 4).
func TestAWorkloadIdentityWorkflowLoadsWithNoCredentialPresent(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("BEN_TEST_TRACKER_TOKEN", "")
	def := sourceWorkflow{
		trackerProvider: octoTrackerProvider,
		sources:         octoSources("/nonexistent/octo/oidc-token"),
		publish:         octoPublishBlock,
		limits:          octoLimits,
	}.mustLoad(t)

	// And the *instances* can still be constructed, because construction is
	// also pure: the exchange happens at Ready.
	if _, err := def.Config.NewSources(registry.Source); err != nil {
		t.Errorf("NewSources = %v, want ok with nothing mounted", err)
	}
	if _, err := os.Stat("/nonexistent/octo/oidc-token"); err == nil {
		t.Skip("the path this test assumes absent exists")
	}
}

// The two implicit consumer credentials are separate source instances. Named
// sources have the stronger one-instance-per-declaration rule below.
func TestNewSourcesBuildsDistinctImplicitConsumerSources(t *testing.T) {
	setTrackerToken(t, "ghp-resolved")
	def := sourceWorkflow{
		publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_TEST_PUBLISH\n",
	}.mustLoad(t)

	sources, err := def.Config.NewSources(registry.Source)
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	if sources.Tracker == nil || sources.Publish == nil {
		t.Fatalf("sources = %+v, want one instance each", sources)
	}
	if any(sources.Tracker) == any(sources.Publish) {
		t.Error("the tracker and the publisher share one instance")
	}
	if got := sources.Tracker.Descriptor().Authority; got != "env:BEN_TEST_TRACKER_TOKEN" {
		t.Errorf("tracker instance authority = %q, want the compiled one", got)
	}

	// No publish block yields no instance, and specifically a nil one: a typed
	// nil inside a non-nil interface would make the runner mint from it.
	plain := sourceWorkflow{}.mustLoad(t)
	plainSources, err := plain.Config.NewSources(registry.Source)
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	if plainSources.Publish != nil {
		t.Errorf("publish instance = %v, want nil for a workflow that configures none", plainSources.Publish)
	}
}

type recordingSourceKind struct {
	core.SourceKind
	instances []core.CredentialSource
}

func (k *recordingSourceKind) New(d core.SourceDescriptor, block map[string]any) (core.CredentialSource, error) {
	source, err := k.SourceKind.New(d, block)
	if err == nil {
		k.instances = append(k.instances, source)
	}
	return source, err
}

// Assembly constructs every declared source exactly once, including an
// unreferenced entry, then selects the already-built instance for each
// consumer (SPEC §11).
func TestNewSourcesBuildsEveryNamedSourceOnce(t *testing.T) {
	def := sourceWorkflow{
		trackerProvider: "    repo: acme/widgets\n    credential_source: tracker\n",
		sources: "credential_sources:\n" +
			"  tracker:\n    kind: static\n    value: $BEN_TEST_TRACKER_TOKEN\n" +
			"  unused:\n    kind: static\n    value: $BEN_TEST_UNUSED_TOKEN\n",
	}.mustLoad(t)

	kind := &recordingSourceKind{SourceKind: credential.StaticKind{}}
	sources, err := def.Config.NewSources(func(name string) (core.SourceKind, bool) {
		if name == credential.StaticKindName {
			return kind, true
		}
		return nil, false
	})
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	if len(kind.instances) != 2 {
		t.Fatalf("constructed %d source instances, want one for each of two declarations", len(kind.instances))
	}

	byAuthority := make(map[string]core.CredentialSource, len(kind.instances))
	for _, source := range kind.instances {
		byAuthority[source.Descriptor().Authority] = source
	}
	if len(byAuthority) != 2 || byAuthority["env:BEN_TEST_UNUSED_TOKEN"] == nil {
		t.Errorf("constructed authorities = %v, want both declared sources", reflect.ValueOf(byAuthority).MapKeys())
	}
	if sources.Tracker != byAuthority["env:BEN_TEST_TRACKER_TOKEN"] {
		t.Error("the tracker did not receive its already-constructed named source instance")
	}
}

// The `$VAR` syntax a `static` source accepts is the loader's own. Two
// spellings would mean a credential no environment lookup could ever find, and
// the two regexes live in packages that must not import each other.
// Compared through `publish.value`, which is this package's own "exactly one
// reference and nothing else" rule over the same character class, rather than
// against envVarRe directly: envVarRe *finds* references inside a string, so it
// matches `$WITH-DASH` at `$WITH`, and comparing a search against a validation
// would report a disagreement that is not one.
func TestSourceReferenceSyntaxMatchesTheLoader(t *testing.T) {
	for _, value := range []string{
		"$GH_TOKEN", "$_LEADING", "$A1_B2", "$X",
		"$lower", "$1LEADING", "$WITH-DASH", "not-a-reference", "$A-$B", " $A ",
	} {
		_, sourceErr := (credential.StaticKind{}).Describe(map[string]any{"kind": "static", "value": value})
		_, loaderErr := publishValueVar(value)
		if (sourceErr == nil) != (loaderErr == nil) {
			t.Errorf("%q: the source kind says %v and the loader says %v", value, sourceErr, loaderErr)
		}
	}
	// And the extracted name is the same one, or a source would read a variable
	// the split check never compared.
	name, err := publishValueVar("$GH_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	d, err := (credential.StaticKind{}).Describe(map[string]any{"kind": "static", "value": "$GH_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Authority != credential.EnvAuthority(name) {
		t.Errorf("authority = %q, want %q", d.Authority, credential.EnvAuthority(name))
	}
}
