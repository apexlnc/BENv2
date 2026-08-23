package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// workflowWithPublish builds a loadable workflow whose `publish` block is
// spelled by the caller — the whole variable in these tests. The agent block
// carries permission_mode for the reason workflowWith does: Load never runs
// Structural, so a file that only ever failed *there* would let a negative test
// pass for the wrong reason.
func workflowWithPublish(publish string) string {
	return "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
		"  required_labels: [\"ben\"]\nagent:\n  kind: claude-code\n  provider:\n" +
		"    permission_mode: bypassPermissions\n" +
		publish +
		"deployment:\n  mode: attended\n" +
		"---\nDo the work described in {{ issue.title }}.\n"
}

const tokenPublishBlock = "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_PUBLISH_TOKEN\n"

// The block parses to a reference: a kind, a child variable, and the *name* of
// the variable holding the credential (SPEC §5.2.8).
func TestLoadPublishBlock(t *testing.T) {
	def, err := Load(writeWorkflow(t, workflowWithPublish(tokenPublishBlock)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := PublishConfig{Kind: PublishKindToken, Env: "GH_TOKEN", ValueVar: "BEN_PUBLISH_TOKEN"}
	if def.Config.Publish != want {
		t.Errorf("Publish = %+v, want %+v", def.Config.Publish, want)
	}
	if got, want := def.Config.Publish.ValueReference(), "$BEN_PUBLISH_TOKEN"; got != want {
		t.Errorf("ValueReference = %q, want %q", got, want)
	}
	// The projection that crosses the assembly boundary carries the two names and
	// nothing else: a kind is this package's business, and a value does not exist.
	wantCred := core.PublishCredential{Env: "GH_TOKEN", Var: "BEN_PUBLISH_TOKEN"}
	if got := def.Config.Publish.Credential(); got != wantCred {
		t.Errorf("Credential = %+v, want %+v", got, wantCred)
	}
}

// An absent block is not a misconfiguration: BEN then injects no publish
// credential and the agent authenticates from what §7.6's allowlist carries,
// which is the arrangement that predates the block existing (SPEC §5.2.8).
func TestLoadWithoutPublishBlock(t *testing.T) {
	def, err := Load(writeWorkflow(t, workflowWithPublish("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if def.Config.Publish.Configured() {
		t.Errorf("Publish = %+v, want the zero value", def.Config.Publish)
	}
	if got := def.Config.Publish.Credential(); got != (core.PublishCredential{}) {
		t.Errorf("Credential = %+v, want the zero value", got)
	}
}

// `publish.value` is a *name* at load and a value only per attempt, which is why
// this file loads with nothing in the environment (SPEC §5.5, §5.2.8).
//
// This is the property `make workflow-check` depends on: CI load-validates the
// repo's own WORKFLOW.md and holds no publish credential, so a loader that
// resolved the reference would refuse BEN's own dogfood file on every CI run.
// Asserted with the variable explicitly unset rather than merely unmentioned, so
// it cannot pass because some other test happened to leave it set.
func TestLoadDoesNotResolvePublishValue(t *testing.T) {
	t.Setenv("BEN_PUBLISH_TOKEN", "") // set-but-empty is "missing" under §5.5

	def, err := Load(writeWorkflow(t, workflowWithPublish(tokenPublishBlock)))
	if err != nil {
		t.Fatalf("Load = %v, want ok with no publish credential present", err)
	}
	if got := def.Config.Publish.ValueVar; got != "BEN_PUBLISH_TOKEN" {
		t.Errorf("ValueVar = %q, want the name, unresolved", got)
	}
	// And the provenance says "file", because nothing was read from the
	// environment: the field holds what the file wrote.
	if got := def.Provenance["publish.value"].Source; got != SourceFile {
		t.Errorf("publish.value provenance = %q, want %q", got, SourceFile)
	}
	if vars := def.Provenance["publish.value"].EnvVars; len(vars) != 0 {
		t.Errorf("publish.value provenance names env vars %v, but nothing was resolved", vars)
	}
}

// The refusals, each a named value carrying the field path (AGENTS.md).
func TestLoadRefusesInvalidPublishBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish string
		// wantField is the ValidationError path; wantErr is set instead for the
		// one refusal that is a sentinel rather than a field rule.
		wantField string
		wantErr   error
	}{
		{
			name:      "unknown key",
			publish:   "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $T\n  app_id: 4039728\n",
			wantField: "", // strict decoding refuses this before validation
		},
		{
			name:      "kind omitted",
			publish:   "publish:\n  env: GH_TOKEN\n  value: $T\n",
			wantField: "publish.kind",
		},
		{
			// #117 boundary 2's kind, which does not exist yet. A closed set means
			// an operator who writes it is told so, rather than getting a block
			// that loads and injects nothing.
			name:      "unsupported kind",
			publish:   "publish:\n  kind: github_app\n  env: GH_TOKEN\n  value: $T\n",
			wantField: "publish.kind",
		},
		{
			name:      "env omitted",
			publish:   "publish:\n  kind: token\n  value: $T\n",
			wantField: "publish.env",
		},
		{
			// The composed child environment is a list of NAME=value entries, so a
			// name carrying `=` produces one no child can read back.
			name:      "env is not an environment variable name",
			publish:   "publish:\n  kind: token\n  env: \"GH TOKEN=x\"\n  value: $T\n",
			wantField: "publish.env",
		},
		{
			name:      "env uses the orchestrator's prefix",
			publish:   "publish:\n  kind: token\n  env: BEN_PUBLISH\n  value: $T\n",
			wantField: "publish.env",
		},
		{
			// §7.6's allowlist is copied into every child, and HOME is what both
			// harnesses resolve their own stored credential from: naming it here
			// would point that lookup at a token.
			name:      "env names an allowlisted variable",
			publish:   "publish:\n  kind: token\n  env: HOME\n  value: $T\n",
			wantField: "publish.env",
		},
		{
			name:      "value omitted for kind token",
			publish:   "publish:\n  kind: token\n  env: GH_TOKEN\n",
			wantField: "publish.value",
		},
		{
			name:    "value is a literal credential",
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: ghp_literalcredential\n",
			wantErr: ErrPublishValue,
		},
		{
			// One token is not the concatenation of several secrets.
			name:    "value interpolates two variables",
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $PREFIX$SUFFIX\n",
			wantErr: ErrPublishValue,
		},
		{
			name:    "value carries surrounding text",
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: \"Bearer $T\"\n",
			wantErr: ErrPublishValue,
		},
		{
			// An explicit null decodes to "absent", and absent here *means*
			// something: no publish credential is injected. Selecting that
			// silently is a capability loss that looks like a passing load.
			name:      "the whole block written empty",
			publish:   "publish:\n",
			wantField: "publish",
		},
		// Every written-but-empty spelling, because the first fix enumerated two of
		// them and `{kind: ""}` walked through the gap: a one-key map is not empty
		// and an empty string is not null, so it decoded to the zero PublishConfig
		// and read as omission — preserving the ambient-HOME fallback the section
		// exists to replace. The rule is presence ⇒ kind, and these rows are what
		// keeps it stated that way rather than as a list of shapes.
		{
			// `{}` needs saying at all because it is the *deliberate* spelling for
			// an empty provider block. There is no empty publish credential.
			name:      "the whole block written as an empty map",
			publish:   "publish: {}\n",
			wantField: "publish.kind",
		},
		{
			name:      "kind written empty",
			publish:   "publish:\n  kind: \"\"\n",
			wantField: "publish.kind",
		},
		{
			name:      "env written empty and nothing else",
			publish:   "publish:\n  env: \"\"\n",
			wantField: "publish.kind",
		},
		{
			// Refused as a malformed reference rather than by the presence rule:
			// an empty string is not one `$VAR`, and that is the more specific
			// thing to tell an operator. The presence rule is still the backstop
			// underneath it — both refuse this block, independently.
			name:    "value written empty and nothing else",
			publish: "publish:\n  value: \"\"\n",
			wantErr: ErrPublishValue,
		},
		{
			name:    "every field written empty",
			publish: "publish:\n  kind: \"\"\n  env: \"\"\n  value: \"\"\n",
			wantErr: ErrPublishValue,
		},
		{
			// Present and non-empty, so the closed-set refusal names the value —
			// distinct from the presence rule above, and it must stay distinct.
			name:      "kind written as whitespace",
			publish:   "publish:\n  kind: \"  \"\n  env: GH_TOKEN\n  value: $T\n",
			wantField: "publish.kind",
		},
		{
			// Whitespace is "something else" (SPEC §5.2.8's exact-reference rule).
			// Reachable only by quoting — YAML strips it from a plain scalar — so
			// it is a deliberate spelling, and rewriting it silently would make the
			// loader lenient about the one field the rule keeps exact.
			name:    "value is a padded reference",
			publish: "publish:\n  kind: token\n  env: GH_TOKEN\n  value: \" $T \"\n",
			wantErr: ErrPublishValue,
		},
		{
			name:      "a field written empty",
			publish:   "publish:\n  kind: token\n  env: GH_TOKEN\n  value:\n",
			wantField: "publish.value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("T", "a-token")
			t.Setenv("PREFIX", "gh")
			t.Setenv("SUFFIX", "p_rest")

			_, err := Load(writeWorkflow(t, workflowWithPublish(tc.publish)))
			if err == nil {
				t.Fatal("Load = nil, want a refusal")
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Load = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantField == "" {
				return // the strict YAML pass owns this one; any refusal will do
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Load = %v, want a *ValidationError", err)
			}
			if verr.Field != tc.wantField {
				t.Errorf("field = %q, want %q", verr.Field, tc.wantField)
			}
		})
	}
}

// The field rules, asked directly, because the table above cannot distinguish
// *which* stage refused a file and this half of the rule is not reachable through
// Load at all: resolve refuses a written-but-kindless block first, using the fact
// only it holds.
//
// So the branch would otherwise be untested code guarded by a comment claiming it
// cannot fire — and the claim would be about today's single caller rather than
// about the function. Stated here instead: whatever assembles a PublishConfig, an
// empty kind means "absent" only when nothing else was set.
func TestValidatePublish(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       PublishConfig
		wantErr bool
	}{
		{name: "the zero value is an omitted block", p: PublishConfig{}},
		{
			name:    "a kind with no env",
			p:       PublishConfig{Kind: PublishKindToken},
			wantErr: true,
		},
		{
			// The shape resolve catches through Load, asserted where the rule lives.
			name:    "an env with no kind",
			p:       PublishConfig{Env: "GH_TOKEN"},
			wantErr: true,
		},
		{
			name:    "a value with no kind",
			p:       PublishConfig{ValueVar: "BEN_PUBLISH_TOKEN"},
			wantErr: true,
		},
		{
			name: "complete",
			p:    PublishConfig{Kind: PublishKindToken, Env: "GH_TOKEN", ValueVar: "BEN_PUBLISH_TOKEN"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePublish(tc.p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validatePublish(%+v) = %v, want refusal=%v", tc.p, err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) || !strings.HasPrefix(verr.Field, "publish.") {
				t.Errorf("refusal = %v, want a *ValidationError naming a publish field", err)
			}
		})
	}
}

// The one refusal in this package whose offending value may be a live credential
// must not print it (SPEC §5.2.8, and the reasoning core.ConfigValueError was
// introduced for).
//
// Anchored on the rendered message rather than on the mechanism: it does not care
// that the value travels as ConfigValueError.Value, only that no path from Load to
// an operator's terminal carries the token. An operator who pastes a literal
// credential into this field and hits `make workflow-check` in CI would otherwise
// have published it to the build log of the run that refused it.
func TestPublishValueRefusalDoesNotEchoTheCredential(t *testing.T) {
	const literal = "ghp_averyrealisticlookingcredential123"
	_, err := Load(writeWorkflow(t, workflowWithPublish(
		"publish:\n  kind: token\n  env: GH_TOKEN\n  value: "+literal+"\n")))
	if !errors.Is(err, ErrPublishValue) {
		t.Fatalf("Load = %v, want %v", err, ErrPublishValue)
	}
	if strings.Contains(err.Error(), literal) {
		t.Errorf("the refusal echoes the credential: %v", err)
	}
	// It still travels as data, so a renderer that *has* provenance could choose
	// to show it — the split core.ConfigValueError exists for.
	var verr *core.ConfigValueError
	if !errors.As(err, &verr) {
		t.Fatalf("refusal = %#v, want a *core.ConfigValueError carrying the value", err)
	}
	if verr.Field != "publish.value" || verr.Value != literal {
		t.Errorf("refusal = ConfigValueError{Field: %q, Value: %q}, want {publish.value, the literal}",
			verr.Field, verr.Value)
	}
}

// The publish credential is the third site the §10.2 split covers, and it is
// invisible to the other three: not in the opaque block, not forwarded by the
// adapter, and never resolved, so no provenance entry names it (SPEC §10.2).
//
// Without this, a tracker PAT reaching the agent as `publish.value` passes the
// one check that exists to stop a single secret doing both jobs.
func TestLoadRefusesTheTrackerCredentialAsPublishValue(t *testing.T) {
	t.Setenv("SHARED_TOKEN", "value-of-SHARED_TOKEN")

	_, err := Load(writeWorkflow(t,
		"---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n"+
			"    token: $SHARED_TOKEN\n  required_labels: [\"ben\"]\n"+
			"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n"+
			"publish:\n  kind: token\n  env: GH_TOKEN\n  value: $SHARED_TOKEN\n"+
			"deployment:\n  mode: attended\n"+
			"---\nDo the work described in {{ issue.title }}.\n"))

	var shared *CredentialSharedError
	if !errors.As(err, &shared) {
		t.Fatalf("Load = %v, want CredentialSharedError", err)
	}
	if shared.Var != "SHARED_TOKEN" {
		t.Errorf("Var = %q, want SHARED_TOKEN", shared.Var)
	}
	if shared.AgentField != "publish.value" {
		t.Errorf("AgentField = %q, want publish.value — the line an operator can edit", shared.AgentField)
	}
	if msg := shared.Error(); strings.Contains(msg, "value-of-") {
		t.Errorf("the refusal leaked a resolved secret: %s", msg)
	}
}

// The tracker's *undeclared* fallback against the publish block: the tracker names
// no variable at all, so the collision exists only because the kind declares what
// its Ready reads. This is #47's shape aimed at the new site.
func TestLoadRefusesTheTrackerFallbackAsPublishValue(t *testing.T) {
	_, err := Load(writeWorkflow(t, workflowWithPublish(
		"publish:\n  kind: token\n  env: GH_TOKEN\n  value: $GITHUB_TOKEN\n")))

	var shared *CredentialSharedError
	if !errors.As(err, &shared) {
		t.Fatalf("Load = %v, want CredentialSharedError", err)
	}
	if shared.Var != "GITHUB_TOKEN" || shared.AgentField != "publish.value" {
		t.Errorf("refusal = {Var: %q, AgentField: %q}, want {GITHUB_TOKEN, publish.value}",
			shared.Var, shared.AgentField)
	}
}

// `config effective` prints the block as written, in both renderings.
//
// Not redacted, and the schema is the guarantee rather than a promise: every field
// here is a name, because `publish.value` accepts exactly one `$VAR` reference and
// refuses a literal (TestLoadRefusesInvalidPublishBlock). Hiding the reference
// would conceal what an operator needs when the credential is misconfigured — the
// call §10.2 already makes for `env_passthrough` names.
func TestEffectiveRendersPublishAsWritten(t *testing.T) {
	t.Setenv("BEN_PUBLISH_TOKEN", "ghp-must-not-be-readable-from-output")

	def, err := Load(writeWorkflow(t, workflowWithPublish(tokenPublishBlock)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	text := EffectiveText(def)
	for _, want := range []string{"publish:", "kind: token", "env: GH_TOKEN", "value: $BEN_PUBLISH_TOKEN"} {
		if !strings.Contains(text, want) {
			t.Errorf("EffectiveText is missing %q:\n%s", want, text)
		}
	}
	// The value the variable holds is not in the output, because nothing read it.
	if strings.Contains(text, "ghp-must-not-be-readable-from-output") {
		t.Errorf("EffectiveText resolved the publish credential:\n%s", text)
	}

	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatalf("EffectiveJSON: %v", err)
	}
	if strings.Contains(string(raw), "ghp-must-not-be-readable-from-output") {
		t.Errorf("EffectiveJSON resolved the publish credential:\n%s", raw)
	}
	var doc struct {
		Config struct {
			Publish struct {
				Kind, Env, Value string
			}
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	got := doc.Config.Publish
	if got.Kind != "token" || got.Env != "GH_TOKEN" || got.Value != "$BEN_PUBLISH_TOKEN" {
		t.Errorf("JSON publish = %+v, want {token GH_TOKEN $BEN_PUBLISH_TOKEN}", got)
	}
}

// The migration pin, config half: the `publish` block denotes exactly the
// `env_passthrough` entry it replaces — the same daemon variable to the same child
// variable (#117 boundary 1 introduces no new capability).
//
// The other half is TestEnvironPublishMatchesForwardingTheSameVariable in
// internal/agent/harness, which asserts those two references compose an identical
// child environment. This one cannot: composing a child environment is not the
// loader's job, and internal/config must not import an adapter to find out.
func TestPublishBlockDenotesTheEnvPassthroughItReplaces(t *testing.T) {
	oldSpelling, err := Load(writeWorkflow(t, workflowWith("", "    env_passthrough: [GH_TOKEN]\n")))
	if err != nil {
		t.Fatalf("loading the env_passthrough spelling: %v", err)
	}
	newSpelling, err := Load(writeWorkflow(t,
		workflowWithPublish("publish:\n  kind: token\n  env: GH_TOKEN\n  value: $GH_TOKEN\n")))
	if err != nil {
		t.Fatalf("loading the publish spelling: %v", err)
	}

	kind, ok := registry.Runner(oldSpelling.Config.Agent.Kind)
	if !ok {
		t.Fatalf("no runner kind for %q", oldSpelling.Config.Agent.Kind)
	}

	// The old spelling forwards the daemon's GH_TOKEN by name, and states no
	// publish credential.
	if got := kind.ForwardedEnvVars(oldSpelling.Config.Agent.Provider); len(got) != 1 || got[0] != "GH_TOKEN" {
		t.Errorf("old spelling forwards %v, want [GH_TOKEN]", got)
	}
	if oldSpelling.Config.Publish.Configured() {
		t.Error("old spelling states a publish credential; it is supposed to predate the block")
	}

	// The new spelling forwards nothing by name — it must not, since the
	// reservation refuses both spellings at once — and names the same pair.
	if got := kind.ForwardedEnvVars(newSpelling.Config.Agent.Provider); len(got) != 0 {
		t.Errorf("new spelling forwards %v, want nothing: the publish block replaces it", got)
	}
	want := core.PublishCredential{Env: "GH_TOKEN", Var: "GH_TOKEN"}
	if got := newSpelling.Config.Publish.Credential(); got != want {
		t.Errorf("new spelling = %+v, want %+v — same source variable, same child variable", got, want)
	}
}

// And the two spellings together are refused, which is what makes the migration
// mechanical rather than merely intended: a half-migrated file does not load, so
// `make workflow-check` catches it (SPEC §7.6).
func TestLoadRefusesBothSpellingsAtOnce(t *testing.T) {
	t.Setenv("BEN_PUBLISH_TOKEN", "ghp-token")
	path := writeWorkflow(t,
		"---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n"+
			"  required_labels: [\"ben\"]\nagent:\n  kind: claude-code\n  provider:\n"+
			"    permission_mode: bypassPermissions\n    env_passthrough: [GH_TOKEN]\n"+
			tokenPublishBlock+
			"deployment:\n  mode: attended\n"+
			"---\nDo the work described in {{ issue.title }}.\n")

	// Load itself accepts it: the collision is between a core-owned field and an
	// *opaque* block, so only the adapter can see it, and Load never runs
	// Structural (SPEC §5.7, §5.8). `ben config effective` — which is what
	// `make workflow-check` runs — is where it refuses.
	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v; the refusal belongs to Structural, not to the loader", err)
	}
	kind, ok := registry.Runner(def.Config.Agent.Kind)
	if !ok {
		t.Fatalf("no runner kind for %q", def.Config.Agent.Kind)
	}
	err = kind.Structural(core.AgentConfig{
		Provider: def.Config.Agent.Provider,
		Publish:  def.Config.Publish.Credential(),
	})
	if err == nil {
		t.Fatal("Structural = nil, want a refusal: two sites write GH_TOKEN")
	}
	if !strings.Contains(err.Error(), "publish.env") {
		t.Errorf("refusal does not name the owning site: %v", err)
	}
}
