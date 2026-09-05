package main

import (
	"context"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// Every refusal below happens before a single request is made. A controller
// that discovers it is misconfigured after it has already unassigned somebody
// is worse than one that never started.
func TestRunRefusesBeforeItTouchesAnything(t *testing.T) {
	base := []string{
		"-repo", "acme/ben",
		"-issue", "11",
		"-principal", "ben-claim-bot",
		"-tracker-author", "ben-tracker-bot",
		"-controller", "ben-review-bot",
	}
	with := func(extra ...string) []string {
		out := append([]string(nil), base...)
		return append(out, extra...)
	}

	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "no repository",
			args: []string{"-issue", "11"},
			want: "owner/name",
		},
		{
			name: "a repository without an owner",
			args: []string{"-repo", "ben", "-issue", "11"},
			want: "owner/name",
		},
		{
			name: "the controller is also the claim principal",
			args: with("-controller", "ben-claim-bot"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "unassign itself",
		},
		{
			name: "the controller is also the milestone author",
			args: with("-controller", "ben-tracker-bot"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "trigger its own rounds",
		},
		{
			name: "no principal to unassign",
			args: []string{"-repo", "acme/ben", "-issue", "11", "-tracker-author", "t", "-controller", "c"},
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "missing principal",
		},
		{
			name: "the informational label is the required one",
			args: with("-queue-label", review.HumanReviewLabel, "-add-human-review-label"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "would be an approval",
		},
		{
			name: "a BEN state label is the required one",
			args: with("-queue-label", "BEN:needs-review"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "reserved state-label namespace",
		},
		{
			name: "the removable label is absent from the complete approval set",
			args: with("-required-label", "security-approved"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "not in the complete required-label set",
		},
		{
			name: "a round cap of zero",
			args: with("-round-cap", "0"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "leaves no round",
		},
		{
			name: "no token",
			args: with(),
			want: "cannot publish",
		},
		{
			// #11 accepted a reviewer argv here. #204 moved rounds to the daemon,
			// and a command that silently dropped the reviewer it was handed would
			// look like it had run one.
			name: "a reviewer argv this command cannot honour",
			args: append(with(), "--", "/bin/true"),
			env:  map[string]string{"BEN_REVIEW_TOKEN": "t"},
			want: "never invokes a reviewer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEN_REVIEW_TOKEN", "")
			t.Setenv("GITHUB_REPOSITORY", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			code, err := run(context.Background(), tc.args, t.Logf)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (a usage refusal); err = %v", code, err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestOperatorCommandAcceptsTheCompleteRequiredLabelSet(t *testing.T) {
	o, _, err := parseOptions([]string{
		"-required-label", "ben-queue",
		"-required-label", "security-approved",
	}, discard{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.requiredLabels, ","); got != "ben-queue,security-approved" {
		t.Fatalf("required labels = %q", got)
	}
}

// The operator command has no reviewer at all, by construction: it holds no
// process backend, no sandbox and no durable execution record, so there is
// nothing for it to open a round with. Asserted against the flag set, because
// the way this regresses is somebody adding the flag back.
func TestTheOperatorCommandHasNoReviewerFlags(t *testing.T) {
	for _, forbidden := range []string{"-reviewer-env", "-reviewer-timeout", "-reviewer"} {
		t.Setenv("BEN_REVIEW_TOKEN", "t")
		if _, _, err := parseOptions([]string{forbidden, "x"}, discard{}); err == nil {
			t.Errorf("%s parses; opening a review round belongs to `ben run`", forbidden)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
