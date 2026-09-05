package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Choosing which verifier judges an attempt.
//
// There are two now, and they are not interchangeable: the local one reads a
// worktree and a base repository on the daemon's disk, the remote one reads a
// pin, a canonical remote and a forge. Pointing either at the other's attempt is
// not a degradation but a hole. The local checker aimed at a remote attempt
// would read a workspace the daemon prepared and the run never touched — a
// branch still at its base, or worse, a branch some *other* attempt left there —
// and the remote checker aimed at a local attempt would ask a fact source for a
// claim nothing ever pinned.
//
// So selection is explicit, total, and refuses rather than falls back. It is
// also deliberately pure: nothing here reads the world, so the one decision that
// must not be influenced by a run cannot be.

// ErrNoLocalVerifier refuses a local attempt with no local checker configured.
var ErrNoLocalVerifier = errors.New("verify: a local attempt has no local verifier configured")

// ErrAmbiguousSubstrate refuses an attempt that names both substrates. A run
// happened in exactly one place, and a caller that cannot say which has not
// established the fact every leg of either check is measured against.
var ErrAmbiguousSubstrate = errors.New("verify: an attempt names both a local workspace and a remote run")

// Attempt is one finished attempt, as much of it as choosing a verifier needs.
//
// Remote is a pointer and its absence is the default, which is the v1 behaviour
// this ticket must not change: an attempt says nothing about a remote run unless
// something explicitly made it a remote run.
type Attempt struct {
	Issue core.Issue
	// Workspace is the local workspace, for an attempt this daemon hosted.
	Workspace core.Workspace
	// Remote names the claim, run and verification of an attempt that happened
	// on a v2 remote substrate. Nil for every local attempt.
	Remote *core.RemoteRunRef
}

// Publication is the one question the loop asks about a finished attempt, with
// the attempt already bound: has this run's work actually been published
// (SPEC §9.7)?
//
// One shape over both substrates, so that routing (§9.6) is written once and
// cannot acquire a branch that treats a remote verdict differently from a local
// one. What differs between them is *what counts as evidence*, and that is
// settled before this interface is reached.
type Publication interface {
	Verify(ctx context.Context) (PublicationResult, error)
}

// PublicationResult keeps the routing shape common to both substrates while
// retaining the bounded evidence record a remote verdict needs for audit.
// RemoteFacts is zero for a local publication and the exact independently read
// facts for a remote one.
type PublicationResult struct {
	Result
	RemoteFacts core.RemotePublishFacts
}

// SelectPublication binds an attempt to the verifier that may judge it.
//
// The remote verifier is selected only for an attempt that explicitly names a
// remote run, and a remote attempt is never verified by the local one: a missing
// remote verifier is ErrNoRemoteVerifier, not a fallback. That refusal is the
// whole point of this function existing rather than being a two-line branch at
// the call site — the fallback is the failure mode, and it is the kind that
// looks like a sensible default right up until it verifies a sandbox against
// itself.
func SelectPublication(local *Checker, remote *RemoteChecker, a Attempt) (Publication, error) {
	if a.Issue.Identifier == "" {
		return nil, errors.New("verify: an attempt to verify must name its issue")
	}
	if a.Remote == nil {
		if local == nil {
			return nil, fmt.Errorf("%w: issue %s", ErrNoLocalVerifier, a.Issue.Identifier)
		}
		return localPublication{checker: local, issue: a.Issue, ws: a.Workspace}, nil
	}
	if a.Workspace != (core.Workspace{}) {
		return nil, fmt.Errorf("%w: issue %s names remote run %s and local workspace %s",
			ErrAmbiguousSubstrate, a.Issue.Identifier, a.Remote.Run, a.Workspace.Key)
	}
	if remote == nil {
		return nil, fmt.Errorf("%w: issue %s, run %s", ErrNoRemoteVerifier, a.Issue.Identifier, a.Remote.Run)
	}
	return remotePublication{checker: remote, issue: a.Issue, run: *a.Remote}, nil
}

type localPublication struct {
	checker *Checker
	issue   core.Issue
	ws      core.Workspace
}

func (p localPublication) Verify(ctx context.Context) (PublicationResult, error) {
	result, err := p.checker.Verify(ctx, p.issue, p.ws)
	return PublicationResult{Result: result}, err
}

type remotePublication struct {
	checker *RemoteChecker
	issue   core.Issue
	run     core.RemoteRunRef
}

func (p remotePublication) Verify(ctx context.Context) (PublicationResult, error) {
	result, err := p.checker.Verify(ctx, p.issue, p.run)
	return PublicationResult{Result: result.Result(), RemoteFacts: result.Facts}, err
}
