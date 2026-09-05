// Package mirror is the daemon-side git fact source behind v2 publication
// verification: a bare repository BEN owns, fetches into with its own
// credential, and reads without ever asking the thing it is verifying.
//
// It exists because v1's evidence has an anchor v2's does not. When BEN hosts
// the run, the worktree and the base repository are BEN's, so a local branch
// head is already a fact about what the run did (SPEC §9.7, internal/workspace).
// When the run happens somewhere BEN does not own, everything inside that place
// is authored by the thing being judged — its response, its transcript, its
// filesystem, and any `git rev-parse` executed through it. None of it is
// evidence (SPEC §3.5). What is left is what BEN can read for itself, and this
// package is where BEN reads it.
//
// # What it owns
//
//   - **The pin.** The claim-time base is fetched and recorded *before* a remote
//     run may start, from the canonical remote, by the daemon. A base taken
//     afterwards is a base the run may already have moved, and the whole of leg 1
//     rests on it having been fixed while BEN was the only party who could move it.
//   - **The candidate.** The issue branch's head is read from the canonical
//     remote every observation and materialized in this repository's own object
//     store, so the ancestry question is answered over objects BEN holds rather
//     than over a SHA somebody reported.
//   - **Nothing else.** No worktree, no checkout, no push, no agent, no hook. The
//     store is unreachable from any run, which is what makes its contents
//     evidence.
//
// # Layout
//
//	<root>/<repository digest>/repository    the credential-free remote identity
//	<root>/<repository digest>/mirror.git    the bare store
//	<root>/<repository digest>/claims/<key>.json   the recorded claim pins
//
// Keyed by a digest of the remote rather than by a name, so two repositories
// cannot land on one store however similarly they are spelled, and the
// `repository` file is what turns a digest collision or a re-pointed root into a
// refusal instead of a silent mix (see identify). The tree is durable state, not
// a cache: the pin in it is the one fact a restart cannot reconstruct from the
// outside world, because after the run has pushed there is nothing left that
// says where the branch started.
//
// # Concurrency
//
// Every git invocation against one store is serialized, and claim records are
// replaced whole by rename, so a reader sees one record or the previous one and
// never a splice. Two Mirror values over one directory share those locks — the
// lock's identity is the directory, not the object — because assembly rebuilds
// adapters on reload and two generations must not drive one repository at once.
// Two *daemons* over one directory are excluded by deployment, exactly as they
// are for base.git (SPEC §10.1's single claim principal).
package mirror

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

