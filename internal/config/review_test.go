package config

import (
	"errors"
	"strings"
	"testing"
)

// The strict `review:` section (#204). Every case is a *load* refusal, for the
// substrate section's reason: a review controller is authored once and then
// unassigns BEN and revokes a human's required label on every publication, so
// the moment to refuse a wrong login is startup rather than the first round.

// validReview is a workflow whose daemon runs the review controller, with the
// controller's credential named indirectly — the only spelling this section
// accepts.
const validReview = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $TRACKER_PAT
    claim_assignee: ben-bot
  required_labels: ["ben-queue"]
agent:
  kind: claude-code
credential_sources:
  reviewer:
    kind: static
    value: $REVIEW_TOKEN
review:
  enabled: true
  principal: ben-bot
  tracker_author: ben-tracker[bot]
  controller: ben-reviewer[bot]
  auth_source: reviewer
  reviewer_argv: ["codex", "exec", "--json", "-"]
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`

func reviewEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRACKER_PAT", "tracker-secret")
	t.Setenv("REVIEW_TOKEN", "review-secret")
	t.Setenv("AIRLOCK_TOKEN", "airlock-secret")
}

func loadReview(t *testing.T, content string) *WorkflowDefinition {
	t.Helper()
	reviewEnv(t)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return def
}

func refuseReview(t *testing.T, content, wantField string) error {
	t.Helper()
	reviewEnv(t)
	_, err := Load(writeWorkflow(t, content))
	if err == nil {
		t.Fatal("Load accepted an invalid review section")
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

// An omitted section is a daemon with no review controller, and every workflow
// that predates #204 is exactly that.
func TestAnOmittedReviewSectionResolvesToNoController(t *testing.T) {
	def := loadReview(t, validMinimal)
	if def.Config.Review.Enabled {
		t.Fatalf("an omitted review section enabled the controller: %+v", def.Config.Review)
	}
	if def.Config.Credentials.Review.Configured() {
		t.Fatalf("an omitted review section resolved a credential: %+v", def.Config.Credentials.Review)
	}
	if got := def.Provenance["review.enabled"].Source; got != SourceDefault {
		t.Errorf("review.enabled provenance = %v, want default", got)
	}
}

func TestReviewResolvesItsDefaults(t *testing.T) {
	def := loadReview(t, validReview)
	r := def.Config.Review
	if !r.Enabled {
		t.Fatal("the controller did not enable")
	}
	// The required label defaults to the one that already dispatches this
	// daemon: a controller revoking any other label would stop nothing.
	if r.QueueLabel != "ben-queue" {
		t.Errorf("queue_label = %q, want tracker.required_labels[0]", r.QueueLabel)
	}
	if r.RoundCap != DefaultReviewRoundCap || r.IntervalMS != DefaultReviewIntervalMS ||
		r.TimeoutMS != DefaultReviewTimeoutMS || r.MaxDiffBytes != DefaultReviewMaxDiffBytes ||
		r.RequestTimeoutMS != DefaultReviewRequestTimeoutMS {
		t.Errorf("defaults = %+v", r)
	}
	if r.AddHumanReviewLabel {
		t.Error("the informational label is added by default; it must be opt-in")
	}
	if !def.Config.Credentials.Review.Configured() || def.Config.Credentials.Review.Name != "reviewer" {
		t.Errorf("review credential = %+v", def.Config.Credentials.Review)
	}
	for path, want := range map[string]Source{
		"review.enabled":     SourceFile,
		"review.controller":  SourceFile,
		"review.queue_label": SourceDefault,
		"review.round_cap":   SourceDefault,
	} {
		if got := def.Provenance[path].Source; got != want {
			t.Errorf("%s provenance = %v, want %v", path, got, want)
		}
	}
}

func profiledReview() string {
	return strings.Replace(validReview,
		`  reviewer_argv: ["codex", "exec", "--json", "-"]`,
		`  reviewer_default_profile: deep
  reviewer_profiles:
    deep: ["codex", "exec", "--json", "--model", "gpt-5.6-sol", "-c", 'model_reasoning_effort="xhigh"', "-"]
    fast: ["codex", "exec", "--json", "--model", "gpt-5.6-sol", "-c", 'model_reasoning_effort="high"', "-"]`, 1)
}

func TestReviewResolvesNamedReviewerProfiles(t *testing.T) {
	def := loadReview(t, profiledReview())
	r := def.Config.Review
	if r.ReviewerDefaultProfile != "deep" || len(r.ReviewerProfiles) != 2 {
		t.Fatalf("profiles = default %q %+v", r.ReviewerDefaultProfile, r.ReviewerProfiles)
	}
	if got := r.ReviewerProfiles["deep"]; len(got) < 2 || got[len(got)-1] != "-" {
		t.Fatalf("deep argv = %v", got)
	}
	if r.ReviewerArgv != nil {
		t.Fatalf("named profiles retained a legacy argv: %v", r.ReviewerArgv)
	}
	for _, path := range []string{
		"review.reviewer_profiles", "review.reviewer_profiles.deep",
		"review.reviewer_profiles.fast", "review.reviewer_default_profile",
	} {
		if got := def.Provenance[path].Source; got != SourceFile {
			t.Errorf("%s provenance = %v, want file", path, got)
		}
	}
}

func TestReviewRefusesOpenOrAmbiguousProfileDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		field   string
	}{
		{
			name: "legacy and profiles",
			content: strings.Replace(profiledReview(), "  reviewer_default_profile: deep\n",
				"  reviewer_argv: [\"codex\"]\n  reviewer_default_profile: deep\n", 1),
			field: "review.reviewer_profiles",
		},
		{
			name:    "no default",
			content: strings.Replace(profiledReview(), "  reviewer_default_profile: deep\n", "", 1),
			field:   "review.reviewer_default_profile",
		},
		{
			name:    "unknown default",
			content: strings.Replace(profiledReview(), "reviewer_default_profile: deep", "reviewer_default_profile: expensive", 1),
			field:   "review.reviewer_default_profile",
		},
		{
			name:    "empty profile argv",
			content: strings.Replace(profiledReview(), `deep: ["codex", "exec", "--json", "--model", "gpt-5.6-sol", "-c", 'model_reasoning_effort="xhigh"', "-"]`, `deep: []`, 1),
			field:   "review.reviewer_profiles.deep",
		},
		{
			name:    "invalid profile name",
			content: strings.Replace(profiledReview(), "    fast:", "    FAST!:", 1),
			field:   "review.reviewer_profiles.FAST!",
		},
		{
			name: "empty map",
			content: strings.Replace(validReview,
				`  reviewer_argv: ["codex", "exec", "--json", "-"]`, "  reviewer_profiles: {}", 1),
			field: "review.reviewer_profiles",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuseReview(t, tc.content, tc.field)
		})
	}
}

func TestReviewRefusesMoreThanEightProfiles(t *testing.T) {
	content := strings.Replace(profiledReview(), "    fast:", `    p3: ["codex"]
    p4: ["codex"]
    p5: ["codex"]
    p6: ["codex"]
    p7: ["codex"]
    p8: ["codex"]
    p9: ["codex"]
    fast:`, 1)
	refuseReview(t, content, "review.reviewer_profiles")
}

// A block read by nothing is a setting an operator believes is in effect.
func TestReviewRefusesConfigurationWithoutEnabling(t *testing.T) {
	content := strings.Replace(validReview, "  enabled: true\n", "", 1)
	err := refuseReview(t, content, "review.enabled")
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %v, want it to say why", err)
	}
}

// Set-vs-unset is the contract here. An explicit default, false, empty string,
// or empty list is still a setting a disabled controller silently ignores.
func TestDisabledReviewRefusesEveryExplicitSetting(t *testing.T) {
	for _, setting := range []string{
		`principal: ""`,
		`tracker_author: ""`,
		`controller: ""`,
		`allow_shared_tracker_controller: false`,
		`auth_source: ""`,
		`api_base_url: ""`,
		`queue_label: ben-queue`,
		`add_human_review_label: false`,
		`round_cap: 3`,
		`reviewer_argv: []`,
		`reviewer_profiles: {}`,
		`reviewer_default_profile: deep`,
		`reviewer_env: []`,
		`guidance_file: ""`,
		`interval_ms: 300000`,
		`timeout_ms: 1200000`,
		`request_timeout_ms: 60000`,
		`max_diff_bytes: 400000`,
	} {
		t.Run(strings.SplitN(setting, ":", 2)[0], func(t *testing.T) {
			block := "review:\n  enabled: false\n  " + setting + "\n"
			content := strings.Replace(validMinimal, "deployment:\n", block+"deployment:\n", 1)
			refuseReview(t, content, "review.enabled")
		})
	}
}

func TestRemoteReviewRefusesDeleteOnSuccess(t *testing.T) {
	content := strings.Replace(validReview, "credential_sources:\n", `credential_sources:
  airlock:
    kind: static
    value: $AIRLOCK_TOKEN
