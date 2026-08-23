package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The prior-attempt account (SPEC §5.6, §9.6, #61).
//
// Retrying a nondeterministic process with no memory of its failure is how three
// attempts fail identically and bill three budgets. What a retry has today is
// `attempt` and `run.previous_outcome` — a number and one word from the §7.3
// taxonomy — and this is the rest of the handoff: what the last attempt committed,
// which files it touched, and the tail of what it said before it stopped.
//
// **One preformatted string, fenced whole.** Not an object with a field per fact.
// §5.6's taint rule would carry the fence either way, so the object was never
// unsafe — but its safety would rest on one field still being present, and the
// moment someone drops that field the object silently becomes trusted. A single
// fenced string has no such degree of freedom.
//
// **Everything in it is agent-authored, whatever proves it exists.** Git is
// authoritative about a commit existing and a path being in a tree; it says
// nothing about the words the agent chose for either, and `git commit -m
// "<injected>"` launders exactly what the fence is for through a path that looks
// like a git fact (core.AttemptFacts). The output tail is worse still: it is
// written by an agent that had already read the fenced issue body, so anything an
// attacker can put in that body the agent can be induced to restate. Bind it
// anywhere but `untrusted()` and this variable becomes a one-hop laundering route
// out of the fence its content arrived in.
//
// **No model call.** A generated summary would buy quality for a new cost centre,
// a new failure mode mid-retry, and a nondeterministic prompt — which makes every
// prompt test a snapshot of a model's mood. Everything here is derived, so the
// same attempt composes the same bytes.

const (
	// summaryBudget bounds the composed account, the truncation notices included.
	//
	// A constant rather than a `limits.` key: every key is a support surface, and
	// this one has no natural per-workflow value. Ask for one when an operator
	// does.
	//
	// It is **not** a guarantee against `limits.max_prompt_bytes` (SPEC §5.2.7).
	// A 16 KiB addition can still push a large prompt over the ceiling, and
	// §5.6's refusal then applies exactly as it does today — which is why the
	// bound is well under it rather than merely below it.
	summaryBudget = 16 << 10
	// summaryOutputReserve is the share of the budget held for what the agent
	// said. Without it a branch carrying two hundred changed files consumes the
	// whole allowance and crowds out the part that carries the information: the
	// account of what the attempt was *trying*.
	summaryOutputReserve = 8 << 10
	// summaryIndent prefixes every listed entry. The output tail is deliberately
	// not indented — see said.
	summaryIndent = "  "
)

// attemptAccount is everything retained about one finished attempt, before it is
// rendered into the string the next attempt's prompt carries.
//
// Composed when the outcome is routed, which is the moment every input is final:
// the terminal reason is known, the event stream is closed, and the commits are on
// the branch. Never at render time — a render-time derivation would read a
// workspace the next attempt may already be preparing.
type attemptAccount struct {
	// attempt is the number of the attempt being described, not the one being
	// dispatched.
	attempt int
	// outcome is `run.previous_outcome`: "succeeded", or the §7.3 reason. BEN's
	// own enum, and the only part of this that is not agent-authored.
	outcome string
	facts   core.AttemptFacts
	// factsRead reports that the branch was read at all. False means BEN did not
	// find out, which the account says rather than reporting no commits: turning
	// an unread branch into "committed nothing" is a fabrication, and one the
	// agent would act on.
	factsRead bool
	// output is the retained tail of the agent's own prose, already redacted at
	// the harness boundary (SPEC §10.3) — it reaches the loop through
	// core.Event.Text, which handle.emit covers for exactly this reason.
	output string
	// outputTotal is how many bytes of prose the attempt produced altogether, so
	// the account can state what it is not showing.
	outputTotal int
}

// render composes the account. Exported behaviour is the constants' version;
// budget and reserve are parameters so a test can drive every truncation branch
// without a 16 KiB fixture.
func (a attemptAccount) render() string { return a.renderWithin(summaryBudget, summaryOutputReserve) }

func (a attemptAccount) renderWithin(budget, reserve int) string {
	head := fmt.Sprintf("attempt %d ended: %s\n", a.attempt, a.outcome)
	// The observation half is capped short of the whole budget, so a long file
	// list cannot crowd the output tail out entirely.
	observed := a.observed(budget - reserve - len(head))
	said := a.said(budget - len(head) - len(observed))
	out := head + observed + said

	// A final clamp, and it should be unreachable: the section headings are
	// fixed-width and the budgets above are thousands of bytes
	// (TestSummarySectionsFitTheBudget pins that). It is here because the one
	// thing this must not do is hand back more than it promised, and a section
	// added later is where that would break.
	if len(out) > budget {
		return truncateRunes(out, budget)
	}
	return out
}

// observed renders what BEN saw of the branch.
func (a attemptAccount) observed(budget int) string {
	if !a.factsRead {
		// One line, and it does not pretend to be two empty lists. An agent told
		// "no commits" reasonably concludes the last attempt wrote nothing.
		return "commits and files changed: not read\n"
	}
	commits := listSection("commits", a.facts.Commits, a.facts.CommitsTruncated, budget)
	return commits + listSection("files changed", a.facts.Files, a.facts.FilesTruncated, budget-len(commits))
}

// listSection renders one bounded list, stating every entry it does not show.
// A summary that quietly ends mid-list is worse than a shorter one that says it
// was cut: the agent cannot tell the difference, and acts as though it has the
// whole account.
//
// more carries the provider's own truncation (core.AttemptFacts), so an entry
// dropped upstream and one dropped for want of budget are counted together —
// there is only one number the reader cares about.
func listSection(label string, items []string, more bool, budget int) string {
	for kept := len(items); kept > 0; kept-- {
		s := renderList(label, items[:kept], len(items)-kept, more)
		if len(s) <= budget {
			return s
		}
	}
	return renderList(label, nil, len(items), more)
}

func renderList(label string, items []string, dropped int, more bool) string {
	if len(items) == 0 && dropped == 0 && !more {
		return label + ": none\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", label)
	for _, item := range items {
		b.WriteString(summaryIndent + item + "\n")
	}
	switch {
	case dropped > 0 && more:
		fmt.Fprintf(&b, "%s… %d more not shown, and there were others beyond the read limit\n", summaryIndent, dropped)
	case dropped > 0:
		fmt.Fprintf(&b, "%s… %d more not shown\n", summaryIndent, dropped)
	case more:
		fmt.Fprintf(&b, "%s… more, beyond the read limit\n", summaryIndent)
	}
	return b.String()
}

// said renders the agent's own words, last and verbatim.
//
// Last, because whatever the agent wrote is the part of this string it controls,
// and putting BEN's own sections after it would invite a forged heading. Verbatim,
// because indenting or re-wrapping an account in order to make it look tidier is a
// reshape of untrusted content, and the byte accounting on the heading would then
// describe something other than what is shown.
func (a attemptAccount) said(budget int) string {
	const label = "what the agent said, most recent last"
	if a.output == "" {
		if a.outputTotal > 0 {
			// It produced prose and none of it survived — a partial or missing
			// retention, which is exactly the state a crashed attempt is read in.
			return fmt.Sprintf("%s: none retained, of %d bytes\n", label, a.outputTotal)
		}
		return label + ": nothing\n"
	}

	heading := func(shown int) string {
		if shown < a.outputTotal {
			return fmt.Sprintf("%s (truncated, %d of %d bytes):\n", label, shown, a.outputTotal)
		}
		return fmt.Sprintf("%s (%d bytes):\n", label, shown)
	}
	// Room for the heading before anything is spent on the text, measured on its
	// *longest* form: the truncated spelling with the largest numbers it could
	// carry. Measuring the form that turns out not to be used would let the fit
	// depend on whether truncation happened, which is what is being decided.
	room := budget - len(fmt.Sprintf("%s (truncated, %d of %d bytes):\n", label, a.outputTotal, a.outputTotal))
	if room <= 0 {
		return fmt.Sprintf("%s: not shown, of %d bytes\n", label, a.outputTotal)
	}

	shown := a.output
	if len(shown) > room {
		// The *tail*: what the agent said last is what says where it got to. On a
		// rune boundary, and before the value is fenced — a split rune inside a
		// fence is a fence that may not survive rendering.
		shown = tailRunes(shown, room)
	}
	if !strings.HasSuffix(shown, "\n") {
		shown += "\n"
	}
	return heading(len(strings.TrimSuffix(shown, "\n"))) + shown
}

// Both cuts below return a **copy**, and that is the difference between bounding
// what is addressed and bounding what is held.
//
// A Go string slice shares its input's backing array, so a 16 KiB tail taken from
// one 10 MiB progress event pins all ten megabytes — for the life of the record,
// which for a parked one is until a human resolves it. The bound would then be a
// bound on nothing: the whole point of truncating as events arrive is that the
// record's footprint does not follow how much the agent chose to say
// (#61 re-review, finding 1).
//
// strings.Clone rather than a manual copy because it does exactly this and says so.
// The `len(s) <= max` paths need no copy: they return the input whole, so there is
// no larger array left addressed by a smaller string.

// truncateRunes keeps the first max bytes of s, cut on a rune boundary.
//
// utf8.RuneStart on the first dropped byte is the whole test: a continuation byte
// there means the rune before the cut is split, so the cut moves back. Bounded by
// utf8.UTFMax-1 steps on valid input, and it terminates on invalid input too —
// which agent output can be.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Clone(s[:cut])
}