// Named refusals (see AGENTS.md conventions); tests assert on these.
var (
	// ErrMirrorState refuses a store that is not what this package left behind:
	// a mirror.git that is not a bare repository, a ref that will not resolve, a
	// read that could not be completed. Fail closed with no auto-repair, for
	// SPEC §6.6's reason — repairing a store whose state nobody understands is
	// how evidence becomes fiction.
	ErrMirrorState = errors.New("mirror: store in unexpected state")

	// ErrRepositoryMismatch refuses a store that belongs to a different remote
	// than the one this mirror was built for. It is what makes "safe across
	// concurrent repositories" a property rather than a hope: a digest
	// collision, a root pointed at another workflow's tree, or an operator's
	// copy each surface here instead of ordering one repository's commits
	// against another's pin.
	ErrRepositoryMismatch = errors.New("mirror: store belongs to a different repository")

	// ErrRemoteCredentials refuses a remote URL carrying a credential, for every
	// scheme (SPEC §10.2). The message is constant because the value being
	// refused is the secret — workspace.ErrRemoteCredentials' rule, and the same
	// reasoning: the credential belongs behind an AuthSource that reaches git
	// through the child environment.
	ErrRemoteCredentials = errors.New("mirror: remote URL must not embed credentials — pass them via Repository.AuthSource so they stay out of argv and git config (SPEC §10.2)")

	// ErrTransportHelperRemote refuses Git's explicit <helper>::<address>
	// syntax. Its address is opaque to Git and may contain a credential in a
	// helper-specific grammar, so accepting it would make credential validation
	// a guess. The constant message cannot echo that potentially secret value.
	ErrTransportHelperRemote = errors.New("mirror: Git transport-helper remotes are not supported (SPEC §10.2)")

	// ErrCleartextCredentialRemote refuses the pairing of a credential source
	// with a remote git would authenticate to over an unencrypted transport
	// (#230) — workspace.ErrCleartextCredentialRemote's rule, for the same
	// reason. The credential helper's host scoping is no answer here: the
	// configured host is the one reading the token off the wire.
	//
	// The pairing and not the scheme, so a credential-free remote of any
	// transport is still fetchable, which is what BEN's own suites read from.
	ErrCleartextCredentialRemote = errors.New("mirror: a credential source cannot be used with a cleartext remote — http:// and ftp:// put the credential on the wire (SPEC §10.2)")

	// ErrClaimRefInvalid refuses a claim reference that is not fully named, has
	// a non-positive epoch, or whose key cannot be a git ref path component.
	//
	// A refusal rather than a sanitization, deliberately. The sanitizing
	// definition of a workspace key already exists (workspace.Key) and having a
	// second one here would let the two disagree about which branch a claim is
	// over — and the branch is the whole binding between an issue and the commits
	// verified for it. This package validates what it is handed and derives the
	// branch from it; it never invents a spelling of its own.
	ErrClaimRefInvalid = errors.New("mirror: claim reference is not usable")

	// ErrClaimUnrecorded refuses evidence for a claim that has no pin.
	//
	// It is the fail-closed direction and it is load-bearing: without a pin there
	// is no "advanced" to assert, every commit trivially descends from nothing,
	// and a remote run that pushed somebody else's branch would verify. A remote
	// run must not be startable before RecordClaim has returned, so meeting this
	// at verification time means either that ordering was broken or the store was
	// lost — and both must park rather than pass.
	ErrClaimUnrecorded = errors.New("mirror: no claim-time base is recorded for this claim")

	// ErrClaimTargetUnrecorded marks a claim record written before #152. The
	// base may be carried into a later epoch transition, but the record cannot
	// authorize same-epoch restore, prompt rendering, or verification.
	ErrClaimTargetUnrecorded = core.ErrClaimTargetUnrecorded

	// ErrBaseBranchNotFound is a structurally valid configured branch, or the
	// branch named by the remote HEAD symref, that the canonical remote does not
	// advertise.
	ErrBaseBranchNotFound = errors.New("mirror: base branch does not exist on the remote")

	// ErrBaseBranchReserved refuses a configured or repository-default target
	// inside the namespace BEN uses for issue publication branches. Allowing the
	// repository default to be ben/<workspace_key> would make the target and
	// candidate the same ref.
	ErrBaseBranchReserved = errors.New("mirror: base branch uses BEN's reserved branch namespace")

	// ErrClaimEpochMismatch refuses evidence for one claim cycle against the pin
	// of another. See core.RemoteClaimRef: an untouched publication from the
	// previous cycle is complete evidence for this one unless the epochs are
	// compared, and this is where they are compared.
	ErrClaimEpochMismatch = errors.New("mirror: the recorded claim-time base belongs to a different claim epoch")

	// ErrClaimPinLost refuses a recorded claim whose pinned commit is no longer
	// in the store, or no longer the commit the record names.
	//
	// The record and the ref are written together and must agree. When they do
	// not, the object store cannot order anything against the base — and the
	// alternative to refusing is asking git to compare against a commit it does
	// not have, whose answer is an error a looser classifier would read as "does
	// not descend".
	ErrClaimPinLost = errors.New("mirror: the recorded claim-time base is missing from the store")

	// ErrRemoteRaced refuses an observation whose subject moved while it was
	// being made: the head named by the probe is not the head the fetch landed.
	//
	// A moving branch is not a verdict. The facts would pair one commit's
	// ancestry with another commit's identity, and a pull request checked against
	// either would be checked against a head that was never simultaneously true.
	// The next tick observes again, which is the whole cost of refusing.
	ErrRemoteRaced = errors.New("mirror: the canonical remote branch moved during the observation")

	// ErrRefAmbiguous refuses a remote that answered with more than one object
	// for one exact ref path. Nothing in this package picks between them.
	ErrRefAmbiguous = errors.New("mirror: the canonical remote reported an ambiguous ref")
)

