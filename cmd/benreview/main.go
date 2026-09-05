// Command benreview is the operator's window onto BEN's review controller
// (#11, #204).
//
// It is **not** the availability mechanism, and the distinction is the whole
// reason this file is short. Under #11 the controller was a GitHub Actions
// workflow and this binary was how it ran; under #204 the BEN daemon reconciles
// review work on its ordinary poll/sweep lifecycle, and this is the command an
// operator reaches for to look at one issue, or to push a stuck one along from
// a shell. A daemon that needed somebody to run this would be a daemon that
// stops when nobody is watching.
//
// What it can do is bounded by the same reducer the daemon uses — one package,
// one set of rules, one recovery path (internal/reviewctl). What it cannot do
// is open a review round: the reviewer runs through the daemon's configured
// process backend, in the daemon's sandbox, under the daemon's durable
// execution record, and none of those exist in a short-lived CLI. So this
// reconciles and it reports; it never invokes a model.
//
//	benreview -repo o/r -issue 11 -dry-run   # decide, and perform nothing
//	benreview -repo o/r -issue 11            # finish what an interrupted run owes
//	benreview -repo o/r                      # sweep every candidate
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewctl"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("benreview: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := run(ctx, os.Args[1:], log.Printf)
	if err != nil {
		log.Print(err)
	}
	os.Exit(code)
}

type options struct {
	repo           string
	issue          int
	principal      string
	tracker        string
	controller     string
	queueLabel     string
	requiredLabels []string
	addHuman       bool
	roundCap       int
	api            string
	tokenEnv       string
	timeout        time.Duration
	dryRun         bool
}

// parseOptions is separated from run so a caller's invocation can be checked
// against the flags this binary actually has.
func parseOptions(args []string, out io.Writer) (options, []string, error) {
	fs := flag.NewFlagSet("benreview", flag.ContinueOnError)
	if out != nil {
		fs.SetOutput(out)
	}
	var o options
	fs.StringVar(&o.repo, "repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name of the repository")
	fs.IntVar(&o.issue, "issue", 0, "the issue to drive; 0 sweeps every candidate")
	fs.StringVar(&o.principal, "principal", os.Getenv("BEN_PRINCIPAL"), "BEN's claim assignee — the only login this controller may unassign")
	fs.StringVar(&o.tracker, "tracker-author", os.Getenv("BEN_TRACKER_AUTHOR"), "the login BEN posts milestone comments as")
	fs.StringVar(&o.controller, "controller", os.Getenv("BEN_CONTROLLER"), "the login this controller publishes as")
	fs.StringVar(&o.queueLabel, "queue-label", envOr("BEN_QUEUE_LABEL", "ben-queue"), "the human-applied required label; only ever removed")
	fs.Var((*labelList)(&o.requiredLabels), "required-label", "one label in the complete approval set; repeat for multiple labels (defaults to -queue-label)")
	fs.BoolVar(&o.addHuman, "add-human-review-label", false, "add the fixed, non-required human-review label on terminal routes")
	fs.IntVar(&o.roundCap, "round-cap", 3, "how many distinct reviewed heads one approval cycle may spend")
	fs.StringVar(&o.api, "api", envOr("GITHUB_API_URL", "https://api.github.com"), "GitHub API root")
	fs.StringVar(&o.tokenEnv, "token-env", "BEN_REVIEW_TOKEN", "environment variable holding the controller's token")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "per-request timeout")
	fs.BoolVar(&o.dryRun, "dry-run", false, "observe and decide, but mutate nothing")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: benreview [flags]\n\n"+
			"Reconciles BEN's review controller for one issue, or sweeps every candidate.\n"+
			"It never invokes a reviewer: opening a round belongs to the daemon, which\n"+
			"holds the process backend, the sandbox and the durable execution record.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return o, nil, err
	}
	if len(o.requiredLabels) == 0 {
		o.requiredLabels = []string{o.queueLabel}
	}
	return o, fs.Args(), nil
}

type labelList []string

func (l *labelList) String() string { return strings.Join(*l, ",") }

func (l *labelList) Set(value string) error {
	*l = append(*l, strings.TrimSpace(value))
	return nil
}

func run(ctx context.Context, args []string, logf func(string, ...any)) (int, error) {
	o, extra, err := parseOptions(args, nil)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, nil
	}
	if len(extra) > 0 {
		// #11 accepted a reviewer argv after `--`. It is gone rather than
		// ignored: a command that silently dropped the reviewer it was handed
		// would look like it had run one.
		return 2, fmt.Errorf("unexpected arguments %v: this command reconciles and never invokes a reviewer; "+
			"configure the reviewer under `review:` in WORKFLOW.md and let `ben run` open rounds", extra)
	}

	owner, repoName, ok := strings.Cut(o.repo, "/")
	if !ok || owner == "" || repoName == "" {
		return 2, fmt.Errorf("-repo must be owner/name, got %q", o.repo)
	}

	token := os.Getenv(o.tokenEnv)
	if token == "" && !o.dryRun {
		return 2, fmt.Errorf("$%s is empty; the controller cannot publish anything", o.tokenEnv)
	}

	controller, err := reviewctl.New(reviewctl.Options{
		Policy: review.Config{
			Owner:               owner,
			Repo:                repoName,
			Issue:               o.issue,
			Principal:           o.principal,
			TrackerAuthor:       o.tracker,
			Controller:          o.controller,
			RequiredLabels:      append([]string(nil), o.requiredLabels...),
			QueueLabel:          o.queueLabel,
			AddHumanReviewLabel: o.addHuman,
			RoundCap:            o.roundCap,
		},
		Forge:  reviewctl.NewClient(o.api, token, owner, repoName, o.timeout).WithLog(logf),
		Log:    logf,
		DryRun: o.dryRun,
	})
	if err != nil {
		return 2, err
	}

	if o.issue == 0 {
		if err := controller.Sweep(ctx); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if err := controller.Reconcile(ctx, o.issue); err != nil {
		return 1, err
	}
	return 0, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
