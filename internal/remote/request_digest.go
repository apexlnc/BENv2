package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Request digests are durable wire formats, not hashes of Go's current struct
// layout. The v1 shapes below exactly reproduce the pre-Git-capability JSON so
// an empty extension remains readable before and after a rolling deployment.
// Requests that use direct argv or a Git scope carry an explicit v2 field in
// their preimage and therefore cannot be mistaken for a legacy request.
const requestDigestVersion2 = 2

type requestDigestClaim struct {
	Repository string
	Issue      string
	Epoch      int64
}

type requestDigestIdentity struct {
	Claim           requestDigestClaim
	Branch          string
	BaseSHA         string
	SandboxID       string
	ProfileRevision string
}

type requestDigestLimits struct {
	StallTimeout   time.Duration
	AttemptTimeout time.Duration
	MaxTurns       int
	MaxCostUSD     float64
}

type requestDigestGitV2 struct {
	Phase          GitPhase
	Repository     string
	Branch         string
	BaseCommit     string
	BaseBranch     string
	CheckoutCommit string
	Operation      string
}

type processRequestDigestV1 struct {
	Identity requestDigestIdentity
	Argv     []string
	Env      map[string]string
	Stdin    []byte
	Limits   requestDigestLimits
}

type processRequestDigestV2 struct {
	Version  int
	Identity requestDigestIdentity
	Argv     []string
	Env      map[string]string
	Stdin    []byte
	Limits   requestDigestLimits
	Git      requestDigestGitV2
}

type hookRequestDigestV1 struct {
	Identity requestDigestIdentity
	Phase    HookPhase
	Attempt  int
	Script   string
	Timeout  time.Duration
}

type hookRequestDigestV2 struct {
	Version  int
	Identity requestDigestIdentity
	Phase    HookPhase
	Attempt  int
	Script   string
	Argv     []string
	Git      requestDigestGitV2
	Timeout  time.Duration
}

func processRequestDigestPayload(spec ProcessSpec) any {
	identity := requestDigestIdentityOf(spec.Identity)
	limits := requestDigestLimitsOf(spec.Limits)
	if spec.Git.Empty() {
		return processRequestDigestV1{
			Identity: identity, Argv: spec.Argv, Env: spec.Env, Stdin: spec.Stdin, Limits: limits,
		}
	}
	return processRequestDigestV2{
		Version: requestDigestVersion2, Identity: identity,
		Argv: spec.Argv, Env: spec.Env, Stdin: spec.Stdin, Limits: limits, Git: requestDigestGitOf(spec.Git),
	}
}

func hookRequestDigestPayload(spec HookSpec) any {
	identity := requestDigestIdentityOf(spec.Identity)
	if len(spec.Argv) == 0 && spec.Git.Empty() {
		return hookRequestDigestV1{
			Identity: identity, Phase: spec.Phase, Attempt: spec.Attempt,
			Script: spec.Script, Timeout: spec.Timeout,
		}
	}
	return hookRequestDigestV2{
		Version: requestDigestVersion2, Identity: identity,
		Phase: spec.Phase, Attempt: spec.Attempt, Script: spec.Script,
		Argv: spec.Argv, Git: requestDigestGitOf(spec.Git), Timeout: spec.Timeout,
	}
}

func requestDigestIdentityOf(identity Identity) requestDigestIdentity {
	return requestDigestIdentity{
		Claim: requestDigestClaim{
			Repository: identity.Claim.Repository,
			Issue:      identity.Claim.Issue,
			Epoch:      identity.Claim.Epoch,
		},
		Branch: identity.Branch, BaseSHA: identity.BaseSHA,
		SandboxID: identity.SandboxID, ProfileRevision: identity.ProfileRevision,
	}
}

func requestDigestLimitsOf(limits core.RunLimits) requestDigestLimits {
	return requestDigestLimits{
		StallTimeout: limits.StallTimeout, AttemptTimeout: limits.AttemptTimeout,
		MaxTurns: limits.MaxTurns, MaxCostUSD: limits.MaxCostUSD,
	}
}

func requestDigestGitOf(scope GitScope) requestDigestGitV2 {
	return requestDigestGitV2(scope)
}

func marshalRequestDigest(payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