const (
	// branchPrefix is the canonical issue-branch namespace, the same one the v1
	// strategy publishes into (SPEC §6.3). Stated here rather than imported:
	// this package must not depend on the local workspace strategy at all — see
	// the arch test — and the constant is part of the wire contract with the
	// forge, not an implementation detail of either package.
	branchPrefix = "ben/"
	// pinRefPrefix holds the claim-time base. The epoch is in the path, so a pin
	// recorded for one claim cycle is not reachable by another's name even if
	// this package's record handling were wrong.
	pinRefPrefix = "refs/ben/v2/pin/"
	// headRefPrefix holds the fetched candidate. A non-branch ref, so nothing
	// about it can be confused with a branch the store publishes.
	headRefPrefix = "refs/ben/v2/head/"
	// mirrorDirName and claimsDirName are the store's two halves.
	mirrorDirName = "mirror.git"
	claimsDirName = "claims"
	// repositoryFileName records which remote the store is for.
	repositoryFileName = "repository"
)

// Options parameterize a mirror. Assembly builds them; there is deliberately no
// WORKFLOW.md key behind Root, because adding one is a §5.2 schema change and
// this ticket does not carry SPEC sign-off.
type Options struct {
	// Root is the absolute directory the per-repository stores live under.
	Root string
	// Repository is the canonical remote and the credential that reads it, as
	// the tracker named them (core.RepositorySource, workspace.RepositoryFrom).
	// RemoteURL MUST be credential-free; the credential belongs behind
	// AuthSource, which is resolved immediately before each remote invocation.
	// With an AuthSource it must also not be a remote git would authenticate to
	// in the clear (ErrCleartextCredentialRemote).
	Repository core.Repository
	// BaseBranch is workspace.base_branch. Empty selects the then-current
	// repository default for each new claim epoch.
	BaseBranch string
	// Now defaults to time.Now. It stamps the audit fields and nothing decides
	// anything from it, which is why a fake clock here cannot weaken a verdict.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Mirror is one repository's daemon-side store.
type Mirror struct {
	dir        string // <root>/<digest>
	gitDir     string // <dir>/mirror.git
	claimsDir  string // <dir>/claims
	remoteURL  string
	baseBranch string
	repository string // credential-free identity, reported in facts
	authSource core.RemoteAuthSource
	now        func() time.Time
	logger     *slog.Logger
	locks      *locks
	// syncDirectory is fixed to syncDir in production. The private seam lets the
	// durability regressions prove RecordClaim refuses a failed store, Git or
	// record publication sync instead of mistaking rename success for durable
	// state.
	syncDirectory func(string) error
}

// locks serialize one store. Held per directory rather than per Mirror: a config
// reload builds a new adapter over the same tree, and two generations fetching
// into one repository at once is git's lock contention surfacing as a spurious
// evidence failure.
type locks struct {
	// bootstrap serializes atomic store creation and its durability publication
	// across adapter generations in this process.
	bootstrap sync.Mutex
	// durable is protected by bootstrap. A fresh process proves the destination
	// directory entry once before it relies on an existing store.
	durable bool
	// git serializes every invocation against the store, as baseMu does for
	// base.git.
	git sync.Mutex
	// claims serializes the record for one key against itself.
	claims sync.Map // key -> *sync.Mutex
}

var (
	registryMu sync.Mutex
	registry   = map[string]*locks{}
)

func locksFor(dir string) *locks {
	registryMu.Lock()
	defer registryMu.Unlock()
	l, ok := registry[dir]
	if !ok {
		l = &locks{}
		registry[dir] = l
	}
	return l
}

// New builds a mirror over Root for one repository. It touches nothing on disk:
// the store is created on first use, so a daemon whose v2 path is never reached
// leaves no tree behind.
func New(opts Options) (*Mirror, error) {
	if opts.Root == "" || !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("mirror: root must be an absolute path, got %q", opts.Root)
	}
	identity, err := identify(opts.Repository.RemoteURL)
	if err != nil {
		return nil, err
	}
	if opts.Repository.AuthSource != nil && gitremote.IsCleartextTransport(opts.Repository.RemoteURL) {
		return nil, ErrCleartextCredentialRemote
	}
	dir := filepath.Join(filepath.Clean(opts.Root), digest(identity))
	m := &Mirror{
		dir:           dir,
		gitDir:        filepath.Join(dir, mirrorDirName),
		claimsDir:     filepath.Join(dir, claimsDirName),
		remoteURL:     opts.Repository.RemoteURL,
		baseBranch:    opts.BaseBranch,
		repository:    identity,
		authSource:    opts.Repository.AuthSource,
		now:           opts.Now,
		logger:        opts.Logger,
		locks:         locksFor(dir),
		syncDirectory: syncDir,
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	return m, nil
}

// Repository is the credential-free identity of the remote this mirror reads.
// It is what the verifier compares its expectation against, and what appears in
// facts and in log lines — never the URL as configured, which may carry
// userinfo a scheme allows and this package refuses.
func (m *Mirror) Repository() string { return m.repository }

// Ready proves the branch a future claim would select exists, with the same
// credential source RecordClaim uses. The store is initialized first because
// remote Git invocations run with its bare directory as cwd.
func (m *Mirror) Ready(ctx context.Context) error {
	if err := m.ensure(ctx); err != nil {
		return err
	}
	_, _, err := m.resolveTargetBranch(ctx)
	return err
}

// Dir is the store's directory, for operators and for tests.
func (m *Mirror) Dir() string { return m.dir }

// Branch is the canonical issue branch for a workspace key. Derived, never
// accepted: a branch supplied by a caller is a branch a compromised caller
// chooses, and every leg of the check is about *this* branch.
func Branch(key string) string { return branchPrefix + key }

// identify reduces a remote URL to a credential-free identity, refusing one that
// carries a secret.
//
// Two shapes reach here. A URL with a scheme is parsed, and any userinfo at all
// is a refusal — not merely a password half: a bare username in a URL is still
// an authenticated URL, and this string is written to disk, put in facts, and
// printed. An scp-like address (`git@host:path`) or a plain filesystem path has
// no userinfo grammar to trust, so the part before an `@` is dropped and the
// rest kept verbatim.
//
// The identity is a *comparison key and an audit line*, not a URL to fetch from:
// nothing reconstructs a remote from it. Its readable suffix normalizes the
// scheme and SSH username away, while a fingerprint of the exact configured
// value keeps those transport choices — and literal .git suffixes — from
// collapsing distinct remotes onto one store. The port remains in the readable
// suffix because two forges can serve different repositories at one host.
func identify(remoteURL string) (string, error) {
	identity, err := gitremote.RepositoryIdentity(remoteURL)
	switch {
	case errors.Is(err, gitremote.ErrRemoteEmpty):
		return "", errors.New("mirror: repository remote URL is empty")
	case errors.Is(err, gitremote.ErrTransportHelperRemote):
		return "", ErrTransportHelperRemote
	case errors.Is(err, gitremote.ErrRemoteCredentials):
		return "", ErrRemoteCredentials
	case err != nil:
		return "", fmt.Errorf("mirror: identifying repository: %w", err)
	}
	return identity, nil
}

// digest names the store's directory from the identity: 64 bits of FNV-1a as 16
// hex characters, the same collision floor workspace.Key meets.
//
// A digest rather than the identity itself, so the directory name cannot be a
// path traversal, a case-folding collision, or a 300-character URL — and so that
// the tree carries no repository name for a passer-by to read. What the store is
// for is written *inside* it, where the mismatch check can find it.
func digest(identity string) string {
	h := fnv.New64a()
	io.WriteString(h, identity) //nolint:errcheck // hash.Write never fails
	return fmt.Sprintf("%016x", h.Sum64())
}

// ensure creates the store if it is absent and proves it usable if it is not.
//
// Creation is atomic: the bare repository is initialized in a temporary
// directory and renamed into place, so an interrupted bootstrap leaves either no
// store or a complete one and never a half-initialized repository a later run
// would fetch into. The identity file is written first *inside* the temporary
// tree, so the store and the statement of what it is for arrive together.
//
// No `origin` remote is configured, and that is deliberate: nothing pushes here,
// no agent can reach here, and every fetch names the URL per invocation. A store
// that records no remote is a store whose on-disk config cannot leak one.
func (m *Mirror) ensure(ctx context.Context) error {
	m.locks.bootstrap.Lock()
	defer m.locks.bootstrap.Unlock()

	switch present, err := m.present(); {
	case err != nil:
		return err
	case present:
		if err := m.verifyIdentity(ctx); err != nil {
			return err
		}
		return m.publishDurably()
	}

	if err := mkdirAllSynced(filepath.Dir(m.dir), 0o700, m.syncDirectory); err != nil {
		return fmt.Errorf("mirror: preparing %s: %w", filepath.Dir(m.dir), err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(m.dir), ".tmp-mirror-")
	if err != nil {
		return fmt.Errorf("mirror: preparing a store for %s: %w", m.repository, err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // no-op once the rename succeeds

	if err := os.WriteFile(filepath.Join(tmp, repositoryFileName), []byte(m.repository+"\n"), 0o600); err != nil {
		return fmt.Errorf("mirror: recording the repository identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, claimsDirName), 0o700); err != nil {
		return fmt.Errorf("mirror: preparing the claim store: %w", err)
	}
	if _, err := m.gitIn(ctx, filepath.Dir(m.dir), "", nil,
		"init", "--quiet", "--bare", filepath.Join(tmp, mirrorDirName)); err != nil {
		return fmt.Errorf("%w: initializing a store for %s: %v", ErrMirrorState, m.repository, err)
	}
	// Rename publishes names, not the bytes and child entries behind them.
	// Flush the complete initialized tree before making it authoritative.
	if err := syncTree(tmp, m.syncDirectory); err != nil {
		return fmt.Errorf("mirror: syncing the new store for %s: %w", m.repository, err)
	}
	if err := os.Rename(tmp, m.dir); err != nil {
		// A store standing where this one was going means another generation of
		// this daemon won the race and finished first. Its contents are this
		// one's contents, so adopt it after the identity check rather than
		// refusing — but never merge into it.
		//
		// Decided by looking rather than by reading the errno, because the errno
		// is not one value: renaming a directory onto a populated one is
		// ENOTEMPTY on Linux, EEXIST elsewhere, and neither is what the platform
		// promises. A spurious refusal here would park a claim over a race that
		// resolved itself.
		if present, perr := m.present(); perr == nil && present {
			if err := m.verifyIdentity(ctx); err != nil {
				return err
			}
			return m.publishDurably()
		}
		return fmt.Errorf("mirror: publishing the store for %s: %w", m.repository, err)
	}
	if err := m.publishDurably(); err != nil {
		return err
	}
	m.logger.Info("mirror store created", "path", m.dir, "repository", m.repository)
	return nil
}

// publishDurably closes rename's last crash window. A successful rename can
// still disappear after power loss until the destination parent is synced;
// RecordClaim must not let a remote run start while that remains possible.
// bootstrap protects durable.
func (m *Mirror) publishDurably() error {
	if m.locks.durable {
		return nil
	}
	if err := m.syncDirectory(filepath.Dir(m.dir)); err != nil {
		return fmt.Errorf("mirror: making the store for %s durable: %w", m.repository, err)
	}
	m.locks.durable = true
	return nil
}

// present reports whether the store's directory is there to be read. Lstat, and
// only fs.ErrNotExist is absence — internal/workspace's rule, for its reason: a
// dangling symlink is an entry over a target that could reappear, which is
// "cannot know" and must not read as storage nobody has written yet (#183).
func (m *Mirror) present() (bool, error) {
	if _, err := os.Lstat(m.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: cannot tell whether the store %s exists: %v", ErrMirrorState, m.dir, err)
	}
	return true, nil
}

// verifyIdentity proves an existing store is this repository's and is a bare git
// repository. Both halves, because they fail differently: the first is a store
// belonging to somebody else, the second is a store nobody can fetch into.
func (m *Mirror) verifyIdentity(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(m.dir, repositoryFileName))
	if err != nil {
		return fmt.Errorf("%w: cannot read the identity of the store at %s: %v", ErrMirrorState, m.dir, err)
	}
	if got := strings.TrimSpace(string(raw)); got != m.repository {
		return fmt.Errorf("%w: the store at %s is for %q, this mirror reads %q",
			ErrRepositoryMismatch, m.dir, got, m.repository)
	}
	out, err := m.git(ctx, "rev-parse", "--is-bare-repository")
	if err != nil || out != "true" {
		return fmt.Errorf("%w: %s is not a bare repository (%q): %v", ErrMirrorState, m.gitDir, out, err)
	}
	if err := mkdirAllSynced(m.claimsDir, 0o700, m.syncDirectory); err != nil {
		return fmt.Errorf("mirror: preparing the claim store: %w", err)
	}
	return nil
}

// validate refuses a claim reference this package cannot name a ref with.
func validate(ref core.RemoteClaimRef) error {
	if !ref.Complete() {
		return fmt.Errorf("%w: issue, key and epoch are all required (got %+v)", ErrClaimRefInvalid, ref)
	}
	if !refComponentSafe(ref.Key) {
		return fmt.Errorf("%w: workspace key %q is not a git ref path component", ErrClaimRefInvalid, ref.Key)
	}
	return nil
}

// refComponentSafe reports whether s is usable as one component of a ref path
// and as a file name — the same rule workspace.Key sanitizes towards
// (git-check-ref-format(1)), applied here as a check.
//
// Restated rather than imported, and the arch test is what holds the reason:
// this package must not depend on the local workspace strategy, because the
// point of it is to produce evidence that does not pass through anything a run
// could have touched.
func refComponentSafe(s string) bool {
	switch {
	case s == "":
		return false
	case strings.HasPrefix(s, "."), strings.HasSuffix(s, "."):
		return false
	case strings.Contains(s, ".."), strings.HasSuffix(s, ".lock"):
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (m *Mirror) pinRef(ref core.RemoteClaimRef) string {
	return pinRefPrefix + strconv.FormatInt(ref.Epoch, 10) + "/" + ref.Key
}

func (m *Mirror) headRef(ref core.RemoteClaimRef) string {
	return headRefPrefix + strconv.FormatInt(ref.Epoch, 10) + "/" + ref.Key
}

// DefaultBranch is the canonical remote's default branch, read with the daemon's
// own credential.
//
// Exported for the one consumer that needs it and cannot ask anything else: the
// v2 verifier's TargetBranch (verify.RemoteExpectation), which is what makes "a
// pull request against an unprotected branch is not the review gate" checkable.
// The tracker could be asked instead, and deliberately is not — the branch a
// publication must target is a property of the repository BEN fetches, and
// reading it from the same remote the evidence comes from keeps one answer
// rather than two that can disagree.
//
// A read, made when the daemon assembles rather than per verification: it is
// deployment configuration in all but name, and re-reading it per claim would
// spend a network round trip to be told the same thing. Assembly is also the
// store's first use on a fresh deployment, so this method creates and validates
// the store before running Git from inside it.
func (m *Mirror) DefaultBranch(ctx context.Context) (string, error) {
	if err := m.ensure(ctx); err != nil {
		return "", err
	}
	return m.defaultBranch(ctx)
}
