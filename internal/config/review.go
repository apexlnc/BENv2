package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The review-controller declaration (#204, superseding #11's deployment).
//
// **This section is closed, not opaque**, for `substrate:`'s reason: nothing
// here belongs to a pluggable adapter's schema. Every key is one this package
// validates, and a typo in a login is the failure mode the section exists to
// prevent — a controller told a name that is close but not the API login makes
// every author check fail, and its answer to that is to do nothing at all,
// quietly, and forever.
//
// It is **off by default and cannot be arrived at by omission**, which is the
// opposite posture from `substrate:` and the same one as `deployment.mode`. A
// review controller unassigns BEN and revokes a human's required label; a
// deployment that got one because it did not write a section would be a
// deployment surprised by its own automation.
//
// Nothing here is `$VAR`-resolved, and the one credential it needs is named
// indirectly through `credential_sources` — the same rule, for the same reason,
// as `substrate.airlock.auth_source`.

// DefaultReviewRoundCap is the number of distinct reviewed heads one approval
// cycle may spend. Three, as #11 fixed it: enough for a machine reviewer to be
// useful, few enough that a loop which is not converging reaches a human.
const DefaultReviewRoundCap = 3

// Review timing defaults. The interval is the *sweep*, and it is deliberately
// slower than `polling.interval_ms`: the controller's work is bounded by
// publications rather than by the queue, and every tick of it is a list request
// against the forge.
const (
	DefaultReviewIntervalMS       = 300000
	DefaultReviewTimeoutMS        = 1200000
	DefaultReviewMaxDiffBytes     = 400000
	DefaultReviewRequestTimeoutMS = 60000
)

// ReviewConfig is the resolved declaration.
type ReviewConfig struct {
	// Enabled is the whole gate. False — including by omission — means `ben run`
	// builds no controller, dispatches no reviewer and makes no forge write
	// under the controller identity.
	Enabled bool

	// The three identities #11 requires to be distinct by default. Principal and
	// TrackerAuthor describe BEN itself and Controller is the reviewer's own
	// login; a controller that were either of the others could unassign itself
	// or manufacture its own triggers. The explicit attended-canary exception
	// below relaxes only the tracker/controller comparison.
	Principal     string
	TrackerAuthor string
	Controller    string
	// AllowSharedTrackerController explicitly accepts that GitHub cannot
	// distinguish tracker and controller artifacts when one App mints both
	// credentials. It is valid only for an attended canary.
	AllowSharedTrackerController bool

	// AuthSource names the `credential_sources` entry minting the controller's
	// least-privilege token. Required when enabled: there is no legacy spelling
	// to inherit, and a literal here would be a secret in a section
	// `config effective` prints in full.
	AuthSource string
	// APIBaseURL is the forge API root. Empty is github.com.
	APIBaseURL string

	// QueueLabel is the human-applied required label the controller may only
	// remove. It must be one of `tracker.required_labels`: a controller revoking
	// a label that does not dispatch anything would stop nothing.
	QueueLabel string
	// AddHumanReviewLabel adds the one fixed, non-required informational label
	// on terminal routes.
	AddHumanReviewLabel bool
	// RoundCap bounds automation at this many distinct reviewed heads per
	// approval cycle.
	RoundCap int

	// ReviewerArgv is the command the reviewer runs as, on whichever substrate
	// `substrate:` selects. BEN composes the prompt; this is the operator's
	// choice of model harness.
	ReviewerArgv []string
	// ReviewerProfiles is the operator-owned allowlist used by ticket labels.
	// It is mutually exclusive with ReviewerArgv, which remains the legacy
	// non-selectable form. ReviewerDefaultProfile is used when no profile label
	// is present.
	ReviewerProfiles       map[string][]string
	ReviewerDefaultProfile string
	// ReviewerEnv names host variables a *local* reviewer child may be given.
	// Forge and backend credentials are refused outright; a provider credential
	// is the operator's call locally and is never serialized into a backend
	// request (reviewrun.ForbiddenEnv, reviewrun.ProviderEnv).
	ReviewerEnv []string
	// GuidanceFile is an optional file whose contents are appended to the
	// reviewer's prompt as the deployment's own standard for what counts as a
	// finding. It cannot state the verdict contract, which is BEN's.
	GuidanceFile string

	IntervalMS       int
	TimeoutMS        int
	RequestTimeoutMS int
	MaxDiffBytes     int
}

// ReviewBinding is the process-lifetime identity of the review controller: its
// resolved declaration, the complete tracker approval set that selects its
// workspaces, and the canonical definition of the credential it holds. A name
// alone is insufficient because a reload may edit the referenced source beneath
// an unchanged name (SubstrateBinding's reason).
type ReviewBinding struct {
	Config         ReviewConfig
	Credential     core.SourceBinding
	RequiredLabels []string
}

// ReviewBinding projects Config onto that identity.
func (c Config) ReviewBinding() ReviewBinding {
	var required []string
	if c.Review.Enabled {
		required = append([]string(nil), c.Tracker.RequiredLabels...)
	}
	return ReviewBinding{
		Config:         c.Review,
		Credential:     c.Credentials.Review.Binding(),
		RequiredLabels: required,
	}
}

// rawReview mirrors the YAML. Every field is a pointer so set-vs-unset drives
// defaults and provenance.
type rawReview struct {
	Enabled                      *bool               `yaml:"enabled"`
	Principal                    *string             `yaml:"principal"`
	TrackerAuthor                *string             `yaml:"tracker_author"`
	Controller                   *string             `yaml:"controller"`
	AllowSharedTrackerController *bool               `yaml:"allow_shared_tracker_controller"`
	AuthSource                   *string             `yaml:"auth_source"`
	APIBaseURL                   *string             `yaml:"api_base_url"`
	QueueLabel                   *string             `yaml:"queue_label"`
	AddHumanReviewLabel          *bool               `yaml:"add_human_review_label"`
	RoundCap                     *yamlInt            `yaml:"round_cap"`
	ReviewerArgv                 []string            `yaml:"reviewer_argv"`
	ReviewerProfiles             map[string][]string `yaml:"reviewer_profiles"`
	ReviewerDefaultProfile       *string             `yaml:"reviewer_default_profile"`
	ReviewerEnv                  []string            `yaml:"reviewer_env"`
	GuidanceFile                 *string             `yaml:"guidance_file"`
	IntervalMS                   *yamlInt            `yaml:"interval_ms"`
	TimeoutMS                    *yamlInt            `yaml:"timeout_ms"`
	RequestTimeoutMS             *yamlInt            `yaml:"request_timeout_ms"`
	MaxDiffBytes                 *yamlInt            `yaml:"max_diff_bytes"`
}

// resolveReview applies defaults and records provenance.
func resolveReview(raw *rawReview, cfg *Config, prov Provenance) error {
	rv := raw
	if rv == nil {
		rv = &rawReview{}
	}
	if (rv.Enabled == nil || !*rv.Enabled) && rv.configuredWithoutEnable() {
		return &ValidationError{Field: "review.enabled", Msg: "the review section is configured but not enabled: " +
			"set `enabled: true` to run the controller, or remove the keys — a block read by nothing is a " +
			"setting an operator believes is in effect"}
	}
	if rv.ReviewerArgv != nil && rv.ReviewerProfiles != nil {
		return &ValidationError{Field: "review.reviewer_profiles", Msg: "cannot be combined with review.reviewer_argv; use one non-selectable command or one named allowlist"}
	}
	if rv.ReviewerProfiles != nil && len(rv.ReviewerProfiles) == 0 {
		return &ValidationError{Field: "review.reviewer_profiles", Msg: "must contain at least one named invocation"}
	}
	if rv.Enabled != nil {
		cfg.Review.Enabled, prov["review.enabled"] = *rv.Enabled, FieldOrigin{Source: SourceFile}
	} else {
		prov["review.enabled"] = FieldOrigin{Source: SourceDefault}
	}

	setStr := func(path string, dst *string, src *string) {
		if src != nil {
			*dst, prov[path] = strings.TrimSpace(*src), FieldOrigin{Source: SourceFile}
		} else {
			prov[path] = FieldOrigin{Source: SourceDefault}
		}
	}
	setInt := func(path string, dst *int, src *yamlInt, fallback int) {
		if src != nil {
			*dst, prov[path] = src.Int(), FieldOrigin{Source: SourceFile}
		} else {
			*dst, prov[path] = fallback, FieldOrigin{Source: SourceDefault}
		}
	}

	r := &cfg.Review
	setStr("review.principal", &r.Principal, rv.Principal)
	setStr("review.tracker_author", &r.TrackerAuthor, rv.TrackerAuthor)
	setStr("review.controller", &r.Controller, rv.Controller)
	setStr("review.auth_source", &r.AuthSource, rv.AuthSource)
	setStr("review.api_base_url", &r.APIBaseURL, rv.APIBaseURL)
	setStr("review.guidance_file", &r.GuidanceFile, rv.GuidanceFile)
	setStr("review.reviewer_default_profile", &r.ReviewerDefaultProfile, rv.ReviewerDefaultProfile)
	if rv.AllowSharedTrackerController != nil {
		r.AllowSharedTrackerController = *rv.AllowSharedTrackerController
		prov["review.allow_shared_tracker_controller"] = FieldOrigin{Source: SourceFile}
	} else {
		prov["review.allow_shared_tracker_controller"] = FieldOrigin{Source: SourceDefault}
	}

	// The required label defaults to the tracker's first, which is the label
	// that already dispatches this daemon. Stated rather than assumed: a
	// controller revoking some other label would stop nothing, and one revoking
	// a label a deployment did not think of would stop everything.
	if rv.QueueLabel != nil {
		r.QueueLabel, prov["review.queue_label"] = strings.TrimSpace(*rv.QueueLabel), FieldOrigin{Source: SourceFile}
	} else {
		prov["review.queue_label"] = FieldOrigin{Source: SourceDefault}
		if len(cfg.Tracker.RequiredLabels) > 0 {
			r.QueueLabel = cfg.Tracker.RequiredLabels[0]
		}
	}

	if rv.AddHumanReviewLabel != nil {
		r.AddHumanReviewLabel = *rv.AddHumanReviewLabel
		prov["review.add_human_review_label"] = FieldOrigin{Source: SourceFile}
	} else {
		prov["review.add_human_review_label"] = FieldOrigin{Source: SourceDefault}
	}

	setInt("review.round_cap", &r.RoundCap, rv.RoundCap, DefaultReviewRoundCap)
	setInt("review.interval_ms", &r.IntervalMS, rv.IntervalMS, DefaultReviewIntervalMS)
	setInt("review.timeout_ms", &r.TimeoutMS, rv.TimeoutMS, DefaultReviewTimeoutMS)
	setInt("review.request_timeout_ms", &r.RequestTimeoutMS, rv.RequestTimeoutMS, DefaultReviewRequestTimeoutMS)
	setInt("review.max_diff_bytes", &r.MaxDiffBytes, rv.MaxDiffBytes, DefaultReviewMaxDiffBytes)

	r.ReviewerArgv = trimAll(rv.ReviewerArgv)
	prov["review.reviewer_argv"] = originOr(rv.ReviewerArgv != nil, SourceDefault)
	r.ReviewerProfiles = cloneReviewerProfiles(rv.ReviewerProfiles)
	prov["review.reviewer_profiles"] = originOr(rv.ReviewerProfiles != nil, SourceDefault)
	for name := range r.ReviewerProfiles {
		prov["review.reviewer_profiles."+name] = FieldOrigin{Source: SourceFile}
	}
	r.ReviewerEnv = trimAll(rv.ReviewerEnv)
	prov["review.reviewer_env"] = originOr(rv.ReviewerEnv != nil, SourceDefault)
	return nil
}

// configuredWithoutEnable is anchored in the raw declaration because only it
// can distinguish an omitted field from an explicitly written default, empty
// string, false, zero, or empty list. Every one of those is still a setting an
// operator expects a disabled controller to honor.
func (r *rawReview) configuredWithoutEnable() bool {
	return r.Principal != nil || r.TrackerAuthor != nil || r.Controller != nil ||
		r.AllowSharedTrackerController != nil ||
		r.AuthSource != nil || r.APIBaseURL != nil || r.QueueLabel != nil ||
		r.AddHumanReviewLabel != nil || r.RoundCap != nil || r.ReviewerArgv != nil ||
		r.ReviewerProfiles != nil || r.ReviewerDefaultProfile != nil ||
		r.ReviewerEnv != nil || r.GuidanceFile != nil || r.IntervalMS != nil ||
		r.TimeoutMS != nil || r.RequestTimeoutMS != nil || r.MaxDiffBytes != nil
}

// validateReview enforces the value rules.
//
// A disabled section is checked for the one thing that matters — that nothing
// was written into it — because a block an operator believes is in effect and
// that is read by nothing is the same defect `substrate.airlock` refuses.
func validateReview(cfg *Config) error {
	r := cfg.Review
	if !r.Enabled {
		if r.Principal != "" || r.TrackerAuthor != "" || r.Controller != "" ||
			r.AllowSharedTrackerController || r.AuthSource != "" || len(r.ReviewerArgv) > 0 ||
			len(r.ReviewerProfiles) > 0 || r.ReviewerDefaultProfile != "" || len(r.ReviewerEnv) > 0 || r.GuidanceFile != "" {
			return &ValidationError{Field: "review.enabled", Msg: "the review section is configured but not enabled: " +
				"set `enabled: true` to run the controller, or remove the keys — a block read by nothing is a " +
				"setting an operator believes is in effect"}
		}
		return nil
	}

	for _, f := range []struct {
		field string
		value string
		why   string
	}{
		{"review.principal", r.Principal, "the claim assignee is the only login the controller may unassign"},
		{"review.tracker_author", r.TrackerAuthor, "only comments by this login can trigger a round"},
		{"review.controller", r.Controller, "only artifacts by this login are trusted as the durable record"},
		{"review.auth_source", r.AuthSource, "name a `credential_sources` entry; the controller has no legacy credential spelling"},
	} {
		if f.value == "" {
			return &ValidationError{Field: f.field, Msg: "required when review.enabled is true: " + f.why}
		}
	}
	if _, ok := cfg.CredentialSources[r.AuthSource]; !ok {
		return unknownSourceRef("review.auth_source", r.AuthSource, cfg)
	}
	if cfg.Substrate.Remote() && cfg.Substrate.Airlock.OnSuccess == DisposalDelete {
		return &ValidationError{Field: "substrate.airlock.on_success", Msg: "cannot be `delete` while review.enabled is true: " +
			"the reviewer must resume the published claim's workspace-cycle sandbox; use `suspend` or `retain`"}
	}
	if r.AllowSharedTrackerController && cfg.Deployment.Mode != DeploymentAttended {
		return &ValidationError{Field: "review.allow_shared_tracker_controller", Msg: "may be true only when deployment.mode is `attended`: shared GitHub App attribution cannot prove independent tracker/controller provenance"}
	}

	if r.QueueLabel == "" {
		return &ValidationError{Field: "review.queue_label", Msg: "required: there is no required label to revoke, " +
			"and `tracker.required_labels` supplied no default"}
	}
	if !containsFold(cfg.Tracker.RequiredLabels, r.QueueLabel) {
		return &ValidationError{Field: "review.queue_label", Msg: fmt.Sprintf(
			"%q is not one of tracker.required_labels (%s): revoking a label that does not dispatch this daemon "+
				"stops nothing", r.QueueLabel, strings.Join(cfg.Tracker.RequiredLabels, ", "))}
	}
	if len(r.ReviewerArgv) > 0 && len(r.ReviewerProfiles) > 0 {
		return &ValidationError{Field: "review.reviewer_profiles", Msg: "cannot be combined with review.reviewer_argv; use one non-selectable command or one named allowlist"}
	}
	if len(r.ReviewerProfiles) == 0 {
		if r.ReviewerDefaultProfile != "" {
			return &ValidationError{Field: "review.reviewer_default_profile", Msg: "requires review.reviewer_profiles"}
		}
		if err := validateReviewerArgv("review.reviewer_argv", r.ReviewerArgv); err != nil {
			return err
		}
	} else {
		if r.ReviewerDefaultProfile == "" {
			return &ValidationError{Field: "review.reviewer_default_profile", Msg: "required with review.reviewer_profiles"}
		}
		if len(r.ReviewerProfiles) > review.MaxReviewerProfiles {
			return &ValidationError{Field: "review.reviewer_profiles", Msg: fmt.Sprintf("contains %d profiles; at most %d are allowed", len(r.ReviewerProfiles), review.MaxReviewerProfiles)}
		}
		if _, ok := r.ReviewerProfiles[r.ReviewerDefaultProfile]; !ok {
			return &ValidationError{Field: "review.reviewer_default_profile", Msg: fmt.Sprintf("%q is not one of review.reviewer_profiles", r.ReviewerDefaultProfile)}
		}
		for _, name := range slices.Sorted(maps.Keys(r.ReviewerProfiles)) {
			if !review.ValidReviewerProfile(name) {
				return &ValidationError{Field: "review.reviewer_profiles." + name, Msg: "profile names must be 1-32 lowercase letters, digits, or hyphens"}
			}
			if err := validateReviewerArgv("review.reviewer_profiles."+name, r.ReviewerProfiles[name]); err != nil {
				return err
			}
		}
	}

	// The identity rules are internal/review's, asked here rather than restated:
	// one definition of "these two logins must differ", and the loader is where
	// an operator meets it.
	probe := review.Config{
		Owner: "owner", Repo: "repo", Issue: 1,
		Principal: r.Principal, TrackerAuthor: r.TrackerAuthor, Controller: r.Controller,
		AllowSharedTrackerController: r.AllowSharedTrackerController,
		RequiredLabels:               cfg.Tracker.RequiredLabels,
		QueueLabel:                   r.QueueLabel, AddHumanReviewLabel: r.AddHumanReviewLabel, RoundCap: r.RoundCap,
		ReviewerProfiles: slices.Sorted(maps.Keys(r.ReviewerProfiles)), DefaultReviewerProfile: r.ReviewerDefaultProfile,
	}
	if err := probe.Validate(); err != nil {
		return &ValidationError{Field: "review", Msg: err.Error()}
	}

	for i, name := range r.ReviewerEnv {
		if name == "" {
			return &ValidationError{Field: fmt.Sprintf("review.reviewer_env[%d]", i), Msg: "is empty"}
		}
		// Refused at load rather than at the first review: an operator who wrote
		// it should learn at startup, and the refusal names why. The local rule
		// rather than the shared one, because this key only ever composes a local
		// child's environment — it is refused outright under a substrate — so a
		// name the local executor owns is refused here too (#241).
		if err := reviewrun.CheckLocalEnvName(name); err != nil {
			return &ValidationError{Field: fmt.Sprintf("review.reviewer_env[%d]", i), Msg: err.Error()}
		}
	}

	for _, p := range []struct {
		field string
		v     int
	}{
		{"review.round_cap", r.RoundCap},
		{"review.interval_ms", r.IntervalMS},
		{"review.timeout_ms", r.TimeoutMS},
		{"review.request_timeout_ms", r.RequestTimeoutMS},
		{"review.max_diff_bytes", r.MaxDiffBytes},
	} {
		if p.v <= 0 {
			return &ValidationError{Field: p.field, Msg: "must be a positive integer"}
		}
	}
	if r.MaxDiffBytes > reviewrun.MaxOutput {
		// A diff larger than what a run's whole output may be retained in is a
		// review that can never state a verdict this daemon could read back.
		return &ValidationError{Field: "review.max_diff_bytes", Msg: fmt.Sprintf(
			"%d exceeds the %d bytes a review run's output may be retained in", r.MaxDiffBytes, reviewrun.MaxOutput)}
	}
	return nil
}

func containsFold(set []string, want string) bool {
	for _, s := range set {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// sameReviewBinding compares two review declarations.
//
// A function rather than `==` because ReviewConfig carries slices, and a
// struct with a slice field is not comparable. Written out rather than
// reflect.DeepEqual so that a field added above forces a decision here: a new
// key that silently did not participate in the reload comparison would be one a
// reload could change under an in-flight round.
func sameReviewBinding(a, b ReviewBinding) bool {
	x, y := a.Config, b.Config
	return a.Credential == b.Credential &&
		sameStrings(a.RequiredLabels, b.RequiredLabels) &&
		x.Enabled == y.Enabled &&
		x.Principal == y.Principal &&
		x.TrackerAuthor == y.TrackerAuthor &&
		x.Controller == y.Controller &&
		x.AllowSharedTrackerController == y.AllowSharedTrackerController &&
		x.AuthSource == y.AuthSource &&
		x.APIBaseURL == y.APIBaseURL &&
		x.QueueLabel == y.QueueLabel &&
		x.AddHumanReviewLabel == y.AddHumanReviewLabel &&
		x.RoundCap == y.RoundCap &&
		x.GuidanceFile == y.GuidanceFile &&
		x.IntervalMS == y.IntervalMS &&
		x.TimeoutMS == y.TimeoutMS &&
		x.RequestTimeoutMS == y.RequestTimeoutMS &&
		x.MaxDiffBytes == y.MaxDiffBytes &&
		sameStrings(x.ReviewerArgv, y.ReviewerArgv) &&
		sameReviewerProfiles(x.ReviewerProfiles, y.ReviewerProfiles) &&
		x.ReviewerDefaultProfile == y.ReviewerDefaultProfile &&
		sameStrings(x.ReviewerEnv, y.ReviewerEnv)
}

func cloneReviewerProfiles(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	out := make(map[string][]string, len(src))
	for name, argv := range src {
		out[name] = trimAll(argv)
	}
	return out
}

func validateReviewerArgv(field string, argv []string) error {
	if len(argv) == 0 {
		return &ValidationError{Field: field, Msg: "required when review.enabled is true: a controller with no reviewer command reconciles but can open no round"}
	}
	if argv[0] == "" {
		return &ValidationError{Field: field, Msg: "names an empty command"}
	}
	return nil
}

func sameReviewerProfiles(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, argv := range a {
		other, ok := b[name]
		if !ok || !sameStrings(argv, other) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
