package reviewrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Ref is the durable, sandbox-scoped address of one exact reviewer dispatch.
//
// It is internal/remote's ProcessRef restated for this consumer rather than
// reused, because the two must not be able to address each other: a coding
// attempt and a review are separate runs with separate journals, and a shared
// address type is one refactor away from a review resolving an attempt's
// idempotency record. What is shared is the *shape*, and it is the shape that
// matters — identity, sandbox, pinned profile, request digest.
type Ref struct {
	// Run is the derived review-run identity (Subject.RunID).
	Run string

	// The facts BEN owns about where this runs. They travel with the address
	// rather than beside it, for core.WorkspacePaths' reason: a field added here
	// reaches the durable record and the attach path without a seam remembering
	// to forward it. Repository, Issue and Cycle name the workspace cycle;
	// Branch, BaseSHA and TargetBranch are the publication branch, trusted base,
	// and claim-scoped pull-request target. A backend scope that carried
	// different values would move the subject out from under the review.
	Repository   string
	Issue        string
	Cycle        int64
	Branch       string
	BaseSHA      string
	TargetBranch string

	// Sandbox is the backend's opaque name for the workspace-cycle sandbox, and
	// Profile the immutable revision it was pinned at. Both are empty under the
	// local executor, which has no sandbox to name and says so rather than
	// inventing one.
	Sandbox string
	Profile string
	// Digest is the canonical digest of the Request this address was minted for.
	// Same address, different request is ErrRunMismatch.
	Digest string
}

// Remote reports whether this address names a backend sandbox.
func (r Ref) Remote() bool { return r.Sandbox != "" }

func (r Ref) String() string {
	if !r.Remote() {
		return "local/" + r.Run + "@" + r.Digest
	}
	return r.Sandbox + "/" + r.Run + "@" + r.Digest
}

// Placement is where one review runs: the sandbox the issue's workspace cycle
// already selected, plus the two facts about it BEN owns rather than the
// backend (remote.Identity).
type Placement struct {
	Branch       string
	BaseSHA      string
	TargetBranch string
	Sandbox      string
	Profile      string
}

// Complete reports whether a placement can address a backend run.
func (p Placement) Complete() bool {
	return p.Branch != "" && p.BaseSHA != "" && p.TargetBranch != "" &&
		p.Sandbox != "" && p.Profile != ""
}

// Request is the immutable reviewer invocation. It is *opaque to the
// substrate*: Airlock sees an argv, an environment and stdin bytes, and never a
// pull request, a verdict schema or a provider payload.
type Request struct {
	Argv  []string          `json:"argv"`
	Env   map[string]string `json:"env"`
	Stdin []byte            `json:"stdin"`
}

