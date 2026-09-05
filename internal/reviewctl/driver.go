package reviewctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// errStop ends this run cleanly without an error. It is the answer to
// "something outside the controller declined to produce a usable input" — a
// reviewer that stated no verdict, say. The occurrence stays unrouted, the
// scheduled sweep will look again, and the human whose label is still standing
// is not told their CI is broken.
var errStop = errors.New("stop")

// maxSteps bounds one run. Every step consumes a durable artifact that makes
// the next reduction different, so a no-review terminal cycle is three writes
// (intent, mutate, route) and a reviewed one remains three (review, mutate,
// route); a budget of six is slack, not a policy.
const maxSteps = 6

type driver struct {
	cfg   review.Config
	forge Forge
	// repository is the credential-free identity the workspace cycle is keyed
	// by — mirror.Mirror.Repository(). It is carried rather than derived from
	// owner/repo because the sandbox address must be spelled the one way the
	// workspace strategy spells it; two spellings would be two sandboxes.
	repository   string
	reviewer     Reviewer
	maxDiffBytes int
	log          func(string, ...any)
	dryRun       bool
}

// Run drives one issue to a settled state: observe, reduce, execute, repeat.
//
// The loop *re-observes* rather than assuming what its own write did, which is
// what makes an interrupted run and a fresh one the same code path. Every "the
// process died between X and Y" row of #11's table arrives here as an ordinary
// observation in which X is on the forge and Y is not.
func (d *driver) Run(ctx context.Context) error {
	var last review.Step
	for i := 0; i < maxSteps; i++ {
		snap, err := d.observe(ctx)
		if err != nil {
			return err
		}
		step := review.Reduce(d.cfg, snap)
		d.log("issue #%d: %s — %s", d.cfg.Issue, step.Kind, step.Why)

		if step.Kind == review.StepNothing {
			return nil
		}
		if d.dryRun {
			d.log("issue #%d: dry run, stopping before %s", d.cfg.Issue, step.Kind)
			return nil
		}
		if i > 0 && sameStep(step, last) {
			// The mutation was accepted but the read has not caught up, which
			// GitHub's own eventual consistency permits. Repeating a write on
			// that basis is how one unassignment becomes two.
			d.log("issue #%d: %s is unchanged after being applied; leaving it to reconciliation", d.cfg.Issue, step.Kind)
			return nil
		}
		last = step

		switch err := d.apply(ctx, snap, step); {
		case errors.Is(err, errStop):
			return nil
		case err != nil:
			return err
		}
	}
	return fmt.Errorf("issue #%d: %d steps without settling; something is re-driving the same decision", d.cfg.Issue, maxSteps)
}

func sameStep(a, b review.Step) bool {
	return a.Kind == b.Kind && a.Occurrence == b.Occurrence && a.Head == b.Head &&
		a.PR.Base == b.PR.Base && a.PR.BaseSHA == b.PR.BaseSHA && a.Outcome == b.Outcome &&
		a.ReviewerProfile == b.ReviewerProfile
}

// observe builds one whole snapshot. Reads that fail are errors, never
// absences: a comment page that 500s is not an issue with fewer comments
// (SPEC §9.10's rule, and the reason the reducer can treat what it is handed
// as complete).
func (d *driver) observe(ctx context.Context) (review.Snapshot, error) {
	var snap review.Snapshot
	var err error

	if snap.Issue, err = d.forge.Issue(ctx, d.cfg.Issue); err != nil {
		return snap, err
	}
	if snap.Comments, err = d.forge.Comments(ctx, d.cfg.Issue); err != nil {
		return snap, err
	}
	if snap.Events, err = d.forge.Events(ctx, d.cfg.Issue); err != nil {
		return snap, err
	}

	trigger, ok := review.LatestPublished(d.cfg, snap.Comments)
	if !ok {
		return snap, nil
	}
	owner, repo, number, err := review.ParsePRURL(trigger.PRURL)
	if err != nil || !equalFold(owner, d.cfg.Owner) || !equalFold(repo, d.cfg.Repo) {
		// Leave PR nil and let the reducer say why in one place. Fetching a
		// pull request named by an unvalidated link would be the controller
		// following a pointer an issue comment gave it.
		return snap, nil
	}

	pr, err := d.forge.PullRequest(ctx, number)
	if isNotFound(err) {
		// A deleted or transferred pull request is a fact, and the reducer
		// treats a nil PR as a refusal. Every other error is a failed read.
		return snap, nil
	}
	if err != nil {
		return snap, err
	}
	snap.PR = pr

	// Reviews are the round counter and the deduplication key, so a partial
	// list is the one failure that could cause a *second* review of a head.
	// It is an error here and never a shorter slice.
	if snap.Reviews, err = d.forge.Reviews(ctx, number); err != nil {
		return snap, err
	}
	return snap, nil
}

