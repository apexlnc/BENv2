package fake

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The v2 remote-verification stand-ins: a daemon-side fact source and a forge.
//
// Their fidelity is read off internal/mirror's control flow and
// core.RemotePRSource's contract, in the order those fail (AGENTS.md,
// Conventions). That matters more here than for most fakes, because the thing
// under test is a *refusal* discipline: a fake that answered where the real one
// refuses would let a verifier that skips a check pass its own tests. So the
// rules the real components enforce are enforced here too — a claim must be
// recorded before facts exist, an epoch cannot read another epoch's pin, an
// answer always echoes the request it answered, a forge with two candidates
// refuses rather than choosing — and both run internal/mirror/mirrortest's
// conformance suite unmodified.
//
// What they deliberately do *not* model is git. There is no object store here,
// only a parent chain, which is enough to be a descendant or not and nothing
// more. Every case that turns on git's own classification of a failure —
// unreadable stores, missing objects, ambiguous refs — belongs to internal/mirror's
// own tests, against a real repository.

// MirrorEpoch is the time a fresh Mirror stamps on the records it writes. Fixed
// rather than wall-clock, so a fixture states its own timeline.
var MirrorEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// DefaultRepository is the credential-free repository identity a fresh Mirror
// reads, matching the shape internal/mirror derives from a remote URL.
const DefaultRepository = "github.test/acme/repo"

// Mirror is an in-memory daemon-side fact source: a canonical remote anyone can
// move, a claim store only this daemon writes, and the ancestry between them.
type Mirror struct {
	mu sync.Mutex

	repository string
	// branches is the canonical remote's refs/heads/<branch> → head.
	branches map[string]string
	// parents is the ancestry of every commit this fixture minted, "" for a root.
	parents map[string]string
	// defaultHead is the default branch's head — what a claim pins to when the
	// issue branch does not exist yet, as internal/mirror does.
	defaultHead string
	// targetBranch is the selector a new claim records beside its base. The fake
	// defaults to the repository default's conventional name and lets tests move
	// it independently so claim-scoped retention can be asserted.
	targetBranch string
	// claims are the recorded pins, keyed by workspace key. One per key: a new
	// epoch replaces the old, which is the real store's behaviour and the reason
	// an epoch has to be compared rather than assumed.
	claims map[string]core.RemoteClaim
	seq    uint64

	// Now stamps records. Nothing decides anything from it.
	Now func() time.Time

	// FailFacts, when set, fails the canonical-remote observation after the claim
	// record has been admitted, at the point the real mirror can encounter a
	// credential or network failure. Nil means the ordinary rules decide.
	//
	// A predicate rather than one error, because the fixtures that matter fail
	// one verification of a claim and not the one after it — §9.7's transient
	// credential retry is exactly that shape.
	FailFacts func(run core.RemoteRunRef) error
	// FailRecord is FailFacts for the remote read a new RecordClaim performs. An
	// idempotent same-epoch call returns its existing pin without reaching it, as
	// the real mirror does.
	FailRecord func(ref core.RemoteClaimRef) error
	// StaleFacts, when true, answers with Fetched false — a source serving a
	// remembered head instead of an observed one, which every consumer must
	// refuse.
	StaleFacts bool
	// Rewrite, when set, rewrites each answer on its way out.
	//
	// It is how a fixture states the failures that are *not* about the world: a
	// source replaying an older observation, answering about another branch, or
	// reporting another repository. Those cannot be arranged by moving the
	// canonical remote, because no honest source produces them — which is the
	// reason a consumer has to check for them rather than assume.
	RewriteFacts func(facts core.RemotePublishFacts) core.RemotePublishFacts

	// Requests records every RemoteFacts call, in order.
	Requests []core.RemoteRunRef
}

