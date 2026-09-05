package core

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Remote (v2) publication evidence — the shapes behind proving a publication
// BEN did not perform and did not watch.
//
// v1's evidence is read off the daemon's own disk: the agent ran in a worktree
// BEN created, so a local branch head *is* a fact about what the run did. A run
// that happens somewhere BEN does not own has no such anchor. Its response, its
// transcript, its filesystem and any git command executed through it are all
// authored by the thing being verified, and none of them may be evidence
// (SPEC §3.5).
//
// So the three legs of §9.7 keep their meaning and change their *sources*: the
// claim-time base comes from a daemon-side pin recorded before the run could
// start, the candidate from the canonical remote read with the daemon's own
// fetch credential, and the pull request from the tracker read with the
// daemon's own tracker credential. Nothing here is supplied by the run.
//
// These types live in core for the reason PublishFacts does: they cross an
// adapter boundary, the consumer declares the seam it needs, and the core never
// sees the provider payload either side of it (SPEC §3.6, §6.1).

// RemoteClaimRef names the claim a piece of v2 evidence speaks for.
//
// The epoch is what makes evidence unshareable between claims. A remote branch
// outlives the claim that pushed it — the same `ben/<key>` head, the same open
// pull request, and a *second* claim cycle over the same issue a week later.
// Without an epoch in the binding, the untouched publication of the first claim
// is complete evidence for the second, and a run that did nothing at all is
// `done` on somebody else's work.
type RemoteClaimRef struct {
	// Issue is the tracker identifier the claim is over.
	Issue string
	// Key is the sanitized workspace key (SPEC §6.3); the issue branch is
	// derived from it and never accepted from a caller.
	Key string
	// Epoch is the positive tracker-native event ID of the assignment that
	// opened the claim cycle (SPEC §8.4). It is the same identity carried by a
	// local Workspace and ClaimBase; zero is non-authorizing.
	Epoch int64
}

// Complete reports whether every field a claim must be named by is present. A
// partially named claim is refused rather than defaulted: each empty field is a
// binding that would match something it should not.
func (r RemoteClaimRef) Complete() bool {
	return r.Issue != "" && r.Key != "" && r.Epoch > 0
}

// RemoteRunRef names one verification of one attempt of one claim.
//
// Three identities rather than one, because they expire at different rates and
// each is a way for stale evidence to arrive. The claim survives every attempt
// in the cycle; the run is the attempt whose publication is being judged, so a
// verdict earned by attempt 2 must not settle attempt 3; the verification is the
// single observation, so a fact source that answered an earlier request cannot
// have its answer replayed into a later one.
type RemoteRunRef struct {
	Claim RemoteClaimRef
	// Run identifies the attempt — the remote run/workspace identity, whatever
	// the substrate calls its session.
	Run string
	// Verification identifies this verification attempt. A fresh value per call:
	// it is what makes "these facts were observed for this question" checkable
	// rather than assumed.
	Verification string
}

// Complete reports whether the run reference names all three identities.
func (r RemoteRunRef) Complete() bool {
	return r.Claim.Complete() && r.Run != "" && r.Verification != ""
}

// RemoteClaim is the trusted claim-time pin: the commit a remote run's work must
// descend from, recorded by the daemon **before** that run could start.
//
// Before, and not after, because a pin taken afterwards is a pin taken from a
// branch the run may already have moved. The whole of leg 1 rests on the base
// having been fixed while the only party who could have changed it was BEN.
type RemoteClaim struct {
	Ref RemoteClaimRef
	// Branch is the canonical issue branch, derived from Ref.Key.
	Branch string
	// BaseSHA is the pinned claim-time base commit.
	BaseSHA string
	// TargetBranch is the claim-scoped pull-request target selected at the same
	// boundary as BaseSHA. It may not be inferred from a later repository
	// default or workflow revision.
	TargetBranch string
	// Repository is the credential-free identity of the remote the pin was taken
	// from, so a pin cannot be read against a different repository's history.
	Repository string
	RecordedAt time.Time
}