// tailRunes keeps the last max bytes of s, cut on a rune boundary. It is what
// bounds the retained output as events arrive, and again when the account is
// composed; both want the end of the stream, not the beginning.
func tailRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return strings.Clone(s[cut:])
}

// --- the loop half ---------------------------------------------------------

// summaryReadTimeout bounds the branch read. It exists because applyOutcome
// waits on this hop: a `git log` that never returns would otherwise leave the
// record in `verifying` or `running` with its outcome held, forever, and no
// timer to notice. The window is generous — this is two local git commands —
// and a read that misses it is reported as an unread branch, not as a failure of
// the attempt.
const summaryReadTimeout = 30 * time.Second

// beginSummary reads what the finished attempt left on its branch.
//
// Nothing here can fail the attempt. Every unhappy path — no workspace, an
// unreadable branch, a timeout — resolves to an account that says the branch was
// not read, and the outcome routes exactly as it would have. The prior-attempt
// summary is a lever on cost, not a gate on correctness, and a defect in it must
// not be able to park a run.
func (o *Orchestrator) beginSummary(ctx context.Context, r *Record) {
	if r.summarizing {
		return
	}
	if !r.hasWorkspace() {
		// Prepare never produced one — a launch that failed before it, or a
		// record whose attempt never reached a workspace. There is no branch, and
		// saying "no commits" about one would be an invention.
		r.summarized = true
		o.resumeAfterAccount(ctx, r)
		return
	}
	r.summarizing = true
	gen, token := r.generation, r.token
	id, ws := r.Issue.Identifier, r.Workspace
	workspaces := o.bundle().Workspaces
	r.pending++
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, summaryReadTimeout)
		defer cancel()
		facts, err := workspaces.AttemptFacts(readCtx, ws)
		o.send(ctx, signal{kind: sigSummarized, issue: id, generation: gen, token: token,
			attemptFacts: facts, err: err})
	}()
}