// NewMirror returns a mirror over DefaultRepository whose default branch has one
// commit.
func NewMirror() *Mirror {
	m := &Mirror{
		repository:   DefaultRepository,
		branches:     map[string]string{},
		parents:      map[string]string{},
		claims:       map[string]core.RemoteClaim{},
		targetBranch: "main",
		Now:          func() time.Time { return MirrorEpoch },
	}
	m.defaultHead = m.mint("default")
	return m
}

// Repository is the credential-free identity this mirror reads.
func (m *Mirror) Repository() string { return m.repository }

// SetRepository renames the repository this mirror claims to read, for the
// fixtures where a verifier's expectation and its fact source disagree.
func (m *Mirror) SetRepository(identity string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repository = identity
}

// DefaultHead is the default branch's head — what a fresh claim pins to.
func (m *Mirror) DefaultHead() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultHead
}

// SetTargetBranch changes the selector used only by later claim epochs. A
// branch without an explicit fake head starts at DefaultHead, matching the
// repository-default fixture this fake historically modeled.
func (m *Mirror) SetTargetBranch(branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targetBranch = branch
}

// Commit appends a commit to a branch of the canonical remote and returns it. A
// branch that does not exist yet starts from the default head, which is what an
// agent branching from the default branch produces.
func (m *Mirror) Commit(branch string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, ok := m.branches[branch]
	if !ok {
		parent = m.defaultHead
	}
	sha := m.mint(branch)
	m.parents[sha] = parent
	m.branches[branch] = sha
	return sha
}

// Rewrite replaces a branch with an unrelated history: the force push, and the
// commits-that-are-not-ours case with it.
func (m *Mirror) Rewrite(branch string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sha := m.mint("rewritten-" + branch)
	m.parents[sha] = ""
	m.branches[branch] = sha
	return sha
}

// DeleteBranch removes a branch from the canonical remote.
func (m *Mirror) DeleteBranch(branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.branches, branch)
}

// Head is the canonical remote's head for a branch, "" when it has none.
func (m *Mirror) Head(branch string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.branches[branch]
}

// RecordClaim pins the claim-time base, with internal/mirror's rules: the issue
// branch's head when the canonical remote has one, the default head otherwise,
// and idempotent within one epoch so a restart re-recording a claim cannot move
// a base a run may already be working from.
func (m *Mirror) RecordClaim(_ context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	return m.recordClaim(ref)
}

// RecordClaimRetaining has the real mirror's #266 shape. This fake stores only
// the authoritative current record, not Git refs, so retained cleanup refs have
// no separate in-memory representation; Discard already leaves a later record
// untouched.
func (m *Mirror) RecordClaimRetaining(
	_ context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef,
) (core.RemoteClaim, error) {
	if err := validateRef(ref); err != nil {
		return core.RemoteClaim{}, err
	}
	for _, old := range retained {
		if err := validateRef(old); err != nil {
			return core.RemoteClaim{}, err
		}
		if old.Key != ref.Key || old.Issue != ref.Issue {
			return core.RemoteClaim{}, fmt.Errorf("fake mirror: retained claim %+v does not belong to %+v", old, ref)
		}
	}
	return m.recordClaim(ref)
}

