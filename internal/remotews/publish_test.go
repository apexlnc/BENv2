package remotews_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

func TestTrustedPublishWaitsForCodingDomainQuietAndReplaysOneOperation(t *testing.T) {
	r := newRig(t)
	r.mirror.SetTargetBranch("release/v2")
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	r.dispatch("coding-run")
	r.backend.Complete("coding-run")
	r.backend.Reap("coding-run")

	if err := r.provider.Publish(t.Context(), r.issue, ws); !errors.Is(err, remote.ErrNotQuiet) {
		t.Fatalf("Publish before domain quiet = %v, want ErrNotQuiet", err)
	}
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			t.Fatal("publish command ran before the coding domain was quiet")
		}
	}

	r.backend.SetDomainQuiet("coding-run")
	head := strings.Repeat("b", 40)
	r.backend.SetHookSpecResult(func(spec remote.HookSpec) (remote.HookResult, error) {
		if spec.Git.Phase != remote.GitPhasePublish {
			return remote.HookResult{}, nil
		}
		return remote.HookResult{Output: fmt.Sprintf(
			`{"status":"published","pr_url":"https://example.test/pull/1","branch":%q,"head_sha":%q}`,
			spec.Git.Branch, head,
		)}, nil
	})
	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish after domain quiet: %v", err)
	}

	var calls []struct {
		id   remote.HookID
		argv []string
		git  remote.GitScope
	}
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			calls = append(calls, struct {
				id   remote.HookID
				argv []string
				git  remote.GitScope
			}{call.ID, call.Argv, call.Git})
		}
	}
	if len(calls) != 1 {
		t.Fatalf("publish calls = %d, want one: %+v", len(calls), r.backend.Hooks())
	}
	first := calls[0]
	if len(first.argv) < 2 || !slices.Equal(first.argv[:2], []string{"/usr/local/bin/airlock-git", "publish"}) {
		t.Fatalf("publish argv = %v", first.argv)
	}
	wantScope := remote.GitScope{
		Phase: remote.GitPhasePublish, Repository: gitRepository, Branch: ws.Branch,
		BaseCommit: ws.BaseSHA, BaseBranch: "release/v2", Operation: first.git.Operation,
	}
	if first.git != wantScope || first.git.Operation == "" {
		t.Fatalf("publish scope = %+v, want %+v with operation", first.git, wantScope)
	}

	// A verifier retry resolves the same durable command. No second process is
	// created, and therefore the exact same scoped operation key is replayed.
	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish replay: %v", err)
	}
	var replayed int
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			replayed++
			if call.ID != first.id || call.Git.Operation != first.git.Operation {
				t.Fatalf("publish replay moved identity or operation: first=%+v replay=%+v", first, call)
			}
		}
	}
	if replayed != 1 {
		t.Fatalf("publish replay created %d commands, want one", replayed)
	}
}

func TestTrustedPublishRequiresThePinnedTargetTuple(t *testing.T) {
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	ws.TargetBranch = "other-target"

	if err := r.provider.Publish(t.Context(), r.issue, ws); !errors.Is(err, remotews.ErrCycleState) {
		t.Fatalf("Publish with mismatched target = %v, want ErrCycleState", err)
	}
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			t.Fatal("publish command ran under a workspace whose target contradicted the durable cycle")
		}
	}
}

func TestProtectedPublicationUsesAFreshRunAfterApprovalWithTheSameOperation(t *testing.T) {
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	head := strings.Repeat("c", 40)
	var publishRounds int
	r.backend.SetHookSpecResult(func(spec remote.HookSpec) (remote.HookResult, error) {
		if spec.Git.Phase != remote.GitPhasePublish {
			return remote.HookResult{}, nil
		}
		publishRounds++
		if publishRounds == 1 {
			return remote.HookResult{Output: `{"status":"pending","approval_id":"approval-1","protected_paths":["AGENTS.md"]}`}, nil
		}
		return remote.HookResult{Output: fmt.Sprintf(
			`{"status":"published","pr_url":"https://example.test/pull/1","branch":%q,"head_sha":%q}`,
			spec.Git.Branch, head,
		)}, nil
	})
	err := r.provider.Publish(t.Context(), r.issue, ws)
	if !errors.Is(err, remotews.ErrPublishApprovalRequired) || !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("Publish protected graph = %v", err)
	}
	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish after approval: %v", err)
	}
	var calls []remotetest.HookCall
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			calls = append(calls, call)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("publish runs = %d, want pending plus fresh approved run: %+v", len(calls), calls)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("pending and approved publication reused run identity %q", calls[0].ID)
	}
	if calls[0].Git.Operation == "" || calls[0].Git.Operation != calls[1].Git.Operation {
		t.Fatalf("publication operation changed across approval: %q then %q",
			calls[0].Git.Operation, calls[1].Git.Operation)
	}
	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish completed replay: %v", err)
	}
	var afterReplay int
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			afterReplay++
		}
	}
	if afterReplay != 2 {
		t.Fatalf("completed publication created another run: hooks=%+v", r.backend.Hooks())
	}
}

func TestFailedPublicationUsesAFreshRunAfterCodingRetryWithTheSameOperation(t *testing.T) {
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	head := strings.Repeat("d", 40)
	var publishRounds int
	r.backend.SetHookSpecResult(func(spec remote.HookSpec) (remote.HookResult, error) {
		if spec.Git.Phase != remote.GitPhasePublish {
			return remote.HookResult{}, nil
		}
		publishRounds++
		if publishRounds == 1 {
			return remote.HookResult{ExitCode: 1, Output: "publish repository has no new commit"}, nil
		}
		return remote.HookResult{Output: fmt.Sprintf(
			`{"status":"published","pr_url":"https://example.test/pull/1","branch":%q,"head_sha":%q}`,
			spec.Git.Branch, head,
		)}, nil
	})

	if err := r.provider.Publish(t.Context(), r.issue, ws); !errors.Is(err, remote.ErrHookFailed) {
		t.Fatalf("Publish before the coding retry = %v, want hook failure", err)
	}
	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish after the coding retry: %v", err)
	}

	var calls []remotetest.HookCall
	for _, call := range r.backend.Hooks() {
		if call.Phase == remote.HookGitPublish {
			calls = append(calls, call)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("publish runs = %d, want failed plus fresh retry: %+v", len(calls), calls)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("failed and retried publication reused run identity %q", calls[0].ID)
	}
	if calls[0].Git.Operation == "" || calls[0].Git.Operation != calls[1].Git.Operation {
		t.Fatalf("publication operation changed across the coding retry: %q then %q",
			calls[0].Git.Operation, calls[1].Git.Operation)
	}

	if err := r.provider.Publish(t.Context(), r.issue, ws); err != nil {
		t.Fatalf("Publish completed replay: %v", err)
	}
	if publishRounds != 2 {
		t.Fatalf("completed publication replay executed %d rounds, want 2", publishRounds)
	}
}
