package reviewctl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// Options are what one controller is constructed from.
//
// Policy is [review.Config] and nothing is restated here; what this adds is the
// three things the reducer has no opinion about — who to ask the forge, who to
// ask for a verdict, and which repository identity the workspace cycle is keyed
// by.
type Options struct {
	// Policy is the reducer's configuration. Issue is per-target and is filled
	// in per reconciliation; every other field is validated once at
	// construction, so a sweep does not discover a misconfiguration on its
	// fourth issue.
	Policy review.Config
	// Forge is the trusted process's forge client, holding the controller
	// credential. It is the only credentialed thing in this package.
	Forge Forge
	// Reviewer turns a subject into a verdict. Nil builds a reconcile-only
	// controller, which resumes routing and repairs markers but opens no round.
	Reviewer Reviewer
	// Repository is the credential-free repository identity the workspace cycle
	// is keyed by (mirror.Mirror.Repository()). Defaults to `owner/repo`.
	Repository string
	// MaxDiffBytes is the configured ceiling on bytes handed to the reviewer.
	// Zero selects the compatibility default.
	MaxDiffBytes int
	// Log is where decisions go. The controller's ordinary answer is "nothing",
	// and a reconciliation that says nothing without saying why is
	// indistinguishable from one that is broken.
	Log func(string, ...any)
	// DryRun observes and decides but mutates nothing, on the forge or the
	// substrate.
	DryRun bool
}

// Controller drives issues to a settled state.
type Controller struct {
	opts Options

	// Sweep is one serial reconciliation pass in the daemon. pending retains
	// every discovered candidate that a forge refusal left unsettled.
	// retryBudgetAt gives a candidate that exhausted a shared discovery pass
	// one full-budget retry; if that also exhausts, it moves behind its peers so
	// an oversized observation cannot permanently starve them (#239).
	sweepMu       sync.Mutex
	pending       []int
	retryBudgetAt int
}

// DefaultMaxDiffBytes preserves the controller's original bound for callers
// that do not expose a configuration knob (notably the operator command).
const DefaultMaxDiffBytes = 400_000

// New validates the policy and the seams.
//
// The identity checks are the load-bearing ones and they live in
// review.Config.Validate: a controller that is also the claim principal would
// review and unassign itself, and one that is also the tracker author could
// manufacture its own triggers. Both are refusals here rather than warnings.
func New(opts Options) (*Controller, error) {
	if opts.Forge == nil {
		return nil, errors.New("reviewctl: a controller needs a forge client")
	}
	probe := opts.Policy
	if probe.Issue == 0 {
		probe.Issue = 1
	}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	if opts.Reviewer == nil {
		opts.Reviewer = noReviewer{}
	}
	if opts.Repository == "" {
		opts.Repository = opts.Policy.Owner + "/" + opts.Policy.Repo
	}
	if opts.MaxDiffBytes == 0 {
		opts.MaxDiffBytes = DefaultMaxDiffBytes
	}
	if opts.MaxDiffBytes < 0 {
		return nil, errors.New("reviewctl: max diff bytes must be positive")
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	return &Controller{opts: opts}, nil
}

// Reconcile drives one issue: observe, reduce, execute, re-observe, repeat
// until there is nothing owed.
func (c *Controller) Reconcile(ctx context.Context, issue int) error {
	if issue <= 0 {
		return fmt.Errorf("reviewctl: %d is not an issue number", issue)
	}
	cfg := c.opts.Policy
	cfg.Issue = issue
	d := &driver{
		cfg: cfg, forge: c.opts.Forge, repository: c.opts.Repository,
		reviewer: c.opts.Reviewer, maxDiffBytes: c.opts.MaxDiffBytes,
		log: c.opts.Log, dryRun: c.opts.DryRun,
	}
	return d.Run(ctx)
}

// Sweep reconciles every candidate the forge names.
//
// This is the daemon's normal path and the reason there is no event
// subscription anywhere in this package: delivery is a wake-up that can be
// missed, the durable state is on the forge either way, and a controller whose
// availability depended on a webhook would be a controller that silently stops.
//
// One issue's failure must not strand the others. A sweep that stopped at the
// first error would be a reconciler that only ever fixes the first thing wrong.
// The exceptions are the forge's own refusals to spend (gate.go): under a
// standing backoff or a spent budget every remaining candidate fails
// identically without reaching the network, so the sweep stops at the first
// and names what it left unvisited. A rate-limited candidate stays first
// because no peer can reach the network either. A budget-limited candidate
// gets one retry without discovery, hence the whole allowance; exhausting
// that too proves no progress is possible in one sweep and rotates it behind
// its pending peers (#239).
func (c *Controller) Sweep(ctx context.Context) error {
	c.sweepMu.Lock()
	defer c.sweepMu.Unlock()

	dedicatedRetry := c.retryBudgetAt != 0
	var targets []int
	if dedicatedRetry {
		resetSweepBudget(c.opts.Forge)
	} else {
		var err error
		targets, err = c.opts.Forge.Candidates(ctx, c.opts.Policy.QueueLabel)
		c.pending = appendPending(c.pending, targets)
		if err != nil {
			if errors.Is(err, ErrSweepBudget) && len(c.pending) > 0 {
				c.retryBudgetAt = c.pending[0]
			}
			return err
		}
	}
	total := len(c.pending)
	c.opts.Log("review sweep: %d candidate issue(s) pending (%d discovered in this slice)", total, len(targets))

	var failures []string
	for len(c.pending) > 0 {
		n := c.pending[0]
		if err := c.Reconcile(ctx, n); err != nil {
			if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrSweepBudget) {
				unvisited := len(c.pending) - 1
				if errors.Is(err, ErrSweepBudget) {
					if dedicatedRetry && n == c.retryBudgetAt {
						c.retryBudgetAt = 0
						if len(c.pending) > 1 {
							c.pending = append(c.pending[1:], n)
							c.opts.Log("review sweep: issue #%d exhausted a dedicated request budget; rotated behind %d pending candidate(s)", n, unvisited)
						}
					} else {
						c.retryBudgetAt = n
					}
				}
				return fmt.Errorf("sweep stopped at issue #%d with %d of %d candidate(s) unvisited: %w",
					n, unvisited, len(c.pending), err)
			}
			failures = append(failures, fmt.Sprintf("#%d: %v", n, err))
			c.opts.Log("issue #%d: %v", n, err)
		}
		if ctx.Err() != nil {
			// Repeating one issue after cancellation is harmless; skipping an
			// interrupted mutation is not. The next pass re-observes its facts.
			return ctx.Err()
		}
		if n == c.retryBudgetAt {
			c.retryBudgetAt = 0
			dedicatedRetry = false
		}
		c.pending = c.pending[1:]
	}
	c.retryBudgetAt = 0
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d issue(s) did not settle: %s",
			len(failures), total, strings.Join(failures, "; "))
	}
	return nil
}

// resetSweepBudget is intentionally outside Forge's credentialed method set:
// it is local scheduling state, not another forge capability. The production
// client implements it; simple fakes need no budget to reset.
func resetSweepBudget(f Forge) {
	if resetter, ok := f.(interface{ resetSweepBudget() }); ok {
		resetter.resetSweepBudget()
	}
}

func appendPending(pending, discovered []int) []int {
	seen := make(map[int]bool, len(pending)+len(discovered))
	for _, n := range pending {
		seen[n] = true
	}
	for _, n := range discovered {
		if n > 0 && !seen[n] {
			seen[n] = true
			pending = append(pending, n)
		}
	}
	return pending
}
