package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The configuration boundary (SPEC §5.4): what a valid reload reaches, how
// quickly, and what a read that began before it may still do.
//
// One rule covers most of the file — a read may be applied only if the
// configuration has not moved since it began — and
// TestConfigurationBoundaryAcrossReadsAndEvents is that rule as a table over
// every read that revalidates and then acts, crossed with every configuration
// event that can land while one is out. The single-path tests around it are the
// cases the table generalizes, kept because each names a distinct way the rule
// gets broken: a stale read that rolls the definition *back*, a superseded timer
// that must re-arm rather than launch or strand, a re-arm that must use the new
// ceiling and not the replaced one, and the ticker, which honours a cadence it is
// already asleep on unless a reload wakes it.
//
// Two things here are not about superseding at all. §5.4's defensive
// revalidation is the path a missed watch event arrives by, so it has to adopt
// and wake as a reload does; and Prepare is the long gap — a clone, a fetch, an
// after_create hook — where an edit plainly meant for the next launch lands.

// SPEC §9.4 step 2 is *defensive revalidation*, not a flag read: a watch that
// missed an event would otherwise dispatch stale configuration forever.
func TestPreflightRevalidatesAndAdoptsTheNewDefinition(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var next *config.WorkflowDefinition

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return h.publishDef(next, nil)
		},
	})

	waitFor(t, "preflight to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls > 0
	})

	// A reload the watch missed, discovered by revalidation.
	reloaded := definition(t, "3", "  max_retry_backoff_ms: 99000\n")
	mu.Lock()
	next = reloaded
	mu.Unlock()

	h.Tick()
	waitFor(t, "the revalidated definition to be adopted", func() bool {
		return h.o.MaxRetryBackoffMS() == 99000
	})
}

// SPEC §5.4 blocks *new dispatches* while a reload is invalid. A backoff or
// continuation re-dispatch is a new launch under exactly the configuration
// that failed to validate.
func TestBlockedReloadAlsoHoldsTimerRedispatch(t *testing.T) {
	var mu sync.Mutex
	var block error
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
		blocked: func() error {
			mu.Lock()
			defer mu.Unlock()
			return block
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}

	// The config goes invalid before the backoff fires.
	mu.Lock()
	block = errors.New("workflow reload failed")
	mu.Unlock()
	// PollNow, not Tick: advancing a whole poll interval would sweep past the
	// backoff timer before the block was even observed.
	h.PollNow()

	h.Clock.Advance(11 * time.Second)
	time.Sleep(40 * time.Millisecond)
	if n := h.Runner.StartCount(); n != 1 {
		t.Fatalf("started %d runs; a backoff re-dispatch must not launch under a config that failed to validate", n)
	}

	// Fixing it lets the retry through.
	mu.Lock()
	block = nil
	mu.Unlock()
	h.PollNow()
	h.Clock.Advance(20 * time.Second)
	h.WaitState("1", StateDone)
}

// SPEC §9.4 asks for defensive revalidation before each dispatch cycle, and a
// backoff re-dispatch is one. Trusting the last poll's verdict leaves a
// window a whole poll interval wide: here the config goes invalid *after* the
// last poll and before the timer fires, so only a self-revalidating timer
// path can see it.
func TestTimerRedispatchRevalidatesForItself(t *testing.T) {
	var mu sync.Mutex
	var block error
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			mu.Lock()
			defer mu.Unlock()
			return h.publishDef(nil, block)
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}

	// No poll between here and the timer firing — the last one saw a healthy
	// config.
	mu.Lock()
	block = errors.New("workflow reload failed")
	mu.Unlock()

	h.Clock.Advance(11 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := h.Runner.StartCount(); n != 1 {
		t.Fatalf("started %d runs; the timer path launched under a config it never revalidated", n)
	}
}

