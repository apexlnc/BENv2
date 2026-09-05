package remotews

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// ErrPublishApprovalRequired is the broker's durable protected-file decision.
// The orchestrator keeps the claim in verification and retries after a trusted
// approval instead of treating policy as a coding failure or silently changing
// the proposed graph.
var ErrPublishApprovalRequired = core.ErrPublishApprovalPending

type publishResult struct {
	Status         string   `json:"status"`
	PRURL          string   `json:"pr_url"`
	Branch         string   `json:"branch"`
	HeadSHA        string   `json:"head_sha"`
	ApprovalID     string   `json:"approval_id"`
	ProtectedPaths []string `json:"protected_paths"`
}

// Publish runs the credential-free packager only after the coding run is
// terminal and domain-quiet. The operation key is stable for this exact claim
// attempt, so replaying a lost response converges on the broker's receipt.
func (p *Provider) Publish(ctx context.Context, issue core.Issue, ws core.Workspace) error {
	c, err := p.load(ws.Key, issue.Identifier)
	if err != nil {
		return err
	}
	if c.State != cyclePinned || c.Epoch != ws.ClaimEpoch || c.BaseSHA != ws.BaseSHA ||
		c.TargetBranch == "" || c.TargetBranch != ws.TargetBranch || c.Address() != ws.Path {
		return fmt.Errorf("%w: publication workspace does not match the pinned claim", ErrCycleState)
	}
	if _, hadRun, err := p.settle(ctx, c); err != nil {
		return fmt.Errorf("remotews: coding run is not quiet before publication: %w", err)
	} else if hadRun {
		p.log.Debug("retired the terminal coding run before publication", "issue", c.Issue, "attempt", c.Attempt)
	}
	identity, err := p.identity(ctx, c)
	if err != nil {
		return err
	}
	scope := p.gitScope(c, remote.GitPhasePublish)
	scope.Operation = publishOperation(c)

	// A pending response is a completed process, so its hook journal must stay
	// immutable. On the next verification pass, replay each completed pending
	// result and allocate the first unused round. That gives approval a fresh
	// active-run authorization while keeping the broker operation key fixed.
	for round := 0; ; round++ {
		id := publishHookID(c, round)
		record, loadErr := p.hookStore.LoadHook(remote.HookKey{Claim: c.Claim(), ID: id})
		existed := loadErr == nil
		completed := existed && record.Result != nil
		if loadErr != nil && !errors.Is(loadErr, remote.ErrNoRecord) {
			return fmt.Errorf("remotews: inspect publish round %d: %w", round, loadErr)
		}
		result, err := remote.RunCommand(ctx, p.hookExec, p.hookStore, remote.CommandInvocation{
			Identity: identity,
			ID:       id,
			Phase:    remote.HookGitPublish,
			Attempt:  c.Attempt,
			Argv: []string{
				"/usr/local/bin/airlock-git", "publish",
				"--title", publishTitle(issue),
				"--body", "Closes #" + issue.Identifier,
			},
			Git:     scope,
			Timeout: p.hooks.Timeout,
		})
		if err != nil {
			// A completed command is immutable. Replaying it is how a lost
			// response converges, but a later coding retry may have corrected the
			// workspace after that command failed. Allocate a fresh process round
			// while retaining scope.Operation as the broker idempotency key. Never
			// skip an incomplete record: it may still name a live process whose
			// exact replay must be reconciled before another one can start.
			if completed {
				continue
			}
			return err
		}
		published, err := decodePublishResult(result.Output)
		if err != nil {
			return err
		}
		switch published.Status {
		case "published":
			if published.Branch != c.Branch || !fullSHA(published.HeadSHA) || published.PRURL == "" {
				return fmt.Errorf("%w: airlock-git publish returned an incomplete or mismatched publication", remote.ErrHookMismatch)
			}
			return nil
		case "pending":
			if published.ApprovalID == "" || len(published.ProtectedPaths) == 0 {
				return fmt.Errorf("%w: airlock-git publish returned an incomplete approval", remote.ErrHookMismatch)
			}
			if existed {
				continue
			}
			return fmt.Errorf("%w: %s", ErrPublishApprovalRequired, strings.Join(published.ProtectedPaths, ", "))
		default:
			return fmt.Errorf("%w: airlock-git publish returned status %q", remote.ErrHookMismatch, published.Status)
		}
	}
}

func decodePublishResult(output string) (publishResult, error) {
	var published publishResult
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&published); err != nil {
		return publishResult{}, fmt.Errorf("%w: decoding airlock-git publish result: %v", remote.ErrHookFailed, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return publishResult{}, fmt.Errorf("%w: airlock-git publish returned trailing output", remote.ErrHookFailed)
	}
	return published, nil
}

func publishHookID(c Cycle, round int) remote.HookID {
	return remote.HookID(fmt.Sprintf("%d-%s-%d-%d", c.Epoch, remote.HookGitPublish, c.Attempt, round))
}

func publishOperation(c Cycle) string {
	h := sha256.New()
	for _, value := range []string{c.Repository, c.Issue, fmt.Sprint(c.Approval), fmt.Sprint(c.Epoch), fmt.Sprint(c.Attempt), c.BaseSHA} {
		fmt.Fprintf(h, "%d:%s\n", len(value), value)
	}
	return "ben-publish-" + hex.EncodeToString(h.Sum(nil))[:32]
}

func publishTitle(issue core.Issue) string {
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = "Issue " + issue.Identifier
	}
	title = "BEN: " + title
	const maxBytes = 240
	if len(title) <= maxBytes {
		return title
	}
	title = title[:maxBytes]
	for !utf8.ValidString(title) {
		title = title[:len(title)-1]
	}
	return title
}

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