// RemotePublishFacts is the daemon-side git half of the v2 evidence check: what
// the canonical remote says, ordered against the trusted pin, bound to the
// question it was asked.
//
// Facts, never a verdict — PublishFacts' rule, for PublishFacts' reason: how a
// run ended decides how the same shape routes (SPEC §9.6, §9.7).
type RemotePublishFacts struct {
	// Run echoes the request. A consumer MUST compare it: a fact source that
	// answers about a different claim, attempt or verification has not answered
	// this question, and the difference between the two is exactly the stale
	// evidence the binding exists to catch.
	Run RemoteRunRef
	// Repository is the credential-free identity of the remote that was read —
	// never a URL carrying userinfo, and never the fetch credential.
	Repository string
	// Branch is the canonical issue branch that was read, derived from the key.
	Branch string
	// BaseSHA is the trusted pin these facts were ordered against.
	BaseSHA string
	// RemoteHead is the canonical remote's head for Branch, "" when the remote
	// has no such branch. Meaningful only when Fetched.
	RemoteHead string
	// Fetched reports that the canonical remote was contacted during *this*
	// observation and its answer materialized in the daemon's own object store.
	// False means the fields above are a memory rather than an observation, and
	// a consumer MUST refuse them: a mirror serving a cached head would report a
	// publication that has since been force-pushed away (SPEC §9.10 — absence,
	// and staleness, are not evidence).
	Fetched bool
	// DescendsBase reports that BaseSHA is an ancestor of RemoteHead according to
	// the daemon's own object store. Reflexive, like its v1 counterpart: a branch
	// still at its pin descends from it, and "advanced" is asked separately.
	DescendsBase bool
	// ObservedAt is when the remote was read, for the audit record.
	ObservedAt time.Time
}

// RemoteIssueLinkStatus is the closed set of answers a forge can give to "which
// issues does this pull request close".
type RemoteIssueLinkStatus int

const (
	// RemoteIssueLinkUnknown is the zero value: the source did not state the
	// linkage. It is not "the pull request closes nothing" — a field nobody
	// filled and a forge with no such concept produce the same value, and
	// absence of a fact is never a fact (SPEC §9.10). A verifier must refuse
	// this value; only a stated enumeration can satisfy the issue-binding leg.
	RemoteIssueLinkUnknown RemoteIssueLinkStatus = iota
	// RemoteIssueLinkStated is the source positively enumerating the issues the
	// pull request closes. An enumeration that omits the expected issue is
	// evidence *against* the publication, while an enumeration containing it is
	// the positive issue-binding evidence.
	RemoteIssueLinkStated
)

func (s RemoteIssueLinkStatus) String() string {
	switch s {
	case RemoteIssueLinkUnknown:
		return "unknown"
	case RemoteIssueLinkStated:
		return "stated"
	default:
		return "RemoteIssueLinkStatus(" + strconv.Itoa(int(s)) + ")"
	}
}

// RemotePR is leg 3 as a v2 verifier needs it: everything about an open pull
// request that has to *join* — the repository it lives in, the repository its
// head branch lives in, the branch, the commit, the branch it targets, and the
// issues it closes.
//
// Wider than PR, because v1's three fields are only sufficient alongside a local
// worktree. `FindPR` answers "is there an open PR on this branch", and for a run
// BEN hosted that is enough: BEN created the branch, BEN knows the repository,
// and nobody else pushed to it. For a run BEN did not host, each of those is a
// question — a pull request from a fork, against a different target, carrying a
// commit that is not the head BEN observed, is an open pull request on a branch
// of that name and is not this run's publication.
type RemotePR struct {
	Number int
	URL    string
	// State is the provider-native state name; the verifier admits "open" only.
	State string
	// Repository is the credential-free identity of the repository the pull
	// request belongs to — the base side.
	Repository string
	// HeadRepository is where the head branch lives. It differs from Repository
	// for a fork, which is a publication into somebody else's repository and not
	// into the one BEN reads.
	HeadRepository string
	// HeadBranch is the head ref's short name.
	HeadBranch string
	// HeadSHA is the commit the pull request currently proposes.
	HeadSHA string
	// BaseBranch is the branch it targets.
	BaseBranch string
	// LinkStatus says whether LinkedIssues is an enumeration or silence.
	LinkStatus RemoteIssueLinkStatus
	// LinkedIssues are the issue identifiers the pull request closes, meaningful
	// only when LinkStatus is RemoteIssueLinkStated.
	LinkedIssues []string
}

// RemotePRQuery asks a forge for the open pull request published on one branch
// of one repository.
type RemotePRQuery struct {
	// Issue is the issue the claim is over, for adapters whose lookup is
	// issue-scoped and for the audit line.
	Issue Issue
	// Repository is the credential-free identity of the repository to read.
	Repository string
	// Branch is the canonical issue branch.
	Branch string
}

// RemotePRSource is the forge read behind leg 3, discovered by assertion like
// RepositorySource and for the same reason: §8.2's contract is the read kernel
// plus the closed write set, and a tracker with no pull requests at all should
// not owe an answer here.
//
// Read-only, and one method: a verifier one interface away from a write is a
// verifier that could publish the evidence it is judging (SPEC §8.1).
type RemotePRSource interface {
	// RemotePR returns the *open* pull request published on the query's branch,
	// or nil when the forge positively has none.
	//
	// nil means enumerated-and-absent. An implementation that could not complete
	// its enumeration MUST return ErrRemotePRIncomplete rather than nil, and one
	// that found more than one candidate MUST return ErrRemotePRAmbiguous rather
	// than choosing.
	RemotePR(ctx context.Context, q RemotePRQuery) (*RemotePR, error)
}