// SPEC §5.4: a valid reload "applies to future dispatch, retry scheduling,
// reconciliation, hooks, and launches". Reconciliation happens on a tick and
// nowhere else, so an interval captured once at startup makes the operator's
// edit load, validate, log as adopted — and change nothing.
func TestAReloadedPollingIntervalTakesEffect(t *testing.T) {
	h := start(t, harnessOpts{})
	const startup = config.DefaultPollingIntervalMS * time.Millisecond
	const reloaded = 300 * time.Second

	// Slower, so the old interval firing a tick is observable as a failure
	// rather than as timing noise.
	carrier := h.Tracker.FetchReads()
	h.Source.reload(definition(t, "3", "polling:\n  interval_ms: 300000\n"), nil)

	// The reload wakes the ticker, so the tick that carries it in arrives with
	// no clock advance at all — and the wait the ticker abandoned to take it is
	// still registered here, which is why both waits below are asked for by
	// duration rather than by count. Advancing before the re-arm would spend
	// the new interval's clock against a waiter that did not exist yet, and the
	// test would then sit out the difference.
	waitFor(t, "the tick that carries the reload", func() bool { return h.Tracker.FetchReads() > carrier })
	waitFor(t, "the ticker to re-arm at the reloaded interval", func() bool { return hasWait(h.Clock, reloaded) })
	before := h.Tracker.FetchReads()

	// The old interval must no longer be enough.
	h.Clock.Advance(startup)
	time.Sleep(50 * time.Millisecond)
	if got := h.Tracker.FetchReads(); got != before {
		t.Errorf("the daemon ticked %d more times at the old interval; the reload never reached the ticker", got-before)
	}

	// The new one is.
	h.Clock.Advance(reloaded - startup)
	waitFor(t, "a tick at the reloaded interval", func() bool { return h.Tracker.FetchReads() > before })
}

// The other direction, and the one that hurts. Publishing the new cadence only
// changes what the *next* wait is armed with; a ticker already asleep on a five
// minute interval honours it once more, so 5m → 1s takes five minutes to take
// effect — the case an operator hits when they shorten the interval precisely
// because they want the daemon to react sooner.
func TestAReloadToAFasterIntervalDoesNotWaitOutTheOldOne(t *testing.T) {
	h := start(t, harnessOpts{extraConfig: "polling:\n  interval_ms: 300000\n"})

	// The startup tick has been and gone; the ticker is asleep for five minutes.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker never armed")
	}
	before := h.Tracker.FetchReads()

	h.Source.reload(definition(t, "3", "polling:\n  interval_ms: 1000\n"), nil)

	// No clock movement at all: the sleep is abandoned, not waited out.
	waitFor(t, "a tick without advancing past the old interval", func() bool {
		return h.Tracker.FetchReads() > before
	})

	// And the cadence that follows is the reloaded one.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker did not re-arm after the reload")
	}
	after := h.Tracker.FetchReads()
	h.Clock.Advance(time.Second)
	waitFor(t, "a tick at the reloaded interval", func() bool { return h.Tracker.FetchReads() > after })
}

// Reload is not the only way a definition reaches the loop. §5.4's defensive
// revalidation is there to catch the reload a missed watch event dropped —
// editor atomic saves are exactly what makes that likely — and it arrives
// through adopt, which published the cadence without waking anything. A
// 5m → 1s change discovered that way still waited out the five minutes.
func TestAPreflightReloadWakesTheTickerToo(t *testing.T) {
	slow := "polling:\n  interval_ms: 300000\n"
	fast := definition(t, "3", "polling:\n  interval_ms: 1000\n")

	var serveFast atomic.Bool
	h := start(t, harnessOpts{
		extraConfig: slow,
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if serveFast.Load() {
				return h.publishDef(fast, nil)
			}
			return h.publishDef(nil, nil)
		},
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker never armed")
	}

	before := h.Tracker.FetchReads()
	serveFast.Store(true)

	// One tick, no clock movement. Its revalidation finds the newer definition,
	// and the wait armed for five minutes must not survive it.
	h.PollNow()
	waitFor(t, "the tick the preflight reload woke", func() bool {
		return h.Tracker.FetchReads() >= before+2
	})

	// The other half, and the reason the wake is conditional: preflight returns
	// a definition on every tick whether or not anything moved. Waking on each
	// would tick, revalidate, adopt, wake and tick again — a spin bounded by
	// the tracker's latency rather than by the poll interval.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker did not re-arm at the reloaded cadence")
	}
	settled := h.Tracker.FetchReads()
	time.Sleep(150 * time.Millisecond)
	if got := h.Tracker.FetchReads(); got != settled {
		t.Errorf("the daemon ticked %d more times without the clock moving; an unchanged definition woke the ticker", got-settled)
	}
}

