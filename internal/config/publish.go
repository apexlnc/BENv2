package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The `publish` block's rules (SPEC §5.2.8).
//
// Split from the adapter's half deliberately, by who can know the answer. Every
// rule here is core-knowable — a closed kind, a child variable's shape, the
// reserved names core itself owns — so each refuses in Load, which is stricter
// than waiting for a Structural that Load never calls (SPEC §5.7). The two rules
// that need the adapter's credential table are the collisions between
// `publish.env` and an adapter-owned variable, and between it and the block's
// generic environment surfaces; those live behind the adapter's Structural,
// where the table is (harness.CheckOwnedEnv, harness.CheckPublishEnv).
//
// One rule, two sites — not two mechanisms: every child environment variable has
// exactly one owning config site, which is the site where its value is validated.

// publishValueRe anchors §5.5's reference syntax: `publish.value` is exactly one
// `$VAR` and nothing else. Built from envVarRe rather than spelled again, so the
// character class this accepts cannot drift from the one $VAR resolution
// recognizes everywhere else — a value this accepted and that did not would be a
// credential no environment lookup could ever find.
var publishValueRe = regexp.MustCompile("^" + envVarRe.String() + "$")

// publishEnvRe is what an environment variable name may look like. Checked
// because the composed child environment is a list of `NAME=value` entries
// (harness.Environ): a name carrying `=`, whitespace, or a leading digit
// produces an entry no child can read back, and the credential would be silently
// absent at the publish step rather than refused here.
var publishEnvRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// publishValueVar extracts the variable name from `publish.value`.
//
// The refusal carries the offending value as **data** and never in the message
// (core.ConfigValueError), which matters more here than anywhere else in this
// package: the one thing an operator writes in this field by mistake is the
// credential itself, and a loader that echoed it would put a live token in the
// CI log of the run that refused it. Everything else this file refuses — a kind,
// a variable name — is named in its message, because neither can be a secret.
// Not trimmed first. §5.2.8 says exactly one reference "and nothing else", and
// surrounding whitespace is something else: a quoted `" $GH_TOKEN "` is a
// deliberate spelling, and silently rewriting it to the reference would make the
// loader lenient about the one field the rule exists to keep exact. YAML already
// strips whitespace from a plain scalar, so an ordinary `value: $GH_TOKEN` never
// reaches here padded — only a quoted one does, and that one is refused.
func publishValueVar(raw string) (string, error) {
	m := publishValueRe.FindStringSubmatch(raw)
	if m == nil {
		return "", &core.ConfigValueError{
			Field: "publish.value",
			Value: raw,
			Err: fmt.Errorf("%w: must be exactly one $VAR reference (e.g. $GH_TOKEN) and nothing else — "+
				"not a literal credential, which would live in a repo-owned file and in every "+
				"`config effective` rendering of it, and not an interpolation, which would make one "+
				"token the concatenation of several secrets. The value is not shown", ErrPublishValue),
		}
	}
	return m[1], nil
}

// publishKindRequired is the refusal for a `publish` key that states no kind.
//
// Shared, because two stages reach it and they must not word it differently:
// resolve refuses a *written* block with no kind, which is the only place the
// distinction between written and omitted survives (load.go), and this file
// refuses a block whose other fields say it was meant.
func publishKindRequired() error {
	return &ValidationError{Field: "publish.kind", Msg: fmt.Sprintf(
		"required once a publish block is written (one of %s); omit the whole block to inject no publish credential",
		strings.Join(publishKinds, ", "))}
}