func (m *Mirror) recordClaim(ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	if err := validateRef(ref); err != nil {
		return core.RemoteClaim{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.claims[ref.Key]; ok {
		if existing.Ref.Issue != ref.Issue {
			return core.RemoteClaim{}, fmt.Errorf("fake mirror: %s is recorded for issue %s, asked for %s",
				ref.Key, existing.Ref.Issue, ref.Issue)
		}
		if existing.Repository != m.repository {
			return core.RemoteClaim{}, fmt.Errorf("fake mirror: %s is pinned against %s, this mirror reads %s",
				ref.Key, existing.Repository, m.repository)
		}
		if existing.Ref.Epoch == ref.Epoch {
			return existing, nil
		}
	}
	if m.FailRecord != nil {
		if err := m.FailRecord(ref); err != nil {
			return core.RemoteClaim{}, err
		}
	}
	branch := RemoteBranch(ref.Key)
	base, ok := m.branches[branch]
	if !ok {
		base = m.defaultHead
	}
	claim := core.RemoteClaim{
		Ref:          ref,
		Branch:       branch,
		BaseSHA:      base,
		TargetBranch: m.targetBranch,
		Repository:   m.repository,
		RecordedAt:   m.Now().UTC(),
	}
	m.claims[ref.Key] = claim
	return claim, nil
}

// Claim reads a pin back, refusing absence and refusing another epoch's.
func (m *Mirror) Claim(_ context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	if err := validateRef(ref); err != nil {
		return core.RemoteClaim{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimLocked(ref)
}

func (m *Mirror) claimLocked(ref core.RemoteClaimRef) (core.RemoteClaim, error) {
	claim, ok := m.claims[ref.Key]
	if !ok {
		return core.RemoteClaim{}, fmt.Errorf("fake mirror: no claim-time base is recorded for %s", ref.Key)
	}
	if claim.Ref.Epoch != ref.Epoch {
		return core.RemoteClaim{}, fmt.Errorf("fake mirror: %s is pinned for epoch %d, asked for %d",
			ref.Key, claim.Ref.Epoch, ref.Epoch)
	}
	if claim.Ref.Issue != ref.Issue {
		return core.RemoteClaim{}, fmt.Errorf("fake mirror: %s is recorded for issue %s, asked for %s",
			ref.Key, claim.Ref.Issue, ref.Issue)
	}
	if claim.Repository != m.repository {
		return core.RemoteClaim{}, fmt.Errorf("fake mirror: %s is pinned against %s, this mirror reads %s",
			ref.Key, claim.Repository, m.repository)
	}
	return claim, nil
}

// Discard forgets a claim. Tolerant of one that was never recorded, like the
// real store's.
func (m *Mirror) Discard(_ context.Context, ref core.RemoteClaimRef) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if claim, ok := m.claims[ref.Key]; ok && claim.Ref.Epoch == ref.Epoch {
		if claim.Ref.Issue != ref.Issue {
			return fmt.Errorf("fake mirror: %s is recorded for issue %s, asked for %s",
				ref.Key, claim.Ref.Issue, ref.Issue)
		}
		delete(m.claims, ref.Key)
	}
	return nil
}

// RemoteFacts observes the canonical remote for one verification.
func (m *Mirror) RemoteFacts(_ context.Context, run core.RemoteRunRef) (core.RemotePublishFacts, error) {
	if !run.Complete() {
		return core.RemotePublishFacts{}, fmt.Errorf("fake mirror: incomplete run reference %+v", run)
	}
	if err := validateRef(run.Claim); err != nil {
		return core.RemotePublishFacts{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, run)

	claim, err := m.claimLocked(run.Claim)
	if err != nil {
		return core.RemotePublishFacts{}, err
	}
	if m.FailFacts != nil {
		if err := m.FailFacts(run); err != nil {
			return core.RemotePublishFacts{}, err
		}
	}
	facts := core.RemotePublishFacts{
		Run:        run,
		Repository: m.repository,
		Branch:     claim.Branch,
		BaseSHA:    claim.BaseSHA,
		RemoteHead: m.branches[claim.Branch],
		Fetched:    !m.StaleFacts,
		ObservedAt: m.Now().UTC(),
	}
	if facts.RemoteHead != "" {
		facts.DescendsBase = m.descends(claim.BaseSHA, facts.RemoteHead)
	}
	if m.RewriteFacts != nil {
		facts = m.RewriteFacts(facts)
	}
	return facts, nil
}

// descends walks the parent chain. Reflexive, as `merge-base --is-ancestor` is:
// a branch still at its pin descends from it, and "advanced" is a separate
// question the consumer asks.
func (m *Mirror) descends(base, head string) bool {
	for sha := head; sha != ""; sha = m.parents[sha] {
		if sha == base {
			return true
		}
	}
	return false
}

// mint returns a distinct 40-hex-character object name. Derived from a counter
// so a fixture's shas are stable across runs, and unlike a counter written out
// they do not collide with a test's own literals.
func (m *Mirror) mint(label string) string {
	m.seq++
	h := fnv.New64a()
	fmt.Fprintf(h, "%s#%d", label, m.seq) //nolint:errcheck // hash.Write never fails
	sum := h.Sum64()
	return fmt.Sprintf("%016x%016x%08x", sum, sum^0x5bf03b1d, m.seq)
}

// RemoteBranch derives the canonical issue branch from a workspace key, as both
// internal/mirror and internal/verify do independently.
func RemoteBranch(key string) string { return "ben/" + key }

func validateRef(ref core.RemoteClaimRef) error {
	if !ref.Complete() {
		return fmt.Errorf("fake mirror: incomplete claim reference %+v", ref)
	}
	if strings.ContainsAny(ref.Key, "/ \t") {
		return fmt.Errorf("fake mirror: claim reference %+v is not usable as a ref path", ref)
	}
	return nil
}

// Forge is an in-memory core.RemotePRSource: the open pull requests a v2
// verifier reads for leg 3.
type Forge struct {
	mu  sync.Mutex
	prs []core.RemotePR

	// FailPR, when set, decides each call's outcome. It is how a fixture states
	// the two failures that must never read as absence — an enumeration that did
	// not finish (core.ErrRemotePRIncomplete) and a credential that would not
	// resolve.
	FailPR func(q core.RemotePRQuery) error
	// Rewrite, when set, rewrites each answer on its way out: the adapter that
	// returns a pull request the query did not ask for. No honest forge produces
	// one, which is exactly why a consumer must not assume it cannot happen.
	RewritePR func(pr core.RemotePR) core.RemotePR

	// Queries records every read, in order, so a test can assert that leg 3 was
	// reached — or, for the fixtures where the git legs already settle the
	// verdict, that it was not.
	Queries []core.RemotePRQuery
}

// NewForge returns an empty forge.
func NewForge() *Forge { return &Forge{} }

// Open adds an open pull request. Fields the fixture leaves empty are filled
// with the consistent value — the repository on both sides, the head branch's
// name, "open" — so that a test states only the thing it is varying and a
// mismatch in a fixture is deliberate rather than an omission.
func (f *Forge) Open(pr core.RemotePR) core.RemotePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr.State == "" {
		pr.State = "open"
	}
	if pr.Repository == "" {
		pr.Repository = DefaultRepository
	}
	if pr.HeadRepository == "" {
		pr.HeadRepository = pr.Repository
	}
	if pr.Number == 0 {
		pr.Number = 100 + len(f.prs)
	}
	if pr.URL == "" {
		pr.URL = fmt.Sprintf("https://%s/pull/%d", pr.Repository, pr.Number)
	}
	f.prs = append(f.prs, pr)
	return pr
}

// RemotePR returns the open pull request published on the query's branch.
//
// Ambiguity refuses rather than picking, which is the contract's rule and not a
// convenience of this fixture: two open pull requests on one branch is a world
// the daemon's model does not account for, and any tie-break would be choosing
// the answer that makes verification pass.
func (f *Forge) RemotePR(_ context.Context, q core.RemotePRQuery) (*core.RemotePR, error) {
	if f.FailPR != nil {
		if err := f.FailPR(q); err != nil {
			return nil, err
		}
	}
	if q.Repository == "" || q.Branch == "" {
		return nil, errors.New("fake forge: a query names a repository and a branch")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Queries = append(f.Queries, q)

	var found []core.RemotePR
	for _, pr := range f.prs {
		if pr.State == "open" && pr.Repository == q.Repository && pr.HeadBranch == q.Branch {
			found = append(found, pr)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		pr := found[0]
		if f.RewritePR != nil {
			pr = f.RewritePR(pr)
		}
		return &pr, nil
	default:
		return nil, fmt.Errorf("%w: %d open on %s", core.ErrRemotePRAmbiguous, len(found), q.Branch)
	}
}