// §5.4 reasons about reloads arriving. This is the one direction it never
// contemplates: a reload being *undone*.
//
// The dispatch reads revalidate the config and then fetch, and the fetch can
// outlast a human's edit. Delivered unconditionally, the definition the
// revalidation captured before the edit is re-adopted on arrival — rolling the
// daemon back to a configuration that has already been replaced, with no event
// to say so. Its candidates go the same way: selected under the old label
// partition and active states, they are the old definition's answer to "what is
// BEN's?" (§8.3) being spent on the new definition's slots.
func TestAStalePreflightResultDoesNotRollBackANewerReload(t *testing.T) {
	const (
		oldBackoff = 111000
		newBackoff = 222000
	)
	before := definition(t, "3", "  max_retry_backoff_ms: 111000\n")
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	// hold releases the superseded read; parked keeps its replacement out of
	// the way, since a discard now asks for one immediately and that
	// replacement is entitled to dispatch.
	hold, parked := make(chan struct{}), make(chan struct{})
	var onceHold, onceParked sync.Once
	unblock := func() { onceHold.Do(func() { close(hold) }) }
	release := func() { onceParked.Do(func() { close(parked) }) }
	t.Cleanup(func() { unblock(); release() })
	var reads atomic.Int32

	var edited atomic.Bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		// The file as revalidation finds it: the old definition until the human
		// saves, the new one after.
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if edited.Load() {
				return h.publishDef(after, nil)
			}
			return h.publishDef(before, nil)
		},
	})
	waitFor(t, "the startup revalidation", func() bool { return h.o.MaxRetryBackoffMS() == oldBackoff })
	h.WaitState("1", StateRunning)

	// A tick whose revalidation captures the old definition and then wedges in
	// the fetch.
	h.Tracker.SetFetchGate(func() {
		if reads.Add(1) == 1 {
			<-hold
			return
		}
		<-parked
	})
	h.PollNow()

	// The human saves, and the watcher's reload is adopted while that fetch is
	// still out.
	edited.Store(true)
	h.Source.reload(after, nil)
	h.PollNow()
	waitFor(t, "the reload to be adopted", func() bool { return h.o.MaxRetryBackoffMS() == newBackoff })

	// Something for the superseded read to dispatch, or the candidate half of
	// this test proves nothing: issue 1 is already tracked, so a stale list
	// carrying only that one would be declined for a reason that has nothing to
	// do with the reload.
	h.Tracker.Set(fake.Issue("2", epoch.Add(time.Hour)))

	// The overtaken read lands.
	starts := h.Runner.StartCount()
	unblock()
	time.Sleep(150 * time.Millisecond)

	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d, want %d; a read that began before the reload undid it", got, newBackoff)
	}
	if got := h.Runner.StartCount(); got != starts {
		t.Errorf("dispatched %d runs from candidates the reload superseded", got-starts)
	}

	// Not vacuous: the replacement cycle reads config and candidates together,
	// and dispatches the very issue the superseded read was holding.
	release()
	waitFor(t, "the replacement cycle to dispatch", func() bool { return h.Runner.StartCount() > starts })
	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d after a fresh cycle, want %d", got, newBackoff)
	}
}