func (d *driver) apply(ctx context.Context, snap review.Snapshot, step review.Step) error {
	switch step.Kind {
	case review.StepReview:
		return d.performReview(ctx, snap, step)

	case review.StepUnassign:
		return d.forge.Unassign(ctx, d.cfg.Issue, step.Principal)

	case review.StepRevoke:
		// Revocation first, informational label second: the label that stops
		// automation is the one that must land, and a failure to decorate the
		// issue afterwards must not leave it dispatchable.
		if err := d.forge.RemoveLabel(ctx, d.cfg.Issue, step.RemoveLabel); err != nil && !isNotFound(err) {
			return err
		}
		if step.AddLabel != "" {
			if err := d.forge.AddLabel(ctx, d.cfg.Issue, step.AddLabel); err != nil {
				return err
			}
		}
		return nil

	case review.StepRecordIntent:
		m := review.RouteIntentMarker{
			Occurrence: step.Occurrence,
			Claim:      step.Claim,
			Approval:   step.Approval,
			Head:       step.Head,
			Outcome:    step.Outcome,
		}
		if _, err := review.ParseRouteIntentMarker(m.String()); err != nil {
			return fmt.Errorf("refusing to post a terminal route intent that cannot be read back: %w", err)
		}
		return d.forge.PostComment(ctx, d.cfg.Issue, review.RouteIntentBody(m, step.Why))

	case review.StepRecordRoute:
		m := review.RouteMarker{Occurrence: step.Occurrence, Claim: step.Claim, Head: step.Head, Outcome: step.Outcome}
		// A marker the parser cannot read back is worse than no marker: the
		// route would look unrecorded forever and the occurrence would be
		// re-driven on every sweep. Prove the round trip before writing.
		if _, err := review.ParseRouteMarker(m.String()); err != nil {
			return fmt.Errorf("refusing to post a route marker that cannot be read back: %w", err)
		}
		return d.forge.PostComment(ctx, d.cfg.Issue, review.RouteBody(m, step.Why))

	default:
		return fmt.Errorf("unknown step %q", step.Kind)
	}
}

// performReview is the one place a model's output becomes a forge artifact,
// and the ordering inside it is the safety property: read the diff at the
// exact head, review it with no credential, re-confirm that the whole decision
// still stands, and only then publish.
func (d *driver) performReview(ctx context.Context, snap review.Snapshot, step review.Step) error {
	if step.PR.Base == "" || step.PR.BaseSHA == "" {
		return fmt.Errorf("issue #%d: pull request #%d reports no complete base, so there is no exact diff to review", d.cfg.Issue, step.PR.Number)
	}
	diff, err := d.forge.Diff(ctx, step.PR.BaseSHA, step.Head)
	if err != nil {
		return err
	}
	sub := d.subject(step, BoundDiff(diff, d.maxDiffBytes))
	report, err := d.reviewer.Review(ctx, sub)
	if errors.Is(err, reviewrun.ErrRunRefused) {
		return d.surfaceRefusal(ctx, snap, step, err)
	}
	if err != nil {
		// Missing, empty, ambiguous or unresolvable verdicts authorize no
		// routing — and neither does a run still in flight. Every one of them
		// leaves the occurrence unrouted for the next sweep, which is the same
		// answer #11 gave and is why the daemon's sweep is the availability
		// mechanism rather than an optimization.
		d.log("issue #%d: no usable verdict for head %s (%v); nothing is published", d.cfg.Issue, step.Head, err)
		return errStop
	}

	// Re-read before publishing. A head that moved while the model was
	// thinking must be reviewed again rather than judged from a diff nobody
	// will merge, and a claim that moved must not be routed at all.
	after, err := d.observe(ctx)
	if err != nil {
		return err
	}
	if confirm := review.Reduce(d.cfg, after); !sameReviewSubject(confirm, step) {
		d.log("issue #%d: the subject moved while the reviewer ran (now: %s — %s); nothing is published",
			d.cfg.Issue, confirm.Kind, confirm.Why)
		return nil
	}

	m := review.ReviewMarker{
		Occurrence: step.Occurrence, Claim: step.Claim, Approval: step.Approval,
		Head: step.Head, Base: step.PR.BaseSHA, Verdict: report.Verdict,
		ReviewerProfile: step.ReviewerProfile,
	}
	if err := d.forge.PublishReview(ctx, step.PR.Number, step.Head, review.ReviewBody(m, report.Findings)); err != nil {
		return err
	}
	// The published review is now the durable record of this verdict, so the
	// execution record has no remaining reader. Retiring it is best effort in
	// exactly one direction: it is refused while the run may still be live, and
	// a refusal costs a retained file rather than a repeated review.
	if err := d.reviewer.Retire(ctx, sub); err != nil {
		d.log("issue #%d: the review run's execution record was retained (%v)", d.cfg.Issue, err)
	}
	return nil
}