`, 1)
	content = strings.Replace(content, "review:\n", `substrate:
  kind: airlock
  airlock:
    base_url: https://airlock.example.test
    auth_source: airlock
    profile: ben-agent
    on_success: delete
review:
`, 1)
	err := refuseReview(t, content, "substrate.airlock.on_success")
	if !strings.Contains(err.Error(), "resume") {
		t.Errorf("error = %v, want the retained review workspace reason", err)
	}
}

func TestReviewRefusesAnIncompleteController(t *testing.T) {
	for _, tc := range []struct {
		name  string
		strip string
		field string
	}{
		{name: "no principal", strip: "  principal: ben-bot\n", field: "review.principal"},
		{name: "no tracker author", strip: "  tracker_author: ben-tracker[bot]\n", field: "review.tracker_author"},
		{name: "no controller", strip: "  controller: ben-reviewer[bot]\n", field: "review.controller"},
		{name: "no credential", strip: "  auth_source: reviewer\n", field: "review.auth_source"},
		{
			name:  "no reviewer command",
			strip: `  reviewer_argv: ["codex", "exec", "--json", "-"]` + "\n",
			field: "review.reviewer_argv",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuseReview(t, strings.Replace(validReview, tc.strip, "", 1), tc.field)
		})
	}
}

// #11's identity separation, refused by the loader rather than discovered
// mid-cycle. The rule itself is internal/review's and is asked rather than
// restated, so there is one definition of "these two logins must differ".
func TestReviewRefusesCollapsedIdentities(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{
			name: "the controller is also the claim principal",
			from: "  controller: ben-reviewer[bot]", to: "  controller: ben-bot",
			want: "unassign itself",
		},
		{
			name: "the controller is also the milestone author",
			from: "  controller: ben-reviewer[bot]", to: "  controller: ben-tracker[bot]",
			want: "trigger its own rounds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseReview(t, strings.Replace(validReview, tc.from, tc.to, 1), "review")
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestReviewAllowsSharedTrackerControllerOnlyWhenAttendedAndExplicit(t *testing.T) {
	shared := strings.Replace(validReview,
		"  controller: ben-reviewer[bot]\n",
		"  controller: ben-tracker[bot]\n  allow_shared_tracker_controller: true\n", 1)
	def := loadReview(t, shared)
	if !def.Config.Review.AllowSharedTrackerController {
		t.Fatal("the explicit attended-canary exception was not retained")
	}
	if got := def.Provenance["review.allow_shared_tracker_controller"].Source; got != SourceFile {
		t.Fatalf("exception provenance = %v, want file", got)
	}

	protected := strings.Replace(shared, "  mode: attended", "  mode: protected", 1)
	err := refuseReview(t, protected, "review.allow_shared_tracker_controller")
	if !strings.Contains(err.Error(), "independent tracker/controller provenance") {
		t.Errorf("error = %v, want the provenance loss", err)
	}
}

// The mechanics exception is over one App actor, not merely two source names.
// A deployment may intentionally mint both trusted roles through the tracker's
// authority when attended; without the explicit exception the ordinary
// authority split must continue to refuse it.
func TestAttendedSharedTrackerControllerMayUseTheSameCredentialAuthority(t *testing.T) {
	shared := strings.Replace(validReview,
		"    token: $TRACKER_PAT\n", "    token: $REVIEW_TOKEN\n", 1)
	shared = strings.Replace(shared,
		"  controller: ben-reviewer[bot]\n",
		"  controller: ben-tracker[bot]\n  allow_shared_tracker_controller: true\n", 1)
	def := loadReview(t, shared)
	if got, want := def.Config.Credentials.Review.Authority, def.Config.Credentials.Tracker.Authority; got != want {
		t.Fatalf("review authority = %q, want shared tracker authority %q", got, want)
	}

	withoutException := strings.Replace(shared, "  allow_shared_tracker_controller: true\n", "", 1)
	withoutException = strings.Replace(withoutException,
		"  controller: ben-tracker[bot]\n", "  controller: ben-reviewer[bot]\n", 1)
	_, err := Load(writeWorkflow(t, withoutException))
	if err == nil {
		t.Fatal("Load accepted a shared tracker/controller authority without the attended exception")
	}
	if !errors.Is(err, ErrCredentialAuthorityShared) {
		t.Fatalf("Load = %v, want the ordinary credential-authority refusal", err)
	}
}

// A controller that revokes a label nobody dispatches on stops nothing.
func TestReviewRefusesALabelThatDoesNotDispatch(t *testing.T) {
	content := strings.Replace(validReview, "  enabled: true\n", "  enabled: true\n  queue_label: some-other-label\n", 1)
	err := refuseReview(t, content, "review.queue_label")
	if !strings.Contains(err.Error(), "tracker.required_labels") {
		t.Errorf("error = %v, want it to name the required labels", err)
	}
}

func TestReviewCannotAddAnotherRequiredLabelAsInformational(t *testing.T) {
	content := strings.Replace(validReview,
		`required_labels: ["ben-queue"]`, `required_labels: ["ben-queue", "human-review"]`, 1)
	content = strings.Replace(content, "  enabled: true\n", "  enabled: true\n  add_human_review_label: true\n", 1)
	err := refuseReview(t, content, "review")
	if !strings.Contains(err.Error(), "would be an approval") {
		t.Errorf("error = %v, want it to refuse adding a required label", err)
	}
}

// The reviewer holds no forge or backend credential, and an operator learns at
// startup rather than at the first round.
func TestReviewRefusesAReviewerHandedACredential(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "gh_token", "BEN_REVIEW_TOKEN", "BEN_AIRLOCK_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			content := strings.Replace(validReview, "  enabled: true\n",
				"  enabled: true\n  reviewer_env: [\""+name+"\"]\n", 1)
			err := refuseReview(t, content, "review.reviewer_env[0]")
			if !strings.Contains(err.Error(), "credential") {
				t.Errorf("error = %v, want it to say why", err)
			}
		})
	}
	// The reviewer's own provider credential is the operator's call locally.
	content := strings.Replace(validReview, "  enabled: true\n",
		"  enabled: true\n  reviewer_env: [\"OPENAI_API_KEY\"]\n", 1)
	if def := loadReview(t, content); len(def.Config.Review.ReviewerEnv) != 1 {
		t.Errorf("the reviewer's own provider credential was refused: %+v", def.Config.Review)
	}
}

// The reviewer's home directory is BEN's per run (#241), so a passthrough
// naming it is a config site that would do nothing — refused at load, where the
// operator who wrote it is looking.
func TestReviewRefusesAReviewerEnvNamingTheComposedHome(t *testing.T) {
	for _, name := range []string{
		"HOME", "home",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM",
	} {
		t.Run(name, func(t *testing.T) {
			content := strings.Replace(validReview, "  enabled: true\n",
				"  enabled: true\n  reviewer_env: [\""+name+"\"]\n", 1)
			err := refuseReview(t, content, "review.reviewer_env[0]")
			if !strings.Contains(err.Error(), "composed per run") {
				t.Errorf("error = %v, want it to say why", err)
			}
		})
	}
}

func TestReviewRefusesUnknownKeysAndNonPositiveNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		insert  string
		field   string
		unknown bool
	}{
		{name: "an unknown key", insert: "  reviewr_argv: [\"x\"]\n", unknown: true},
		{name: "a round cap of zero", insert: "  round_cap: 0\n", field: "review"},
		{name: "a negative interval", insert: "  interval_ms: -1\n", field: "review.interval_ms"},
		{name: "a zero timeout", insert: "  timeout_ms: 0\n", field: "review.timeout_ms"},
		{name: "a zero diff bound", insert: "  max_diff_bytes: 0\n", field: "review.max_diff_bytes"},
		{name: "a diff bound past what a run may retain", insert: "  max_diff_bytes: 99999999\n", field: "review.max_diff_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(validReview, "  enabled: true\n", "  enabled: true\n"+tc.insert, 1)
			if tc.unknown {
				_, err := Load(writeWorkflow(t, content))
				if err == nil || !strings.Contains(err.Error(), "field") {
					t.Fatalf("an unknown review key was accepted: %v", err)
				}
				return
			}
			refuseReview(t, content, tc.field)
		})
	}
}

// The controller's credential is a fifth identity and must not be any of the
// others. #11's whole safety argument rests on it.
func TestReviewCredentialMustNotBeAnyOfBENsOwn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		consumer string
	}{
		{
			name: "the tracker's",
			content: strings.Replace(validReview,
				"    value: $REVIEW_TOKEN", "    value: $TRACKER_PAT", 1),
			consumer: "tracker",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewEnv(t)
			_, err := Load(writeWorkflow(t, tc.content))
			if err == nil {
				t.Fatal("a controller sharing an identity with BEN was accepted")
			}
			var shared *SubstrateCredentialError
			if !errors.As(err, &shared) {
				t.Fatalf("got %v, want a *SubstrateCredentialError", err)
			}
			if shared.Holder != "review" || shared.Consumer != tc.consumer {
				t.Fatalf("collision = %+v, want the review credential against %s", shared, tc.consumer)
			}
			if !strings.Contains(err.Error(), "review.auth_source") {
				t.Errorf("error = %v, want it to name the field to change", err)
			}
		})
	}
}

// The declaration is process-lifetime: outstanding review runs address the
// reviewer they were dispatched to, and the identities decide what the forge is
// read as.
func TestReviewDeclarationCannotChangeByReload(t *testing.T) {
	before := loadReview(t, validReview).Config.ReviewBinding()
	after := loadReview(t, strings.Replace(validReview,
		"  controller: ben-reviewer[bot]", "  controller: another-reviewer[bot]", 1)).Config.ReviewBinding()
	if sameReviewBinding(before, after) {
		t.Fatal("a changed controller identity compares equal")
	}
	if !sameReviewBinding(before, before) {
		t.Fatal("an unchanged declaration compares unequal")
	}
	// Including the parts a struct comparison would miss.
	argv := loadReview(t, strings.Replace(validReview,
		`["codex", "exec", "--json", "-"]`, `["codex", "exec"]`, 1)).Config.ReviewBinding()
	if sameReviewBinding(before, argv) {
		t.Fatal("a changed reviewer argv compares equal; the comparison skips a slice field")
	}
	required := loadReview(t, strings.Replace(validReview,
		`required_labels: ["ben-queue"]`, `required_labels: ["ben-queue", "security-approved"]`, 1)).Config.ReviewBinding()
	if sameReviewBinding(before, required) {
		t.Fatal("a changed required-label workspace identity compares equal")
	}
	shared := loadReview(t, strings.Replace(validReview,
		"  controller: ben-reviewer[bot]\n",
		"  controller: ben-tracker[bot]\n  allow_shared_tracker_controller: true\n", 1)).Config.ReviewBinding()
	if sameReviewBinding(before, shared) {
		t.Fatal("a changed shared-identity exception compares equal")
	}
	profiles := loadReview(t, profiledReview()).Config.ReviewBinding()
	changedProfiles := loadReview(t, strings.Replace(profiledReview(),
		`model_reasoning_effort="high"`, `model_reasoning_effort="medium"`, 1)).Config.ReviewBinding()
	if sameReviewBinding(profiles, changedProfiles) {
		t.Fatal("a changed named reviewer argv compares equal")
	}
}

// `config effective` prints the whole declaration with no credentials and no
// network, exactly as it does for every other section. The logins in particular
// are what an operator needs to read back.
func TestReviewRendersInEffectiveOutput(t *testing.T) {
	def := loadReview(t, validReview)
	text := EffectiveText(def)
	for _, want := range []string{
		"review:", "enabled: true", "ben-reviewer[bot]", "ben-tracker[bot]",
		"queue_label: ben-queue", "allow_shared_tracker_controller: false", `reviewer_argv: ["codex", "exec", "--json", "-"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("effective output does not carry %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "$REVIEW_TOKEN=") {
		t.Error("the effective output resolved a credential value")
	}

	profiledText := EffectiveText(loadReview(t, profiledReview()))
	for _, want := range []string{"reviewer_default_profile: deep", "reviewer_profiles:", "deep:", "gpt-5.6-sol", "model_reasoning_effort"} {
		if !strings.Contains(profiledText, want) {
			t.Errorf("profiled effective output does not carry %q:\n%s", want, profiledText)
		}
	}

	body, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"review"`) || !strings.Contains(string(body), `"ben-reviewer[bot]"`) {
		t.Errorf("EffectiveJSON does not carry the review declaration:\n%s", body)
	}

	// A disabled controller renders one shape too, with an explicit null.
	off := EffectiveJSON
	disabled, err := off(loadReview(t, validMinimal))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disabled), `"controller": null`) {
		t.Errorf("a disabled controller does not render as null:\n%s", disabled)
	}
}