// The timer track re-fetches and revalidates on its own, and then *launches*.
// §5.4 says a reload applies to launches, so a re-fetch that began before the
// human saved must not be the thing that starts the next attempt: adopting the
// definition it captured rolls the daemon back, and preparing under it runs an
// agent on a configuration already replaced.
//
// Rearmed rather than dropped — the wait is not the record's fault, and a
// dropped timer strands it in backoff with nothing left to fire.
func TestASupersededTimerRefetchRearmsInsteadOfLaunching(t *testing.T) {
	const oldBackoff, newBackoff = 111000, 222000
	before := definition(t, "3", "  max_retry_backoff_ms: 111000\n")
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	hold := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(unblock)

	var edited, gated atomic.Bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureCrashed) },
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if edited.Load() {
				return h.publishDef(after, nil)
			}
			return h.publishDef(before, nil)
		},
	})
	waitFor(t, "the startup revalidation", func() bool { return h.o.MaxRetryBackoffMS() == oldBackoff })
	h.WaitState("1", StateBackoff)

	// A record in backoff is neither refreshed nor swept by reconciliation (§9.8
	// covers running records and parked ones), so the only Get from here is the
	// timer's own.
	h.Tracker.SetGetGate(func() {
		if gated.CompareAndSwap(false, true) {
			<-hold
		}
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(2 * time.Minute)
	waitFor(t, "the re-fetch to start", func() bool { return gated.Load() })

	// The human saves while that re-fetch is wedged. Reload notes it inline, so
	// the read is superseded the moment this returns.
	edited.Store(true)
	h.Source.reload(after, nil)

	unblock()
	time.Sleep(150 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs; the attempt launched from a re-fetch the save superseded", got)
	}

	// Not stranded, and not rolled back: the rearmed timer fires, and the
	// retry it launches runs under the definition now in force.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the superseded timer did not rearm")
	}
	h.Clock.Advance(10 * time.Minute)
	waitFor(t, "the rearmed retry", func() bool { return h.Runner.StartCount() == 2 })
	waitFor(t, "the retry to run under the reloaded definition", func() bool {
		return h.o.MaxRetryBackoffMS() == newBackoff
	})
}