func (o *Orchestrator) onSummarized(ctx context.Context, r *Record, s signal) {
	r.pending--
	r.summarizing = false
	r.summarized = true
	if s.err != nil {
		// Logged and carried on. The account will say the branch was not read,
		// which is the honest answer and the one an agent can act on.
		o.log.Warn("reading the finished attempt's branch for the next prompt",
			"issue", r.Issue.Identifier, "error", s.err)
	} else {
		r.attemptFacts, r.attemptFactsRead = s.attemptFacts, true
	}
	if o.finishIfRequested(ctx, r) {
		return
	}
	if r.suspended {
		// The drain decides what *follows*, which is nothing: the outcome stays
		// unrouted, the state label stands, and §9.10 resumes from the claim exactly
		// as it does for a verdict that arrives mid-drain (onVerified).
		//
		// An attempt's terminal **bookkeeping** is not "what follows", though, and a
		// deferred park is the one route where it is still owed this late.
		// finishSuspended draws the same line for a run the drain interrupted, for
		// the reason recordAttempt states: a suspended record's attempt is over
		// rather than paused, and §9.10 resumes the issue as a new one. Without this,
		// a signal landing in the window between parkOnBudget deferring and its read
		// reporting costs the §6.5 hook a call it is owed, and lets driveShutdown
		// relabel an attempt that ended on `budget_exceeded` as `killed` — the
		// drain's reason for an attempt it interrupted, which this one was not
		// (#61 re-review round 2).
		o.endAttemptSuspended(ctx, r)
		// The account itself is deliberately not composed: it exists for the next
		// attempt's prompt, and this process will not render one. It is in-memory
		// only, so it goes with the record.
		return
	}
	o.resumeAfterAccount(ctx, r)
}