// ErrRemotePRIncomplete is a forge that could not finish enumerating: a
// pagination cursor that failed halfway, a partial result, a budget spent
// mid-listing.
//
// It exists because the alternative reading is catastrophic and silent. An
// incomplete listing that returns nil says "there is no pull request", which is
// the continuation track's answer — so a publication that is real and merely on
// page two would be re-dispatched to an agent as unfinished work, and if the
// listing keeps failing the same way, forever. The daemon must be able to tell
// "I looked and there is none" from "I could not look".
var ErrRemotePRIncomplete = errors.New("core: the forge could not completely enumerate its pull requests")

// ErrRemotePRAmbiguous is more than one open pull request matching the query.
//
// Not a case for a tie-break rule. Two open pull requests on one branch means
// something the daemon's model does not account for, and picking the newest —
// or the lowest-numbered, or the one whose head matches — would be choosing the
// answer that makes verification pass. Fail closed and let a human look
// (SPEC §9.7).
var ErrPRAmbiguous = errors.New("core: the forge returned more than one open pull request for the branch")

// ErrRemotePRAmbiguous is retained for callers of the v2-specific spelling.
// Both local and remote enumeration now share one cardinality contract.
var ErrRemotePRAmbiguous = ErrPRAmbiguous

// RemoteInvocation is one attempt's provider command as it is submitted to a v2
// execution substrate: the argv the harness is launched with, the environment
// BEN states, and the bytes that reach its standard input.
//
// It is the *provider's own* command and nothing else. Locally an adapter may
// wrap that command in a launcher of this host's — claude-code's `srt` sandbox
// is one, with a generated policy file naming daemon-side paths — and none of
// that may cross this boundary: the paths do not exist in a sandbox, the policy
// describes a machine the run is not on, and the substrate's own worker profile
// owns the one mandatory outer envelope. A wrapper submitted here would be a
// second one inside it.
//
// What is *not* excluded is a sandbox the provider harness enforces itself, from
// inside the same process — codex-exec's `--sandbox workspace-write` and its
// pinned `-c` overrides are part of the command rather than a launcher around
// it, so they travel unchanged (SPEC §7.7).
type RemoteInvocation struct {
	// Argv is the complete command, argv[0] included. It carries no secret:
	// argv is world-readable wherever it runs (SPEC §7.6).
	Argv []string
	// Env is what BEN states about the run, and deliberately only that. A
	// backend sandbox is not the daemon's host and holds none of the daemon's
	// secrets, so composing §7.6's allowlist into it would be exporting this
	// host's environment to another machine rather than protecting anything —
	// the profile defines what a run sees, exactly as it does for a lifecycle
	// hook (docs/REMOTE.md).
	Env map[string]string
	// Stdin is the prompt, which never appears in Argv (SPEC §7.6).
	Stdin []byte
}

// RemoteRunnerKind is the opt-in half of an agent kind that can be dispatched
// onto a v2 execution substrate.
//
// Discovered by assertion, like RepositorySource and ClaimPrincipalSource, and
// refused when absent rather than defaulted. A kind that has not stated how its
// command is composed for a machine BEN does not own has not said its local argv
// is safe to send: the local one may name host paths, a wrapper binary, or a
// generated policy file, and guessing which is which is not something assembly
// may do on an adapter's behalf.
type RemoteRunnerKind interface {
	// RemoteStructural reports whether an otherwise well-formed agent
	// configuration is safe for a v2 execution substrate. It is PURE for the
	// same reason as RunnerKind.Structural, and assembly asks it only when the
	// workflow selects a remote substrate. This separation preserves provider
	// inputs that are valid for a local child while refusing inputs that BEN may
	// not serialize to another machine.
	RemoteStructural(cfg AgentConfig) error
	// RemoteInvocation composes the provider command for one attempt. It is PURE
	// with respect to this host: no filesystem, no subprocess, no environment
	// read, because none of those describes the machine the command will run on.
	RemoteInvocation(cfg AgentConfig, spec RunSpec) (RemoteInvocation, error)
	// RemoteTranslate parses one complete provider output line into normalized
	// events — the same function the local harness translates with, because
	// parsing a provider record is the adapter's business and a second
	// implementation behind the substrate would be a second opinion about what a
	// line means (SPEC §3.6, §7.7).
	RemoteTranslate(line []byte) []Event
	// RemoteCapabilities is what the substrate-hosted runner supports. Asked of
	// the kind rather than of a constructed local runner, which would probe this
	// host for a harness no run here will launch.
	RemoteCapabilities() Capabilities
}