// Staging is when work started under the old definition stops being valid: the
// human has saved. Waiting for the loop to adopt leaves a window as long as a
// poll interval — and when the cadence did not change there is no wake to
// shorten it — in which a wedged candidate read still counts as current and
// dispatches under a configuration already replaced.
func TestAReloadInvalidatesReadsBeforeTheLoopAdoptsIt(t *testing.T) {
	const newBackoff = 222000
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	// One gate per candidate read: the first is the one the save supersedes,
	// the second is the replacement cycle the reload wakes. Holding them apart
	// is what separates "the superseded read did not dispatch" — the property
	// — from "nothing dispatched", which is no longer true and should not be:
	// a reload is supposed to bring the next cycle forward.
	superseded, replacement := make(chan struct{}), make(chan struct{})
	var onceA, onceB sync.Once
	openSuperseded := func() { onceA.Do(func() { close(superseded) }) }
	openReplacement := func() { onceB.Do(func() { close(replacement) }) }
	t.Cleanup(func() { openSuperseded(); openReplacement() })
	var reads atomic.Int32

	h := start(t, harnessOpts{})
	h.Tracker.SetFetchGate(func() {
		if reads.Add(1) == 1 {
			<-superseded
			return
		}
		<-replacement
	})
	h.PollNow()
	waitFor(t, "the first candidate read to wedge", func() bool { return reads.Load() == 1 })

	// Something for the wedged read to dispatch, and a save that supersedes it.
	// The cadence is unchanged, so nothing about the *interval* wakes the
	// ticker: only the staging itself can invalidate the read.
	h.Tracker.Set(fake.Issue("1", epoch))
	h.Source.reload(after, nil)

	openSuperseded()
	waitFor(t, "the replacement cycle the reload woke", func() bool { return reads.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 0 {
		t.Errorf("dispatched %d runs from a read the human's save superseded", got)
	}

	// Not vacuous: the replacement cycle dispatches the same issue, under the
	// definition the loop has since adopted.
	openReplacement()
	waitFor(t, "the replacement cycle to dispatch", func() bool { return h.Runner.StartCount() == 1 })
	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d, want the reloaded %d", got, newBackoff)
	}
}

// The revision advance rule now belongs to the one writer, and is asserted there
// (config.TestAnUnchangedFileDoesNotAdvanceTheRevision,
// config.TestARepeatedFailureRefreshesTheWordingWithoutReversioning). What used
// to be tested here — a compare-and-write the loop performed against its own copy
// of the configuration — has no counterpart: there is one cell, the loop does not
// write it, and a read that arrives stale is discarded rather than recorded. The
// tests that pin *that* are TestAStalePreflightResultDoesNotRollBackANewerReload
// and TestAReloadInvalidatesReadsBeforeTheLoopAdoptsIt, which drive it through
// the loop rather than through a helper.

// The boundary as one table: every read that carries a configuration, against
// every kind of configuration event that can land while it is out.
//
// The three reads are the ones that revalidate and then *act* — the candidate
// poll (§9.4 steps 2–3) and the two timer tracks (§9.6), which re-fetch and
// then launch. The three events are what §5.4 distinguishes: a valid reload, a
// validation that has started failing, and a revalidation that found nothing
// new. The rule is one line — a read may be applied only if the configuration
// has not moved since it began — and the table is what says it holds on every
// path rather than on the one the last review happened to name.
func TestConfigurationBoundaryAcrossReadsAndEvents(t *testing.T) {
	invalid := errors.New("workflow.md: unknown key `retries`")

	// Each read is set up so that exactly one run start can follow it: the
	// poll dispatches a queued issue, and each timer track launches its next
	// attempt. So "did the superseded read act?" is one number either way.
	type track struct {
		name  string
		setup func(t *testing.T, h *harness) // leaves a read gated on the tracker's Get/Fetch
	}

	for _, tr := range []track{
		{
			name: "candidate poll",
			setup: func(t *testing.T, h *harness) {
				h.Tracker.Set(fake.Issue("2", epoch.Add(time.Hour)))
				h.PollNow()
			},
		},
		{
			name: "backoff re-fetch",
			setup: func(t *testing.T, h *harness) {
				h.WaitState("1", StateBackoff)
				if !h.Clock.BlockUntilWaiters(1) {
					t.Fatal("the backoff timer was never armed")
				}
				h.Clock.Advance(2 * time.Minute)
			},
		},
		{
			name: "continuation re-fetch",
			setup: func(t *testing.T, h *harness) {
				h.WaitState("1", StateVerifying)
				if !h.Clock.BlockUntilWaiters(1) {
					t.Fatal("the continuation timer was never armed")
				}
				h.Clock.Advance(2 * time.Second)
			},
		},
	} {
		for _, ev := range []struct {
			name    string
			apply   func(t *testing.T, h *harness, next *config.WorkflowDefinition)
			applied bool // may the superseded read still act?
		}{
			{
				name:  "a valid reload",
				apply: func(_ *testing.T, h *harness, next *config.WorkflowDefinition) { h.Source.reload(next, nil) },
			},
			{
				name: "validation starts failing",
				// Shaped exactly as the watcher delivers it: a failed reload
				// keeps the last-known-good definition and reports the block, so
				// what arrives is the *standing* definition paired with an error
				// — the one transition that announces itself with nothing new to
				// adopt.
				apply: func(_ *testing.T, h *harness, _ *config.WorkflowDefinition) { h.Source.reload(h.def, invalid) },
			},
			{
				name:    "revalidation found nothing new",
				apply:   func(*testing.T, *harness, *config.WorkflowDefinition) {},
				applied: true,
			},
		} {
			t.Run(tr.name+"/"+ev.name, func(t *testing.T) {
				// hold releases the read under test; parked keeps every later
				// read out of the way. A transition wakes the ticker on
				// purpose — the replacement cycle is the point — so without
				// parking, "did the superseded read act?" would be answered by
				// its replacement.
				hold, parked := make(chan struct{}), make(chan struct{})
				var onceHold, onceParked sync.Once
				unblock := func() { onceHold.Do(func() { close(hold) }) }
				t.Cleanup(func() { unblock(); onceParked.Do(func() { close(parked) }) })
				var gated atomic.Bool

				verdict := VerdictIncomplete
				h := start(t, harnessOpts{
					issues: []core.Issue{fake.Issue("1", epoch)},
					script: func(_ core.RunSpec, attempt int) []core.Event {
						if tr.name == "backoff re-fetch" && attempt == 1 {
							return fake.Fail(core.FailureCrashed)
						}
						return fake.Succeed("s")
					},
					verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
						return VerifyResult{Verdict: verdict}, nil
					}),
				})
				if tr.name == "candidate poll" {
					h.WaitState("1", StateVerifying)
				}

				// Wedge this track's read. Both timer tracks re-fetch by Get,
				// and neither state is refreshed by reconciliation, so the gate
				// catches the read under test and nothing else.
				gate := func() {
					if gated.CompareAndSwap(false, true) {
						<-hold
						return
					}
					<-parked
				}
				if tr.name == "candidate poll" {
					h.Tracker.SetFetchGate(gate)
				} else {
					h.Tracker.SetGetGate(gate)
				}
				tr.setup(t, h)
				waitFor(t, "the read under test to wedge", func() bool { return gated.Load() })
				starts := h.Runner.StartCount()

				ev.apply(t, h, definition(t, "3", "  max_retry_backoff_ms: 222000\n"))
				unblock()
				time.Sleep(150 * time.Millisecond)

				acted := h.Runner.StartCount() > starts
				if acted != ev.applied {
					t.Errorf("the read acted = %v, want %v: a read begun under one configuration may be applied only if it has not moved",
						acted, ev.applied)
				}
			})
		}
	}
}

