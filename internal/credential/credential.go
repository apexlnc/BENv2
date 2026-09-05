// Package credential implements the `credential_sources` kinds (SPEC §5.2,
// §5.7, §10.2): the closed registered set behind "every tracker, base-fetch and
// publish credential is obtained from a credential source at the moment it is
// needed".
//
// Three kinds ship. `octo_sts` exchanges a workload identity for a short-lived
// GitHub credential and states a deadline; `projected_oidc` presents a bounded
// workload token directly after checking the identity its claims address; and
// `static` names a daemon-environment variable, is explicitly unbounded, and is
// what every legacy spelling compiles into so that there is exactly one runtime
// treatment (SPEC §8, amendment 9).
//
// Every kind's Describe is PURE — no network, no filesystem, no instance — which
// is what lets `make workflow-check` validate a workload-identity configuration
// on a host holding no credential at all (SPEC §5.7, amendment 4).
package credential

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The named refusals a source schema produces. Each is a load-time refusal
// (SPEC §5.7), asserted on by sentinel rather than by message text
// (AGENTS.md conventions).
var (
	// ErrUnknownSourceKey refuses a key the kind's schema does not name.
	// Strict-at-load applies inside a source block for the reason it applies
	// everywhere else, and sharper: a typo in `oidc_token_path` that silently
	// left it unset is precisely how a partial configuration degrades to a
	// static token.
	ErrUnknownSourceKey = errors.New("credential source: unknown key")
	// ErrMissingSourceField refuses a required field that is absent or blank
	// after trimming.
	ErrMissingSourceField = errors.New("credential source: required field is missing or blank")
	// ErrSourceFieldNotLiteral refuses a field that must be a literal scalar and
	// carries a $VAR reference or an interpolation instead.
	ErrSourceFieldNotLiteral = errors.New("credential source: field must be a literal, not a $VAR reference")
	// ErrSourceFieldType refuses a field whose YAML type is not a string.
	ErrSourceFieldType = errors.New("credential source: field must be a string")
	// ErrSourceValueNotReference refuses a `static` value that is not exactly
	// one $VAR reference.
	ErrSourceValueNotReference = errors.New("credential source: value must be exactly one $VAR reference")
	// ErrSourceURL refuses a URL that is unparseable, names no host, or carries
	// a component that addresses nothing (userinfo, a query, a fragment).
	ErrSourceURL = errors.New("credential source: url is not a usable endpoint")
	// ErrSourceURLScheme refuses a scheme other than https.
	//
	// Refused rather than warned about, and refused at *load* — the same posture
	// `projected_oidc`'s issuer and `internal/airlock`'s base URL take, for the
	// same reason. An exchange presents a projected workload-identity JWT in an
	// `Authorization: Bearer` header, and that JWT federates to GitHub write
	// access: an on-path observer who captures one can replay it to the real
	// issuer for the whole of its life. There is no deployment in which sending
	// it in the clear is a considered choice, and a warning is a refusal nobody
	// reads.
	ErrSourceURLScheme = errors.New("credential source: url scheme must be https")
	// ErrSourceKindUnknown refuses a `kind` with no registered implementation.
	ErrSourceKindUnknown = errors.New("credential source: unknown kind")
)

// envVarRe is §5.5's reference syntax. Spelled here rather than imported from
// internal/config because the dependency runs the other way — the loader asks
// the registry, which asks a kind — and a kind that imported the loader would
// close that loop. The two are pinned equal by
// TestSourceReferenceSyntaxMatchesTheLoader.
var envVarRe = regexp.MustCompile(`\$([A-Z_][A-Z0-9_]*)`)

// oneReferenceRe is a value that is exactly one reference and nothing else.
var oneReferenceRe = regexp.MustCompile("^" + envVarRe.String() + "$")

// requireLiteral reads a required field that MUST be a literal scalar.
//
// "Literal" is the whole point of the rule, not a detail of it: an `octo_sts`
// block names an issuer, a policy namespace, an identity and a path, all four of
// which are non-secret and are printed in full (SPEC §5.5, amendment 3). A $VAR
// there would make one of them invisible in `config effective` while changing
// which trust policy the daemon exchanges against.
func requireLiteral(block map[string]any, kind, key string) (string, error) {
	v, present := block[key]
	if !present || v == nil {
		return "", fmt.Errorf("%w: %s requires %q", ErrMissingSourceField, kind, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s.%s must be a string, got %T", ErrSourceFieldType, kind, key, v)
	}
	if envVarRe.MatchString(s) {
		// The value is not shown: a field that was supposed to be a literal and
		// holds a reference is still a field an operator can mistype a secret
		// into, and this refusal is pasted into CI logs.
		return "", fmt.Errorf("%w: %s.%s must be a literal scalar; the value is not shown", ErrSourceFieldNotLiteral, kind, key)
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", fmt.Errorf("%w: %s requires a non-blank %q", ErrMissingSourceField, kind, key)
	}
	return s, nil
}

// checkKeys refuses any key the schema does not name. `kind` is always allowed:
// it is the discriminator that selected the schema.
func checkKeys(block map[string]any, kind string, allowed ...string) error {
	for k := range block {
		if k == "kind" {
			continue
		}
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s does not define %q (known keys: kind, %s)",
				ErrUnknownSourceKey, kind, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// permanent wraps err as a permanent credential failure attributed to authority.
func permanent(authority string, err error) error {
	return &core.CredentialError{Class: core.CredentialPermanent, Authority: authority, Err: err}
}

// transient wraps err as a transient credential failure attributed to authority.
func transient(authority string, err error) error {
	return &core.CredentialError{Class: core.CredentialTransient, Authority: authority, Err: err}
}
