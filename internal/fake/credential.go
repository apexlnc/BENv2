package fake

import (
	"context"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The credential-source fakes (SPEC §5.2, §10.2).
//
// Here rather than in each consumer's tests because four packages need the same
// three shapes — a source that answers, one that fails with a stated class, and
// one whose deadline is too short — and four private copies would be four
// chances to build a fake that promises something no real source does. Per
// AGENTS.md, a fake's fidelity to the thing it stands in for is a correctness
// concern: what these model is exactly `internal/credential`'s contract, which
// is that a *bounded* source states a deadline and an unbounded one leaves it
// zero, and that every failure carries a class and an authority.

// Source is a scripted core.CredentialSource.
//
// The zero value answers with an empty token under an empty descriptor, which is
// a source defect rather than a convenience: every consumer refuses an empty
// value at its own boundary, and a fake that quietly supplied one would hide
// exactly the refusal those tests are about.
type Source struct {
	// Descriptor is what Descriptor() returns. A zero MinFreshTTL means
	// explicitly unbounded, exactly as it does for a real source.
	Descriptor_ core.SourceDescriptor
	// Value is the credential handed back. Empty is meaningful: it is what a
	// defective source does.
	Value string
	// UsableUntil is the deadline. Zero pairs with an unbounded descriptor;
	// TTL is the alternative spelling, measured from each fetch.
	UsableUntil time.Time
	// TTL, when nonzero, makes each answer's deadline `now + TTL` — the shape a
	// real exchange produces, where the clock starts when the token is minted.
	TTL time.Duration
	// Err is returned instead of a token. Set it to a *core.CredentialError to
	// exercise a class.
	Err error

	mu sync.Mutex
	// Fresh and Cached count the two surfaces separately, so a test can prove
	// the publisher never reached the shared cache.
	Fresh, Cached int
}

var _ core.CredentialSource = (*Source)(nil)

func (s *Source) Descriptor() core.SourceDescriptor { return s.Descriptor_ }

func (s *Source) Fetch(ctx context.Context, p core.Purpose) (core.Token, error) {
	s.mu.Lock()
	s.Cached++
	s.mu.Unlock()
	return s.answer()
}

func (s *Source) FetchFresh(ctx context.Context, p core.Purpose) (core.Token, error) {
	s.mu.Lock()
	s.Fresh++
	s.mu.Unlock()
	return s.answer()
}

func (s *Source) answer() (core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return core.Token{}, s.Err
	}
	tok := core.Token{Value: s.Value, UsableUntil: s.UsableUntil}
	if s.TTL > 0 {
		tok.UsableUntil = time.Now().Add(s.TTL)
	}
	return tok, nil
}

// Counts reports the two fetch tallies under the lock.
func (s *Source) Counts() (fresh, cached int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Fresh, s.Cached
}

// StaticSource is the ordinary unbounded source most tests want: one value, no
// deadline, the authority of a daemon environment variable.
func StaticSource(variable, value string) *Source {
	return &Source{
		Descriptor_: core.SourceDescriptor{
			Kind:       "static",
			Authority:  "env:" + variable,
			BindingKey: "env:" + variable,
		},
		Value: value,
	}
}

// FailingSource answers with one classified failure, forever.
func FailingSource(authority string, class core.CredentialErrorClass, err error) *Source {
	return &Source{
		Descriptor_: core.SourceDescriptor{Kind: "static", Authority: authority, BindingKey: authority},
		Err:         &core.CredentialError{Class: class, Authority: authority, Err: err},
	}
}

// RemoteAuth is a scripted core.RemoteAuthSource — the git half of the tracker
// credential (SPEC §6.2).
//
// It counts calls because the property most tests here assert is *when* the
// credential is obtained: once per remote invocation, so a rotation reaches the
// next fetch and redaction covers the value that invocation used.
type RemoteAuth struct {
	Err error

	mu       sync.Mutex
	username string
	password string
	calls    int
}

var _ core.RemoteAuthSource = (*RemoteAuth)(nil)

// NewRemoteAuth returns a source answering with one credential.
func NewRemoteAuth(username, password string) *RemoteAuth {
	return &RemoteAuth{username: username, password: password}
}

// Rotate replaces what the next call answers with, which is the whole point of
// a source: a value captured once could not do this.
func (r *RemoteAuth) Rotate(password string) {
	r.mu.Lock()
	r.password = password
	r.mu.Unlock()
}

func (r *RemoteAuth) Auth(context.Context) (core.RemoteAuth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.Err != nil {
		return core.RemoteAuth{}, r.Err
	}
	return core.RemoteAuth{Username: r.username, Password: r.password}, nil
}

// Calls is how many times a credential has been obtained.
func (r *RemoteAuth) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