// endAttemptSuspended completes the bookkeeping of an attempt that had already
// ended when the drain arrived, and nothing else.
//
// **Both routes into the account read need it, not just §9.9's park.** The park is
// where a reviewer found it, but the ordinary failure track is the same shape and
// the commoner one: applyOutcome holds its outcome across this read too, so a signal
// in that window used to leave a crashed attempt with no hook call and an outcome
// recorded as `killed`.
//
// What it does *not* do is route. No transition, no projection, no milestone: those
// are what a drain refuses, and §9.10 resumes the issue from the claim and the label
// (onVerified draws the same line for a verdict that arrives mid-drain).
//
// The reason is taken from what had already been decided, never from the drain.
// `killed` is driveShutdown's reason for an attempt *it* interrupted, and an attempt
// that ended on its own before the signal is not that — recordAttempt's own contract
// is that the caller states what ended the attempt. Ordering is guaranteed rather
// than raced: this runs inside a signal handler, driveShutdown is deferred after
// every one, and recordAttempt is once-only, so the reason stated here is the one
// that lands.
func (o *Orchestrator) endAttemptSuspended(ctx context.Context, r *Record) {
	reason := core.FailureReason("")
	switch {
	case r.parkOnBudgetPending:
		// §9.9 decided this before the signal arrived; the park itself is refused.
		r.parkOnBudgetPending = false
		reason = core.FailureBudgetExceeded
	case r.outcome != nil && r.outcome.event.Type == core.EventFailed:
		reason = r.outcome.event.Reason
	}
	// A held *success* leaves the reason empty and the verdict unknown, which is
	// what it is: the attempt ended cleanly and nothing was concluded about what it
	// produced. Same pair onVerified records when the check could not be completed.
	o.attemptEnded(ctx, r)
	o.recordAttempt(r, reason, VerdictUnknown)
}

// resumeAfterAccount carries on with whatever was waiting for the account, and it
// is one function because there are two things that can be: the held outcome an
// ordinary attempt end routes, and §9.9's park, which never reaches the held-outcome
// machinery at all — a budget breach *stops* the run rather than waiting for one to
// end.
//
// Both callers of beginSummary come back through here, including its synchronous
// no-workspace path. An earlier draft resumed straight into applyOutcome from there,
// which stranded the deferred park on the one record that had no workspace to read.
func (o *Orchestrator) resumeAfterAccount(ctx context.Context, r *Record) {
	if r.parkOnBudgetPending {
		r.parkOnBudgetPending = false
		o.parkOnBudget(ctx, r)
		return
	}
	o.applyOutcome(ctx, r)
}

