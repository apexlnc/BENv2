package reviewrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Remote runs the reviewer as one durable process in the issue's
// workspace-cycle sandbox, over #192's `remote.ProcessBackend`.
//
// It is a *translation* and deliberately nothing more. Every recovery property
// #204 asks for — idempotent start, lost-response replay, attach by backend run
// id, durable ordered events after a cursor, three independent lifecycle facts
// — is already the contract that boundary states, and reimplementing any of it
// here would be a second answer to a question that has one. What this type adds
// is the mapping between a review's address and a sandbox-scoped process
// address, plus the one refusal that mapping makes possible: a review is never
// dispatched into a sandbox its workspace cycle did not select.
//
// It holds no Airlock client and no credential. The backend it is given is
// already authenticated by the trusted process (cmd/ben), which is what keeps
// "Airlock service authentication stays in the trusted process" a structural
// fact rather than a convention.
type Remote struct {
	backend    remote.ProcessBackend
	limits     core.RunLimits
	repository string
	log        *slog.Logger
}

// RemoteOptions are what a backend-executed reviewer is constructed from.
type RemoteOptions struct {
	// Backend is the durable foreign-process boundary, already scoped to a
	// tenant and authenticated.
	Backend remote.ProcessBackend
	// GitRepository is the forge owner/name authorized for the review phase.
	// Ref.Repository remains the distinct durable workspace-cycle identity.
	GitRepository string
	// Limits cross the boundary because the backend must enforce both deadlines
	// while BEN is disconnected; reconnecting must not reset either
	// (docs/REMOTE.md).
	Limits core.RunLimits
	Logger *slog.Logger
}