// Discarding a superseded read is only half of it. The record is still in
// backoff with its wait consumed, so a new one is armed — and §5.4 hands retry
// scheduling to the reload, so the new wait must be the new definition's.
//
// It compounds if it is not: the re-fetch that follows *this* wait is liable to
// be superseded too, and each time it re-arms the ceiling the operator has
// already replaced. An edit cutting five minutes to a millisecond would then
// never take hold at all, because the only path that ever adopts it is the one
// being skipped.
func TestASupersededReFetchReArmsUnderTheNewCeiling(t *testing.T) {
	hold := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(unblock)
	var gated atomic.Bool

	// The default ceiling is five minutes, so attempt 1 waits ~10s.
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
	})
	h.WaitState("1", StateBackoff)

	// Wedge the backoff re-fetch. Nothing else reads by Get here: a record in
	// backoff is not one reconciliation refreshes.
	h.Tracker.SetGetGate(func() {
		if gated.CompareAndSwap(false, true) {
			<-hold
		}
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(2 * time.Minute)
	waitFor(t, "the backoff re-fetch to wedge", func() bool { return gated.Load() })

	// The operator cuts the ceiling while the read is out, then it returns.
	h.Source.reload(definition(t, "3", "  max_retry_backoff_ms: 1\n"), nil)
	unblock()

	// Asserted on the wait itself rather than by advancing the clock: any
	// advance large enough to fire the *old* ten-second wait would also fire
	// the new one, so the two are only distinguishable by asking how long was
	// asked for. A millisecond ceiling jitters to at most 1.2ms; the ticker's
	// own wait is thirty seconds, so nothing else can satisfy this.
	waitFor(t, "the re-fetch to re-arm under the new ceiling", func() bool {
		return hasWaitWithin(h.Clock, 2*time.Millisecond)
	})
	if got := h.stateOf("1"); got != StateBackoff {
		t.Errorf("state = %q, want %q: the superseded re-fetch should have waited again, not acted", got, StateBackoff)
	}
}

// A superseded observation records *nothing* — not its definition, not its
// verdict. That is the invariant asserted here, and it is asserted at
// noteConfigAt rather than through the loop because the loop is where the
// interleaving lives and an interleaving is not a thing a test can schedule.
//
// Which is why the check and the state it guards live in one cell with one
// writer: split across two, a stale read passes a check that was true a moment
// ago and then overwrites the state that superseded it — reinstating a definition
// a human has replaced and clearing a block that has just been raised. There is
// no second state to overwrite now, so the invariant holds by construction and
// what remains to test is that a stale read is *discarded*, which
// TestAStalePreflightResultDoesNotRollBackANewerReload drives through the loop.

// §5.4 names five surfaces a valid reload applies to: dispatch, retry
// scheduling, reconciliation, hooks and launches. All of them have to read the
// *same* configuration, and read it as soon as it lands.
//
// The bug this pins is a second source. The loop kept a private copy of the
// definition and refreshed it from the versioned snapshot at the top of a tick,
// so between a reload and the next tick every question below answered under a
// configuration a human had already replaced. Nothing here ticks — that is the
// point: a policy that needs a tick to notice is one that spends a whole poll
// interval wrong, and the run outcomes and timers that drive most of these
// decisions do not wait for ticks.
func TestEveryPolicySurfaceReadsTheReloadImmediately(t *testing.T) {
	o, src := idleWithSource(t, fake.NewTracker())
	issue := core.Issue{
		Identifier: "1", State: "open",
		Labels:    []string{"ben-queue"},
		Assignees: []string{fake.DefaultPrincipal},
	}

	// Every field below is moved off the standing definition's value, and the
	// two tracker fields are moved so that this very issue changes verdict:
	// open is no longer an active state, and ben-queue is no longer the
	// partition.
	next := loadDefinition(t, `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["shipped"]
  active_states: ["in_progress"]
agent:
  kind: claude-code
limits:
  max_concurrent_agents: 1
  max_turns: 1
  max_attempts: 1
  max_retry_backoff_ms: 1000
polling:
  interval_ms: 5000
deployment:
  mode: attended
---
Work issue {{ issue.identifier }}.
`)
	standing := definition(t, "3", "")

	for _, tc := range []struct {
		surface string
		before  func() bool // holds under the harness definition
		after   func() bool // must hold once the reload lands
	}{
		{
			surface: "retry scheduling (max_attempts)",
			before:  func() bool { return o.attemptsRemain(&Record{Attempt: 2}) },
			after:   func() bool { return !o.attemptsRemain(&Record{Attempt: 2}) },
		},
		{
			surface: "the continuation budget (max_turns)",
			before:  func() bool { return o.continuable(&Record{Turns: 2}) },
			after:   func() bool { return !o.continuable(&Record{Turns: 2}) },
		},
		{
			surface: "retry scheduling (max_retry_backoff_ms)",
			before:  func() bool { return o.limits().MaxRetryBackoffMS == config.DefaultMaxRetryBackoffMS },
			after:   func() bool { return o.limits().MaxRetryBackoffMS == 1000 },
		},
		{
			surface: "capacity (max_concurrent_agents)",
			before:  func() bool { return o.freeSlots(o.definition()) == 3 },
			after:   func() bool { return o.freeSlots(o.definition()) == 1 },
		},
		{
			surface: "the ceiling `ben status` reports",
			before:  func() bool { return o.MaxRetryBackoffMS() == config.DefaultMaxRetryBackoffMS },
			after:   func() bool { return o.MaxRetryBackoffMS() == 1000 },
		},
		{
			surface: "the tick cadence",
			before: func() bool {
				d, _ := o.pollWait()
				return d == time.Duration(config.DefaultPollingIntervalMS)*time.Millisecond
			},
			after: func() bool {
				d, _ := o.pollWait()
				return d == 5*time.Second
			},
		},
		{
			surface: "reconciliation (active_states)",
			before:  func() bool { return o.active(o.definition(), issue) },
			after:   func() bool { return !o.active(o.definition(), issue) },
		},
		{
			surface: "routing (required_labels)",
			before:  func() bool { return o.routable(o.configNow(), issue) },
			after:   func() bool { return !o.routable(o.configNow(), issue) },
		},
		{
			surface: "held-claim policy (required_labels)",
			before:  func() bool { return o.hasRequiredLabels(o.definition(), issue) },
			after:   func() bool { return !o.hasRequiredLabels(o.definition(), issue) },
		},
	} {
		t.Run(tc.surface, func(t *testing.T) {
			// Back to the harness values first, so each surface starts from a
			// state where its `after` is genuinely false — a subtest that
			// inherited the reload would pass without proving anything.
			src.reload(standing, nil)
			if !tc.before() {
				t.Fatalf("%s does not hold under the standing definition; the reload below would prove nothing", tc.surface)
			}

			src.reload(next, nil)

			if !tc.after() {
				t.Errorf("%s still answers under the replaced definition: §5.4 gives it to the reload, and nothing here has ticked", tc.surface)
			}
		})
	}
}

// SPEC §5.4 gives *launches* to a valid reload, and Prepare is the long gap
// where one lands: a clone, a fetch and an after_create hook can run for
// minutes. A launch that used the definition dispatch had accepted would spend
// that whole window unable to see an edit plainly meant for it.
//
// The other half is in the same test, because they are one rule: once the run
// has started it keeps what it launched under, whatever arrives next.
func TestAReloadDuringPrepareReachesTheLaunch(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var preparing atomic.Bool

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		prepareGate: func() {
			if preparing.CompareAndSwap(false, true) {
				<-release
			}
		},
	})
	waitFor(t, "the workspace prepare to be in flight", func() bool { return preparing.Load() })

	// The edit lands while the worktree is still being built.
	reloaded := definitionTurns(t, "3", "99", "  max_cost_usd: 7.5\n")
	h.Source.reload(reloaded, nil)
	unblock()
	h.WaitState("1", StateRunning)

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("no run was started")
	}
	if spec.Limits.MaxTurns != 99 {
		t.Errorf("the launch carried max_turns = %d, want the reloaded 99: a reload during Prepare never reached the run it was meant for",
			spec.Limits.MaxTurns)
	}
	if spec.Limits.MaxCostUSD != 7.5 {
		t.Errorf("the launch carried max_cost_usd = %v, want the reloaded 7.5", spec.Limits.MaxCostUSD)
	}

	// The other half of the rule, at the unit the loop cannot expose: a started
	// run keeps what it launched under. The budget is the one thing a live
	// attempt still resolves from its own snapshot (onRunEvent → maxCost), so it
	// is what the assertion reads; r.Definition is loop-owned, and asking a
	// running harness for it from here would be the race this suite keeps
	// finding.
	idle, idleSrc := idleWithSource(t, fake.NewTracker())
	launched := &Record{Definition: reloaded}
	idleSrc.reload(definitionTurns(t, "3", "1", "  max_cost_usd: 0.01\n"), nil)
	if now := idle.limits().MaxCostUSD; now == nil || *now != 0.01 {
		t.Fatal("the second reload did not land; the assertion below would prove nothing")
	}
	if got := maxCost(launched); got != 7.5 {
		t.Errorf("the started attempt's budget moved to %v, want the 7.5 it launched under: §5.4 never disturbs a run already going", got)
	}
}