// parkOnBudget is §9.9's park, after the account of the attempt has been read.
//
// It reads the branch like every other attempt end, and the reason it needs saying
// is that an earlier draft did not: the workspace is *kept* here (§9.9), the park is
// resolved by a human re-queue, and that re-queue restores budgets rather than
// memory — so the next agent would have been the only one to lose the commits and
// files, on the one path where they are still sitting on disk waiting to be read
// (#61 review, finding 4).
//
// Nothing waits on it. Unlike applyOutcome's gate, the routing here is a park a
// human resolves minutes or hours later, so the read costs the park nothing.
//
// It owns the §6.5 after-run hook call for this route, and the ordering is the
// point: that hook runs against the same worktree under the same issue lock
// (workspace AfterRun), and it may commit. Queued before the read, the two would
// race, and the winner would decide whether the next attempt hears about the
// agent's commits, the hook's, or — if the read lost and timed out — neither. Every
// other route already reads before attemptEnded, because applyOutcome's gate
// precedes the routing that calls it; this is the one that had to be told.
func (o *Orchestrator) parkOnBudget(ctx context.Context, r *Record) {
	if !r.summarized {
		r.parkOnBudgetPending = true
		o.beginSummary(ctx, r)
		return
	}
	o.attemptEnded(ctx, r)
	r.recordAccount(r.lastOutcome)
	// The §9.11 attempt-outcome record (#60) came with the routing when it moved
	// here: it belongs to routing this outcome, not to the call site that decided
	// the outcome had happened.
	o.recordAttempt(r, core.FailureBudgetExceeded, VerdictUnknown)
	o.enterNeedsReview(ctx, r, "budget exceeded", core.FailureBudgetExceeded)
}

// retainOutput accumulates one assistant message into the bounded tail the next
// attempt's prompt will carry (SPEC §9.6).
//
// Bounded as it arrives, because the alternative is holding a whole run's prose per
// record for the sake of a 16 KiB tail — and nothing downstream would notice, since
// the account truncates either way (TestTheRetainedTailIsBoundedAsItArrives).
//
// One event is one assistant message (claudecode/stream.go, codexexec/stream.go),
// so messages are joined by a newline rather than concatenated: run two together and
// the account reads as one sentence the agent never wrote.
//
// **outputTotal counts the separators**, and that is not bookkeeping pedantry. It is
// the length of the stream the tail is a tail *of*, and `said` decides the account
// was truncated by comparing the two. Counting only the messages lets the
// separators inflate the retained bytes past the total — so a run of many short
// messages would report "16384 bytes" of a 9000-byte total, claim no truncation, and
// have silently dropped the beginning. That is the silent cut §5.6 forbids
// (#61 review, finding 3), and TestRetainedOutputNeverUnderreportsItsTotal is what
// holds the invariant the two rely on.
func (r *Record) retainOutput(text string) {
	joined := text
	if r.outputTail != "" && !strings.HasSuffix(r.outputTail, "\n") {
		joined = "\n" + joined
	}
	r.outputTotal += len(joined)
	r.outputTail = tailRunes(r.outputTail+joined, summaryBudget)
}

// recordAccount composes the account of the attempt that just ended and stores
// it for the next one's prompt (SPEC §5.6, §9.6).
//
// Called where an outcome is *routed*, because that is the moment every input is
// final: the §7.3 reason has been decided, the event stream is closed, and the
// branch has been read. outcome is passed rather than read off the record because
// failAttempt sets r.lastOutcome and this in the same breath, and a function that
// took one from a field the caller had just written would work by luck.
func (r *Record) recordAccount(outcome string) {
	r.previousAttempt = attemptAccount{
		attempt:     r.Attempt,
		outcome:     outcome,
		facts:       r.attemptFacts,
		factsRead:   r.attemptFactsRead,
		output:      r.outputTail,
		outputTotal: r.outputTotal,
	}.render()
}

// forgetAccount drops it. The continuation track is the one route that must not
// carry one: §9.6 gives that track the resume token, so the session already holds
// its own history and this would duplicate it into the context window it exists to
// save (SPEC §9.6).
func (r *Record) forgetAccount() { r.previousAttempt = "" }