// NewRemote validates the seam and builds the executor.
func NewRemote(opts RemoteOptions) (*Remote, error) {
	if opts.Backend == nil {
		return nil, fmt.Errorf("reviewrun: a remote reviewer needs a durable process backend")
	}
	if opts.GitRepository == "" {
		return nil, fmt.Errorf("reviewrun: a remote reviewer needs a trusted forge repository")
	}
	r := &Remote{backend: opts.Backend, limits: opts.Limits, repository: opts.GitRepository, log: opts.Logger}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// Digest is the backend's own canonical dispatch digest, over the whole process
// spec: the identity the run executes in and the limits it enforces are part of
// the request there, so a digest that omitted them would name a different
// dispatch than the one BEN is about to make.
func (r *Remote) Digest(ref Ref, req Request) (string, error) {
	_, spec, err := r.address(ref, req)
	if err != nil {
		return "", err
	}
	return remote.ProcessRequestDigest(spec)
}

func (r *Remote) Start(ctx context.Context, ref Ref, req Request) (State, error) {
	pref, spec, err := r.address(ref, req)
	if err != nil {
		return State{}, err
	}
	st, err := r.backend.Start(ctx, pref, spec)
	if err != nil {
		return State{}, translateStart(err)
	}
	return r.translate(ref, st), nil
}

// translateStart restates the backend's one definite start answer in this
// package's vocabulary. Every other error crosses unchanged: it is the
// ambiguity the session resolves by replaying the same address, and naming it
// here would be a second answer to a question the boundary already answers.
func translateStart(err error) error {
	var refused *remote.ProcessRefusal
	if !errors.As(err, &refused) {
		return err
	}
	return &RefusedError{Reason: refused.Code, Detail: refused.Message, Cause: err}
}

// ReplayStart resolves the same backend idempotency address after an
// ambiguous response, including across a BEN restart.
func (r *Remote) ReplayStart(ctx context.Context, ref Ref, req Request) (State, error) {
	return r.Start(ctx, ref, req)
}

func (r *Remote) Attach(ctx context.Context, ref Ref, backendRunID string) (State, error) {
	pref, _, err := r.address(ref, Request{})
	if err != nil {
		return State{}, err
	}
	st, err := r.backend.Attach(ctx, pref, backendRunID)
	if err != nil {
		return State{}, err
	}
	return r.translate(ref, st), nil
}

func (r *Remote) Status(ctx context.Context, ref Ref) (State, error) {
	pref, _, err := r.address(ref, Request{})
	if err != nil {
		return State{}, err
	}
	st, err := r.backend.Status(ctx, pref)
	if err != nil {
		return State{}, err
	}
	return r.translate(ref, st), nil
}

func (r *Remote) Events(ctx context.Context, ref Ref, after int64) ([]Chunk, error) {
	pref, _, err := r.address(ref, Request{})
	if err != nil {
		return nil, err
	}
	envelopes, err := r.backend.Events(ctx, pref, remote.Cursor(after))
	if err != nil {
		return nil, err
	}
	out := make([]Chunk, 0, len(envelopes))
	for _, e := range envelopes {
		stream := ChunkControl
		switch e.Stream {
		case remote.StreamStdout:
			stream = ChunkStdout
		case remote.StreamStderr:
			stream = ChunkStderr
		case remote.StreamControl:
		default:
			return nil, fmt.Errorf("reviewrun: remote event %d names unknown stream %s", e.Seq, e.Stream)
		}
		// Control records and stderr advance the durable cursor but never become
		// model text. Only stdout may state a verdict, while a backend's
		// output.truncated fact survives translation as an unconditional refusal.
		out = append(out, Chunk{
			Seq: e.Seq, Stream: stream, Payload: append([]byte(nil), e.Payload...), Truncated: e.Truncated,
		})
	}
	return out, nil
}

// address maps a review address onto a sandbox-scoped process address.
//
// The identity is assembled from what BEN owns (the cycle, the branch, the
// trusted base) plus what the backend owns (the sandbox id and its immutable
// profile revision). remote.Identity.Complete refuses anything less, which is
// the refusal that makes "a review never runs in a sandbox nobody selected for
// it" a compile-and-check property rather than a comment.
func (r *Remote) address(ref Ref, req Request) (remote.ProcessRef, remote.ProcessSpec, error) {
	if !ref.Remote() {
		return remote.ProcessRef{}, remote.ProcessSpec{},
			fmt.Errorf("%w: %s names no sandbox", ErrSandboxMismatch, ref.Run)
	}
	if ref.TargetBranch == "" {
		return remote.ProcessRef{}, remote.ProcessSpec{},
			fmt.Errorf("%w: %s names no claim-scoped target branch", ErrSandboxMismatch, ref.Run)
	}
	id := remote.Identity{
		Claim: remote.Claim{
			Repository: ref.Repository,
			Issue:      ref.Issue,
			// The *approval* anchor, never the assignment: a review and the
			// revision that follows it belong to one workspace cycle and one
			// sandbox, and keying this by the claim epoch would move the address
			// every time the controller handed the claim back (remotews).
			Epoch: ref.Cycle,
		},
		Branch:          ref.Branch,
		BaseSHA:         ref.BaseSHA,
		SandboxID:       ref.Sandbox,
		ProfileRevision: ref.Profile,
	}
	if !id.Complete() {
		return remote.ProcessRef{}, remote.ProcessSpec{},
			fmt.Errorf("%w: %s does not completely name a workspace cycle sandbox", ErrSandboxMismatch, ref.Run)
	}
	pref := remote.ProcessRef{
		Identity: id,
		// The review's own run id, which is derived from the role among other
		// things — so the coding attempt, this review and the revision that
		// follows are three addresses in one sandbox rather than one contended
		// address (Subject.RunID).
		RunID:         remote.RunID(ref.Run),
		RequestDigest: ref.Digest,
	}
	spec := remote.ProcessSpec{
		Identity: id,
		Argv:     req.Argv,
		Env:      req.Env,
		Stdin:    req.Stdin,
		Limits:   r.limits,
		Git: remote.GitScope{
			Phase: remote.GitPhaseReview, Repository: r.repository,
			Branch: ref.Branch, BaseCommit: ref.BaseSHA, BaseBranch: ref.TargetBranch,
		},
	}
	return pref, spec, nil
}

// translate projects the backend's three independent facts onto this package's.
//
// None is inferred from either of the others, and Quiet in particular is read
// through remote.MayReuse rather than off the diagnostic phase: the only safe
// reading of "may another agent start here" is a positive attestation from a
// reachable backend (SPEC §9.8).
func (r *Remote) translate(ref Ref, st remote.Status) State {
	return State{
		BackendRunID: st.BackendRunID,
		Reachable:    st.Reachable,
		Sealed:       st.Reachable && st.Stream == remote.StreamStateSealed,
		Reaped:       st.Reaped(),
		Quiet:        remote.MayReuse(st),
		// The backend does not restate the sandbox or profile per status, so the
		// pinned ones are echoed: a drift is detected where it can be, which is
		// the workspace strategy's acquire (remotews) and this package's resume
		// comparison against the record.
		Profile: ref.Profile,
		Sandbox: ref.Sandbox,
	}
}
