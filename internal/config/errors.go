package config

import (
	"errors"
	"fmt"
)

// Named load/validation errors (SPEC §5.7). Load errors are strict: startup
// with any of these refuses to start; an invalid reload keeps last-known-good
// and blocks new dispatches (hot-reload, B03).
var (
	ErrMissingWorkflowFile = errors.New("workflow file not found")
	ErrMissingFrontMatter  = errors.New("WORKFLOW.md must begin with YAML front matter delimited by --- lines")
	ErrFrontMatterNotMap   = errors.New("WORKFLOW.md front matter must be a YAML map")
	ErrEmptyPrompt         = errors.New("WORKFLOW.md prompt body is empty; there is no fallback prompt (SPEC §5.3)")
	// ErrNoCompiledPrompt refuses a render on a definition that did not come
	// from Load, which is the only thing that compiles a prompt.
	ErrNoCompiledPrompt = errors.New("workflow definition carries no compiled prompt; it did not come from config.Load")
	// ErrPublishValue refuses a `publish.value` that is not exactly one $VAR
	// reference (SPEC §5.2.8). Its own sentinel rather than a ValidationError
	// because it is the one refusal in this package whose offending value may be
	// a live credential, so it travels as data and never in the message
	// (publishValueVar).
	ErrPublishValue = errors.New("publish.value must be a $VAR reference, not a value")
)

// errCredentialAuthorityShared is the §10.2 split refusal stated over credential
// identity, exported through ErrCredentialAuthorityShared for callers asserting
// on the sentinel rather than on the detail (AGENTS.md conventions).
var errCredentialAuthorityShared = errors.New("the tracker and publish credentials are one credential")

// CredentialAuthorityError reports a workflow whose tracker and publish
// credentials resolve to one identity, however they are spelled (SPEC §10.2).
//
// It names the **authority**, which is non-secret by construction: a variable
// name, a config site, or an issuer URL with a scope and an identity. It never
// names a value, and never had one to name — the comparison is over identities a
// configuration references, never over the credentials they resolve to.
type CredentialAuthorityError struct {
	Authority string
	// TrackerName and PublishName are the `credential_sources` entries each side
	// named, or "" for a legacy spelling that compiled into an implicit source.
	TrackerName, PublishName string
}

func (e *CredentialAuthorityError) Error() string {
	return fmt.Sprintf(
		"%s is both the tracker credential (%s) and the publish credential (%s): "+
			"SPEC §10.2 keeps these separate, because the tracker credential can rewrite the queue "+
			"that dispatched the run — give the agent its own push/PR-scoped credential",
		e.Authority, credentialSourceSite(e.TrackerName), credentialSourceSite(e.PublishName))
}

func (e *CredentialAuthorityError) Unwrap() error { return errCredentialAuthorityShared }

func credentialSourceSite(name string) string {
	if name == "" {
		return "compiled from the legacy spelling"
	}
	return "credential_sources." + name
}

// UnsupportedVersionError is the clean "this file needs a newer ben"
// error (SPEC §5.2.1). It is checked before strict key validation so a
// future-version file fails on version, not on its unknown keys.
type UnsupportedVersionError struct {
	Version int
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("WORKFLOW.md declares config version %d; this ben supports version %d — upgrade ben to use this file", e.Version, SupportedVersion)
}

// MissingSecretError reports a $VAR reference that resolved to unset or empty
// (SPEC §5.5: empty resolution = missing).
type MissingSecretError struct {
	Var   string
	Field string
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("missing secret: $%s (referenced by %s) is unset or empty", e.Var, e.Field)
}

// ValidationError reports an invalid value at a dotted field path.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Msg)
}

// WorkflowError names the file a refusal came from.
//
// The invariant is **every refusal Load returns names the workflow file**, and
// there are two mechanisms for it, not one:
//
//  1. This wrapper, applied at Load's boundary, for refusals that carry no
//     location of their own — a validation error, an empty prompt, a version
//     mismatch, a YAML syntax error.
//  2. The refusal itself, for the two kinds that already say where they are: a
//     missing file, whose message *is* the path, and a template.Located error,
//     which carries file **and line** and is therefore strictly better than what
//     this would add.
//
// withPath decides which applies. Saying "Load wraps every error in one" would
// be a third wrong description of this seam in as many rounds — it is the
// *naming* that is universal, not the wrapper.
//
// A type rather than a `fmt.Errorf("%s: %w")` prefix because the path is a fact
// a caller may want rather than only text: `make workflow-check` validates two
// workflows in one run, and a script that has to substring-match the message to
// learn which one failed is a script that breaks when the message improves.
type WorkflowError struct {
	// Path is the absolute path of the workflow that refused.
	Path string
	Err  error
}

func (e *WorkflowError) Error() string { return e.Path + ": " + e.Err.Error() }

func (e *WorkflowError) Unwrap() error { return e.Err }

// Hot-reload refusals (SPEC §5.4, BUILD.md assembly decision 13).
var (
	// ErrNoRuntimeBuilder means Watch was called without the runtime builder.
	// Publishing a definition with nobody to build the adapters bound to it
	// would leave them running under a configuration their readiness check
	// never saw.
	ErrNoRuntimeBuilder = errors.New("config.Watch requires BuildRuntime: a definition and the adapters built from it are published together")
	// ErrReloadPanic reports a panic contained during reload. Startup lets
	// template-engine panics propagate; reload may not (SPEC §5.4).
	ErrReloadPanic = errors.New("panic while reloading WORKFLOW.md")
	// ErrDeploymentChanged refuses a reload that edits the §5.2.9 deployment
	// declaration. It is process-lifetime configuration: a running daemon cannot
	// have been re-launched under a different arrangement, and `attended` asserts
	// something about a human that editing a file does not make true.
	ErrDeploymentChanged = errors.New("the deployment declaration cannot be changed by reload")
	// ErrWorkOutstanding is how a Barrier says "refused for now": the rebuild
	// would change an identity the caller's outstanding work is bound to — the
	// principal holding its claims, the root its worktrees live under — so it
	// cannot be adopted until that work drains.
	//
	// Distinct from an ordinary failed reload because the file is not at fault
	// and asking again cannot help until something else changes. The watcher
	// defers such a candidate rather than rebuilding it per tick
	// (WatchOptions.Quiescent), and dispatch stays blocked meanwhile: the
	// outstanding set does not drain on its own — a blocked re-dispatch re-arms
	// without consuming an attempt, and a parked or held claim waits on a
	// human — so the operator's remedy is to resolve those claims or revert the
	// edit.
	ErrWorkOutstanding = errors.New("reload not adopted: work bound to the current configuration is still outstanding")
)