// validatePublish enforces the block's core-owned rules (SPEC §5.2.8). The
// section is OPTIONAL: an absent block is not a misconfiguration, it means BEN
// injects no publish credential and the agent authenticates from what §7.6's
// allowlist already carries.
//
// An empty kind therefore means the key was omitted, and only that. Every written
// spelling that decodes to an empty one is already refused before this runs —
// `publish:` by the lenient shape pass, and `{}` / `{kind: ""}` / `{env: ""}` by
// resolve's presence ⇒ kind rule, which is the last point where "was the key
// written?" is still answerable. Inferring absence from all-zero fields here was
// exactly wrong: it made every all-zero written block load and inject nothing.
func validatePublish(p PublishConfig) error {
	if p.Kind == "" {
		if p.Env != "" || p.ValueVar != "" || p.Source != "" {
			// Unreachable through Load, which refuses this in resolve. Kept because
			// this function is the field rules' home and a caller reaching it with a
			// half-written block must not be told the block is absent.
			return publishKindRequired()
		}
		return nil
	}
	if !slices.Contains(publishKinds, p.Kind) {
		return &ValidationError{Field: "publish.kind", Msg: fmt.Sprintf(
			"%q is not a supported publish credential kind (supported: %s)", p.Kind, strings.Join(publishKinds, ", "))}
	}
	if err := validatePublishEnv(p.Env); err != nil {
		return err
	}
	// Per kind, because the fields a kind needs are the kind's own: a
	// github_app (#117 boundary 2) needs a signing key rather than a value, and
	// a switch here is where that arrives.
	//
	// Each kind refuses the *other* kind's field too. A block naming both a
	// variable and a source entry is two config sites feeding one credential,
	// which is the shape where one of them is silently doing nothing — the same
	// rule the tracker block states for `token` beside `credential_source`.
	switch p.Kind {
	case PublishKindToken:
		if p.ValueVar == "" {
			return &ValidationError{Field: "publish.value", Msg: fmt.Sprintf(
				"required for kind %q: name the variable holding the credential, e.g. $GH_TOKEN", PublishKindToken)}
		}
		if p.Source != "" {
			return &ValidationError{Field: "publish.source", Msg: fmt.Sprintf(
				"is not read under kind %q, which takes its credential from `value`; write kind %q to name a credential source",
				PublishKindToken, PublishKindSource)}
		}
	case PublishKindSource:
		if p.Source == "" {
			return &ValidationError{Field: "publish.source", Msg: fmt.Sprintf(
				"required for kind %q: name a `credential_sources` entry", PublishKindSource)}
		}
		if p.ValueVar != "" {
			return &ValidationError{Field: "publish.value", Msg: fmt.Sprintf(
				"is not read under kind %q, which takes its credential from the named source; write kind %q to name a variable",
				PublishKindSource, PublishKindToken)}
		}
	}
	return nil
}

// validatePublishEnv refuses a child variable the publish credential may not
// own (SPEC §5.2.8, §7.6).
//
// Both reserved sets are core's own, which is why this is a load refusal rather
// than an adapter's: `BEN_` is the orchestrator's namespace, and the allowlist is
// the list §7.6 copies into every child. The asymmetry with what
// `agent.provider.env` may spell is deliberate — an operator setting `HOME` to a
// path is choosing where the harness looks, while naming `HOME` here sets it to a
// *credential*, which points both harnesses' stored-credential lookup at a token
// and is never a meaningful configuration.
func validatePublishEnv(name string) error {
	switch {
	case name == "":
		return &ValidationError{Field: "publish.env", Msg: "required: name the child environment variable the credential is injected as, e.g. GH_TOKEN"}
	case !publishEnvRe.MatchString(name):
		return &ValidationError{Field: "publish.env", Msg: fmt.Sprintf(
			"%q is not a usable environment variable name (letters, digits and underscore, not starting with a digit)", name)}
	case strings.HasPrefix(name, core.EnvPrefix):
		return &ValidationError{Field: "publish.env", Msg: fmt.Sprintf(
			"%q uses the %s prefix, which is reserved to the orchestrator (SPEC §7.6)", name, core.EnvPrefix)}
	case slices.Contains(core.EnvAllowlist, name):
		return &ValidationError{Field: "publish.env", Msg: fmt.Sprintf(
			"%q is on SPEC §7.6's daemon-environment allowlist, which is copied into every child: "+
				"injecting a credential under it would overwrite a variable the harness resolves its own "+
				"stored credential from", name)}
	}
	return nil
}
