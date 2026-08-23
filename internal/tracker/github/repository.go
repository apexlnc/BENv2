package github

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// cloneUsername is the username half of the fetch credential. GitHub ignores
// it on an HTTPS PAT — "x-access-token" is the documented placeholder — and
// keeping the secret strictly in the password half is what lets the workspace
// redact exactly one value from git's output (SPEC §10.2).
const cloneUsername = "x-access-token"

// The adapter is the only component that knows which repository BEN's issues
// live in, so it is the one that answers for the base clone (SPEC §6.2) — and
// the only one that knows which account its claims name — configured explicitly,
// or authenticated by its credential as the fallback (SPEC §8.4).
var (
	_ core.RepositorySource     = (*Adapter)(nil)
	_ core.ClaimPrincipalSource = (*Adapter)(nil)
)

// Repository names the repository this adapter's issues live in and the
// credential that fetches it (SPEC §6.2, §10.2: the tracker credential "also
// authenticates base-clone git fetch").
//
// Both halves come from Ready and nowhere else. The URL is the `clone_url`
// GitHub itself reported for the repository — never a URL derived from
// `api_url`, because a GitHub Enterprise install's API host and clone host are
// independently chosen and any derivation rule can point the credential at a
// different server. The credential is the one readiness resolved, so the
// $GITHUB_TOKEN fallback is read in exactly one place (SPEC §5.7, §5.8).
//
// Before a successful Ready there is nothing to report and no probe to make
// here: an unready adapter is ErrNotReady, not an unauthenticated remote or a
// guess.
func (a *Adapter) Repository(ctx context.Context) (core.Repository, error) {
	a.mu.Lock()
	clone := a.cloneURL
	a.mu.Unlock()
	if clone == "" {
		return core.Repository{}, ErrNotReady
	}
	// The **source**, not a value. §10.2 gives the daemon's base fetch the
	// tracker credential, and assembly hands the workspace this same instance
	// rather than a second one of its own — so the two share a cache, a
	// deadline and an authority, and there is exactly one thing to rotate.
	//
	// The username half rides along because it is this adapter's to choose: it
	// is a GitHub placeholder, and keeping the secret strictly in the password
	// half is what lets the workspace redact exactly one value.
	return core.Repository{RemoteURL: clone, AuthSource: remoteAuthSource{src: a.cred}}, nil
}

// remoteAuthSource adapts the tracker credential to the shape git wants,
// obtained immediately before each remote invocation (SPEC §6.2, amendment 6).
type remoteAuthSource struct{ src core.Source }

var _ core.RemoteAuthSource = remoteAuthSource{}

func (s remoteAuthSource) Auth(ctx context.Context) (core.RemoteAuth, error) {
	tok, err := s.src.Fetch(ctx, core.PurposeWorkspace)
	if err != nil {
		return core.RemoteAuth{}, err
	}
	if tok.Value == "" {
		// Before git is invoked at all. An unauthenticated fetch against a
		// private repository would fail anyway, but it would fail as a git
		// error nobody could classify, and against a *public* one it would
		// succeed — which is the fallthrough this refusal exists to prevent.
		return core.RemoteAuth{}, emptyCredential(s.src)
	}
	return core.RemoteAuth{Username: cloneUsername, Password: tok.Value}, nil
}

// ClaimPrincipal is the login this adapter's claims are assigned to (SPEC §8.4)
// — the configured `claim_assignee`, or the account behind the credential Ready
// resolved when that key is absent. This is why a rebuilt tracker can be a
// different principal while an ordinary same-account rotation is not.
//
// It reads what readiness *published*, not the earlier value `principal` holds.
// A configured account is present there from construction; the fallback login
// arrives before the repository probe. In both cases readiness can still fail —
// no rate budget, an invisible repository, an unassignable account, a server
// that answered 500. Answering from that earlier value would hand assembly a
// principal for a tracker nothing proved usable, and the claim written under it
// would be the first thing to discover that (SPEC §5.7).
//
// The returned identity is normalized to lowercase at publication. The private
// `principal` retains the configured or API spelling used in GitHub writes, so
// this value is an identity key rather than necessarily their literal payload.
func (a *Adapter) ClaimPrincipal(ctx context.Context) (string, error) {
	a.mu.Lock()
	principal := a.readyPrincipal
	a.mu.Unlock()
	if principal == "" {
		return "", ErrNotReady
	}
	return principal, nil
}