// Digest returns the canonical identity of a request. encoding/json sorts
// string map keys, so Env insertion order cannot change it (remote.ProcessRequestDigest).
func (r Request) Digest() (string, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("reviewrun: encoding the reviewer request: %w", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Chunk is one durable process event. Payload is an arbitrary byte chunk, not
// necessarily a provider record: a chunk may split a line or carry several.
// Stream identity and truncation facts are preserved through admission. Only
// stdout becomes verdict input; verdict.go then unwraps the documented Codex
// JSONL shape, when present, and validates one delimited block.
type Chunk struct {
	Seq       int64       `json:"seq"`
	Stream    ChunkStream `json:"stream"`
	Payload   []byte      `json:"payload"`
	Truncated bool        `json:"truncated,omitempty"`
}

// ChunkStream preserves which sequenced events are reviewer output and which
// are backend control records. Stdout is zero so local executors and older test
// fakes that produce plain chunks keep the natural output spelling.
type ChunkStream uint8

const (
	ChunkStdout ChunkStream = iota
	ChunkStderr
	ChunkControl
)

// State is one fresh observation of a reviewer run.
//
// Sealed, Reaped and Quiet are three independent facts and none is inferred
// from either of the others, exactly as remote.Status keeps them apart. Sealed
// says the output is complete. Quiet is both the only thing that authorizes
// starting another run in the same sandbox and a prerequisite for returning a
// verdict: sealed output cannot route a revision while descendants still run.
type State struct {
	// BackendRunID is the durable resource handle a successful start returned.
	// It is what a restart attaches by; an address alone cannot resolve an
	// unknown start (remote.ProcessBackend.Attach).
	BackendRunID string
	// Reachable is false when the executor could not ask. Every other field is
	// then meaningless and authorizes nothing.
	Reachable bool
	Sealed    bool
	Reaped    bool
	Quiet     bool
	// Profile is the profile revision the backend reports the run is pinned to,
	// when it has one. A value that differs from the pinned Ref.Profile is
	// ErrProfileDrift.
	Profile string
	// Sandbox is the sandbox the backend reports the run is in, when it has one.
	// A value that differs from Ref.Sandbox is ErrSandboxMismatch.
	Sandbox string
}

// Executor is the substrate-neutral durable reviewer-process boundary, and the
// only thing this package knows about where a model runs.
//
// Four operations, and the split between the first two is the recovery story.
// Start is keyed by the address and is idempotent for the exact request, so a
// lost response is resolved by replaying it. Attach is keyed by the backend's
// own run id, which exists only in a response that has already been received —
// so the two are not interchangeable, and an implementation that made Attach
// accept an address would be one that could resolve an unknown start by
// guessing.
type Executor interface {
	// Digest returns the canonical request digest this executor addresses a
	// dispatch by, given the rest of the address and the request.
	//
	// It belongs to the executor rather than to Request because the *address*
	// does: a backend keys idempotency over its own request shape — argv and
	// environment plus the workspace identity and the limits it will enforce —
	// and a digest BEN computed over a narrower struct would be an address the
	// backend does not recognize. The ref passed here carries every field except
	// Digest itself.
	Digest(ref Ref, req Request) (string, error)
	// Start dispatches the exact request at this address, idempotently. Called
	// twice with the same (ref, req) it MUST resolve the same run rather than
	// creating a second. A different request at the same address is
	// ErrRunMismatch.
	Start(ctx context.Context, ref Ref, req Request) (State, error)
	// Attach resolves a run by the backend id a successful Start returned.
	// ErrNoRun when the executor holds none — a fact, as against a failed read.
	Attach(ctx context.Context, ref Ref, backendRunID string) (State, error)
	// Events returns durable output chunks strictly after the cursor, in
	// ascending sequence order. An empty successful result says only that
	// nothing new is available; it is never itself evidence of sealing.
	Events(ctx context.Context, ref Ref, after int64) ([]Chunk, error)
	// Status is a fresh observation and never a cached one.
	Status(ctx context.Context, ref Ref) (State, error)
}

// StartReplayer is the additional capability required to recover a dispatch
// whose durable mark exists but whose Start response does not. A remote
// backend can replay its persisted idempotency key across processes; the local
// executor intentionally cannot. Keeping this separate from Executor prevents
// a restarted daemon from treating process-local idempotence as durable.
type StartReplayer interface {
	ReplayStart(ctx context.Context, ref Ref, req Request) (State, error)
}

// Two credential lists, because the two failures they close are different and
// so are the substrates they apply on.
//
// forbiddenEnv is what a reviewer may never hold on *either* substrate: the
// forge credentials #11 already named, plus the Airlock service credential,
// which never leaves the trusted process at all. Every one of them is authority
// over the very artifacts the review is supposed to be advisory about.
//
// providerEnv is the reviewer's own model credential. It is not forbidden
// outright — a reviewer that cannot reach a model states nothing, and a rule
// nobody can deploy under is a rule that gets removed — but it is refused
// wherever it would become *reusable somewhere BEN cannot see*. Concretely: it
// may be copied into a local child by explicit operator allowlist, and it may
// never be serialized into a backend request, because a sandbox already
// authenticates its own model calls from its profile and a key in the request
// would be a second, exportable copy.
var (
	forbiddenEnv = []string{
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GITHUB_API_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"BEN_REVIEW_TOKEN",
		"BEN_AIRLOCK_TOKEN",
	}
	providerEnv = []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
	}
)

// ForbiddenEnv and ProviderEnv return those sets. Copies: an exported slice is
// a mutable global, and these decide what a model process may hold.
func ForbiddenEnv() []string { return append([]string(nil), forbiddenEnv...) }
func ProviderEnv() []string  { return append([]string(nil), providerEnv...) }

// CheckEnvName refuses one variable name the reviewer must never be given, on
// any substrate.
func CheckEnvName(name string) error {
	for _, bad := range forbiddenEnv {
		if strings.EqualFold(strings.TrimSpace(name), bad) {
			return fmt.Errorf("%w: %s is a forge or backend credential, and the reviewer publishes nothing itself", ErrCredentialLeak, name)
		}
	}
	return nil
}

// CheckRemoteEnvName additionally refuses a provider credential, which is what
// a request crossing to a sandbox may not carry.
func CheckRemoteEnvName(name string) error {
	if err := CheckEnvName(name); err != nil {
		return err
	}
	for _, bad := range providerEnv {
		if strings.EqualFold(strings.TrimSpace(name), bad) {
			return fmt.Errorf("%w: %s is a reusable provider credential, and a sandbox authenticates its "+
				"model calls from its own profile", ErrCredentialLeak, name)
		}
	}
	return nil
}

// CheckRequest refuses a composed request that carries a forbidden credential,
// by name or by value.
//
// The value check is the one that matters and is why this is not merely a
// review of the allowlist. #204 asks for a test that inspects the *actual
// serialized request*; the same predicate the test asserts is applied in
// production, so the property is enforced rather than merely observed. Secrets
// are compared by exact value against what the trusted process holds, which is
// the only comparison available that does not itself require knowing what a
// secret looks like.
//
// remote widens the name rule to providerEnv, and callers pass what they are:
// a request bound for a backend cannot carry a model key, a local child's may
// if an operator named it.
func CheckRequest(req Request, secrets []string, remote bool) error {
	check := CheckEnvName
	if remote {
		check = CheckRemoteEnvName
	}
	for name := range req.Env {
		if err := check(name); err != nil {
			return err
		}
	}
	// Every field the request can carry a value in, as text. The marshalled body
	// alone would not do: encoding/json base64s a []byte, so a credential pasted
	// into the prompt on stdin would not appear in it as itself — which is
	// exactly the leak most worth catching, since the prompt is the one field
	// assembled from several sources.
	carried := append([]string(nil), req.Argv...)
	for name, value := range req.Env {
		carried = append(carried, name, value)
	}
	carried = append(carried, string(req.Stdin))
	if body, err := json.Marshal(req); err == nil {
		carried = append(carried, string(body))
	} else {
		return fmt.Errorf("reviewrun: encoding the reviewer request: %w", err)
	}

	for _, secret := range secrets {
		// A short or empty value is not a credential and would match everything.
		if len(strings.TrimSpace(secret)) < 8 {
			continue
		}
		for _, field := range carried {
			if strings.Contains(field, secret) {
				return fmt.Errorf("%w: the serialized reviewer request carries a value the trusted process holds", ErrCredentialLeak)
			}
		}
	}
	return nil
}