// surfaceRefusal is the one artifact a reviewer that could not start leaves
// behind: a comment on the issue, posted once per refused occurrence, head and
// reason, and never a review or a route (#284).
//
// The distinction from every other unusable verdict is that this one will not
// change by itself. A run in flight seals; a model that said nothing may say
// something next round; a substrate that refused to admit the request refuses
// the same request forever. Leaving that to the log is what let the canary's
// review loop appear to skip a pull request for five hours. So the human whose
// label is standing is told, on the issue, in a body that says plainly it is
// not a verdict — and the controller's next sweep finds the statement already
// made and makes no second one.
func (d *driver) surfaceRefusal(ctx context.Context, snap review.Snapshot, step review.Step, err error) error {
	refusal, ok := reviewrun.RefusalOf(err)
	if !ok || !review.ValidBlockReason(refusal.Reason) {
		// A refusal with no reason, or one the marker cannot carry, is still a
		// refusal: it stops here like any other unusable verdict, and the log
		// carries what the marker could not.
		d.log("issue #%d: the reviewer could not be started for head %s (%v); nothing is published", d.cfg.Issue, step.Head, err)
		return errStop
	}
	m := review.ReviewBlockedMarker{
		Occurrence: step.Occurrence, Claim: step.Claim, Approval: step.Approval,
		Head: step.Head, Reason: refusal.Reason,
	}
	if existing, ok := review.BlockedReviewFor(d.cfg, snap.Comments, m.Occurrence, m.Head); ok && existing.Reason == m.Reason {
		d.log("issue #%d: the reviewer still cannot be started for head %s (%s); the statement is already on the issue",
			d.cfg.Issue, step.Head, refusal.Reason)
		return errStop
	}
	if _, err := review.ParseReviewBlockedMarker(m.String()); err != nil {
		return fmt.Errorf("refusing to post a blocked-review statement that cannot be read back: %w", err)
	}
	d.log("issue #%d: the reviewer could not be started for head %s (%s: %s); stating so on the issue",
		d.cfg.Issue, step.Head, refusal.Reason, refusal.Detail)
	if err := d.forge.PostComment(ctx, d.cfg.Issue, review.ReviewBlockedBody(m, refusal.Detail)); err != nil {
		return err
	}
	return errStop
}

// subject is the bounded review subject, assembled from facts the reducer has
// just revalidated.
//
// Every field comes from this observation rather than from anything the
// reviewer could influence — which is what makes the derived run identity
// (reviewrun.Subject.RunID) a name for *this* comparison under *this* approval,
// and therefore what makes a stale run unreattachable rather than merely
// unlikely to be reattached.
func (d *driver) subject(step review.Step, diff string) reviewrun.Subject {
	return reviewrun.Subject{
		Repository:      d.repository,
		Issue:           strconv.Itoa(d.cfg.Issue),
		Cycle:           step.Approval,
		Occurrence:      step.Occurrence,
		Claim:           step.Claim,
		PR:              step.PR.Number,
		TargetBranch:    step.PR.Base,
		Base:            step.PR.BaseSHA,
		Head:            step.Head,
		Diff:            diff,
		ReviewerProfile: step.ReviewerProfile,
	}
}

func sameReviewSubject(confirm, step review.Step) bool {
	return confirm.Kind == review.StepReview &&
		confirm.Occurrence == step.Occurrence &&
		confirm.Claim == step.Claim &&
		confirm.Approval == step.Approval &&
		confirm.Head == step.Head &&
		confirm.PR.Base == step.PR.Base &&
		confirm.PR.BaseSHA == step.PR.BaseSHA &&
		confirm.ReviewerProfile == step.ReviewerProfile
}
