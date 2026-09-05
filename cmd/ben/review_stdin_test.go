package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewctl"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The #284 boundary, reproduced with the real pieces: the reviewer argv the
// deployment configures, the prompt BEN composes around it, the guidance the
// deployment ships, a diff the size of Airlock PR #606's, and the contract
// fake publishing the deployed profile's 64 KiB inline bound.
//
// The diff fits under the bound. The complete prompt does not. Before this
// ticket that was a 413 on every sweep and a review loop that appeared to skip
// the pull request; now the prompt streams, the run receives every byte, and
// the same session completes the handoff to a verdict.
func TestAReviewPromptLargerThanTheInlineBoundReachesTheReviewerByStreaming(t *testing.T) {
	guidance, err := os.ReadFile("../../docs/REVIEW-GUIDANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	const (
		diffBytes = 64633 // Airlock PR #606's raw diff
		base      = "8a2cdef813bee40d246eafeae86519e38a756ce9"
		head      = "be61e5d38fd2ba507ab094fbee36f940467eb671"
	)
	line := "+	// one more line of the publication lifecycle store\n"
	diff := strings.Repeat(line, diffBytes/len(line)+1)[:diffBytes]
	sub := reviewrun.Subject{
		Repository: "srhg-ai-7cef3f93/airlock", Issue: "601",
		Cycle: 30519285000, Occurrence: 30520400724, Claim: 30519285437,
		PR: 606, TargetBranch: "main", Base: base, Head: head, Diff: diff,
	}
	argv := []string{"codex", "exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "-"}
	compose := reviewctl.ProfiledInvocations(argv, nil, string(guidance))
	req, err := compose(sub)
	if err != nil {
		t.Fatal(err)
	}
	inline := airlocktest.DefaultLimits.Inline
	if int64(len(sub.Diff)) >= inline || int64(len(req.Stdin)) <= inline {
		t.Fatalf("the fixture is not at the boundary: diff %d bytes, prompt %d bytes, inline bound %d",
			len(sub.Diff), len(req.Stdin), inline)
	}

	srv := airlocktest.New(t)
	substrate, err := airlock.New(airlock.Options{
		BaseURL: "https://airlock.test", Auth: staticSource{value: airlocktest.DefaultToken},
		Profile: airlocktest.DefaultProfile, Store: airlock.NewDirStore(t.TempDir()), Transport: srv.Transport(),
		Timeouts: airlock.Timeouts{Request: 5 * time.Second, Poll: 5 * time.Second, PollWait: time.Second, Settle: 2 * time.Second, Retries: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := substrate.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if limits := substrate.StdinLimits(); !limits.Known() || limits.Inline != inline {
		t.Fatalf("Ready did not read the profile's stdin envelope: %+v", limits)
	}
	// The workspace cycle's sandbox, exactly as the coding attempt would have
	// acquired it: the review runs where the code was written.
	id, err := substrate.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim:  remote.Claim{Repository: sub.Repository, Issue: sub.Issue, Epoch: sub.Cycle},
		Branch: "ben/601", BaseSHA: sub.Base,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	exec, err := reviewrun.NewRemote(reviewrun.RemoteOptions{
		Backend: substrate.Processes, GitRepository: sub.Repository,
		Limits: core.RunLimits{StallTimeout: 20 * time.Minute, AttemptTimeout: 20 * time.Minute},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	polls := 0
	session, err := reviewrun.New(reviewrun.Options{
		Executor: exec, Store: reviewrun.NewDirStore(t.TempDir()), Compose: compose,
		Sandbox: func(context.Context, reviewrun.Subject) (reviewrun.Placement, error) {
			return reviewrun.Placement{
				Branch: "ben/601", BaseSHA: sub.Base, TargetBranch: "main",
				Sandbox: id.SandboxID, Profile: id.ProfileRevision,
			}, nil
		},
		Poll: time.Millisecond, Deadline: 10 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if polls++; polls > 50 {
				return context.DeadlineExceeded
			}
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The reviewer, standing in for Codex: it answers only once the whole
	// prompt has arrived and stdin has been closed on it — which is the fact the
	// incident's run never observed.
	verdict := "reading the diff now\n" + reviewrun.VerdictOpen + "\n" +
		`{"verdict":"changes_requested","findings":"approvals and receipts are joined by operation key alone"}` +
		"\n" + reviewrun.VerdictClose + "\n"
	answered := make(chan string, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if runs := srv.RunIDs(); len(runs) == 1 && srv.StdinClosed(runs[0]) {
				srv.Emit(runs[0], "stdout", []byte(verdict))
				srv.Terminate(runs[0], airlocktest.Exited(0))
				answered <- runs[0]
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(answered)
	}()

	// The handoff, end to end: start, stream the prompt, close stdin, read the
	// verdict from the same durable run.
	report, err := session.Review(ctx, sub)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if report.Verdict != review.VerdictChangesRequested {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	run, ok := <-answered
	if !ok {
		t.Fatal("the reviewer never saw a run with its stdin closed")
	}
	if got := srv.RunIDs(); len(got) != 1 || got[0] != run {
		t.Fatalf("runs = %v, want exactly the one the reviewer answered", got)
	}
	if mode := srv.StdinMode(run); mode != "streaming" {
		t.Fatalf("stdin mode = %q, want streaming for a %d-byte prompt over a %d-byte inline bound", mode, len(req.Stdin), inline)
	}
	if !bytes.Equal(srv.Stdin(run), req.Stdin) {
		t.Fatalf("the reviewer received %d of the prompt's %d bytes", len(srv.Stdin(run)), len(req.Stdin))
	}
	if strings.Contains(string(srv.Stdin(run)), airlocktest.DefaultToken) {
		t.Fatal("the prompt carried the substrate credential")
	}

	// The next sweep resumes the stated verdict from the record, and asks the
	// backend for nothing.
	requests := len(srv.Requests())
	again, err := session.Review(ctx, sub)
	if err != nil || again.Verdict != report.Verdict {
		t.Fatalf("resumed Review = (%+v, %v)", again, err)
	}
	if got := len(srv.Requests()); got != requests {
		t.Fatalf("resuming a stated verdict made %d backend requests", got-requests)
	}
}

// The configuration fact the assembly can know before the first live review:
// whether the largest prompt review.max_diff_bytes permits can be delivered at
// all under the profile's total stdin bound.
func TestAReviewBoundTheProfileCannotDeliverIsRefusedAtAssembly(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	def := writeAirlockWorkflow(t, airlockWorkflow)
	def.Config.Review.MaxDiffBytes = config.DefaultReviewMaxDiffBytes
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ceiling := reviewctl.PromptCeiling("acme/widgets", "guidance", def.Config.Review.MaxDiffBytes)

	for name, tc := range map[string]struct {
		limits airlock.StdinLimits
		refuse bool
	}{
		"unknown limits check nothing":       {limits: airlock.StdinLimits{}, refuse: false},
		"an unbounded total admits anything": {limits: airlock.StdinLimits{Inline: 65536, Chunk: 65536, Total: 0}, refuse: false},
		"the deployed profile admits it":     {limits: airlock.StdinLimits{Inline: 65536, Chunk: 65536, Total: 16777216}, refuse: false},
		"a total under the ceiling refuses":  {limits: airlock.StdinLimits{Inline: 65536, Chunk: 65536, Total: int64(ceiling) - 1}, refuse: true},
		"a total at the ceiling admits":      {limits: airlock.StdinLimits{Inline: 65536, Chunk: 65536, Total: int64(ceiling)}, refuse: false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkReviewPromptDelivery(log, def, tc.limits, "acme/widgets", "guidance")
			if !tc.refuse {
				if err != nil {
					t.Fatalf("checkReviewPromptDelivery = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrConstruct) {
				t.Fatalf("checkReviewPromptDelivery = %v, want ErrConstruct", err)
			}
			for _, must := range []string{"review.max_diff_bytes 400000", airlocktest.DefaultProfile, "max_stdin_total_bytes"} {
				if !strings.Contains(err.Error(), must) {
					t.Errorf("the refusal does not name %q: %v", must, err)
				}
			}
		})
	}
}
