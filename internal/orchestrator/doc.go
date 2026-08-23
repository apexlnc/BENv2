// Package orchestrator is the authority loop (SPEC §9): one goroutine owns
// every state transition, fed by tick, timer, and runner signals. Everything
// that can block — claiming, preparing a workspace, starting an agent — runs
// in a worker goroutine and reports back as a signal, so a slow tracker can
// delay work without stalling the machine that accounts for it.
//
// # Reading this package
//
// The entry point is not the largest file. Two rules recover the shape of any
// of them:
//
//   - A `begin*`/`on*` pair is one phase of the state machine. `begin` is a
//     prologue and a handoff to a worker; `on` applies that worker's result back
//     on the loop. The worker in between reads no loop state and holds no
//     `*Record` — it captures what it needs up front, which is why no closure
//     here mentions `r`. What it captures is often not small (a RunSpec, an
//     Issue, the adapter interfaces); the invariant is the Record and the maps,
//     not the size of the capture.
//   - Anything a worker learns arrives as a signal (signals.go), and
//     `deliverable` is what stops one from landing on a record that has moved on.
//
// # Which goroutine am I on?
//
// Ask it before editing anything here. Four answers are possible — the authority
// goroutine, a worker, the pre-loop caller inside `Recover`, and whatever
// goroutine calls the exported API — and **a function's name does not tell you
// which**. Two earlier drafts of this comment tried to answer it by name shape
// (named-vs-closure, then `begin*`/`drive*`/`classify*`) and both were false, so
// the taxonomy is not worth rebuilding: a helper inherits its caller's context,
// and several here have more than one caller.
//
// One anchor is reliable: an `on*` handler only ever runs on the authority
// goroutine, because every production path to one descends from `handle`, and
// `handle` has a single call site in `loop`. Most are dispatched by `handle` or
// `handleRecord` directly; a few are reached one hop further in, as
// `onClaimProjected` is from `afterEffect`. Everything else, trace it.
//
// These are the ones worth knowing by name, because the dangerous edit is
// assuming a function is off-loop and adding a blocking call to it — which on the
// authority goroutine stalls runner events, budget enforcement and shutdown
// together:
//
//	classify        the startup pass (Recover → recoverCandidate) *and* the
//	                retry worker (retryRecovery's goroutine) — and it already
//	                reads the transition log through lastFailure
//	applyRecovery   pre-loop, from recoverCandidate, *and* loop-side from
//	                onRecovered — which drags beginApproval in with it
//	driveHold       pre-loop, from applyRecovery, *and* loop-side from
//	                onEffectDone and retryPendingExits
//	runSweep        straight from Recover *and* from beginSweep's worker
//	revalidate      never on the loop: both call sites are inside a `go func()`
//	                (pipeline.go, signals.go). Listed because the name reads
//	                loop-side and is not
//
// What the loop's ownership buys is lock-free access to `records`, `held` and the
// rest — but ownership is *transferred*, not exclusive to the loop: `Recover`
// builds that record set before the loop exists, through `adoptRecovered` and
// `adoptUnclassified`, and hands it over. Single-threaded either way, which is
// what makes the absence of locks sound.
//
// The *projection* read from outside is separate and mutex-guarded: `published`,
// `heldCount`, `drainingPublished` and `identityWork`, behind `o.mu`. The last is
// for the reload watcher rather than `ben status`. Six functions write across that
// boundary — `publish`, `forget`, `dropHeld`, `publishHeld`,
// `publishIdentityWork`, `onShutdown` — so mutating a Record is free, and making
// the change visible is what takes the lock.
//
// `Shutdown` and `AdoptIdentity` are callable from any goroutine and reach the
// loop by sending a signal and awaiting an ack; `Recover` runs before the loop
// exists at all, which is what `Run`'s ErrNotRecovered refusal enforces.
//
// Where to start, by question:
//
//	"what happens to an issue"          pipeline.go, by begin*/on* pair
//	"what reaches the loop, and how"    signals.go, then orchestrator.go's loop
//	"what may follow what"              state.go
//	"what survives a restart"           recover.go + recover_classify.go
//
// # The files
//
// The machine:
//
//	pipeline.go         The state machine, as begin*/on* phase pairs — but read it
//	                    for the pairs, not as a running order. It opens in the order
//	                    an issue meets them (claim, approval, prepare, start, run),
//	                    then the failure, backoff and timer transitions every phase
//	                    shares, and only then verify, probe and stop — which an
//	                    issue reaches from a run that ended, via applyOutcome, not
//	                    through the failure block they sit behind. Two string
//	                    helpers end the file.
//	signals.go          The signal type and the only door into the loop: handle,
//	                    handleRecord, onTick, and the reconcile pass.
//	state.go            The nine §9.2 states and the legal transitions between
//	                    them. `Allowed` is the whole table.
//	record.go           Per-issue loop state, and the Snapshot `ben status` reads.
//	orchestrator.go     The Orchestrator struct, New, Run, the loop itself, and the
//	                    config/bundle accessors every phase resolves through.
//
// Phases with enough of their own rules to live apart from pipeline.go:
//
//	approval.go         §9.5 content-bound approval: the pin, and what a
//	                    `required_label` is taken to have approved.
//	summary.go          The prior-attempt account a retry is told about
//	                    (§5.6, §9.6, #61), and its byte budget.
//	marker.go           The §9.10 run marker: the file that says a workspace may
//	                    hold a live agent, and the removals owed against it.
//	held.go             A retained `done` claim, released only once the issue
//	                    closes — with one owner at a time.
//	parked.go           The parked half of the §9.8 sweep.
//	recover.go          Startup recovery's I/O half (§9.10): the driver.
//	recover_classify.go Startup recovery's pure half: what the facts mean. Kept
//	                    separate so the verdicts are model-checkable without I/O.
//	shutdown.go         Drain, not kill: `drained` is the condition, and a
//	                    suspended attempt is not a failed one.
//	retry.go            Backoff delay, and whether an attempt remains.
//
// Seams and plumbing:
//
//	deps.go             The narrow interfaces the loop needs — Tracker, Workspaces,
//	                    Runner, Verifier, Clock — declared here rather than
//	                    imported, so the loop names its own requirements.
//	bundle.go           One immutable adapter set plus the definition it came from.
//	                    A reload swaps the bundle; it never mutates one.
//	owed.go             Tracker writes as part of the machine: a write the loop owes
//	                    is retried until it lands, and some block a state exit.
//	absence.go          The one read that may conclude an issue is gone, spent when
//	                    a write cannot say so itself (#134) — and the per-tick
//	                    budget both absence confirmations rotate over.
//	durable.go          The one off-loop queue behind every append-only state-dir
//	                    log, so the authority goroutine never waits on a write.
//	translog.go         The §9.11 transition log, over that queue.
//	attempt.go          The per-attempt outcome log (#60), over that queue.
//
// # Tests
//
// Test files are named for the behavior under test, which for the cross-cutting
// ones is not the name of a source file: transitions_test.go walks all 81 state
// pairs, contract_test.go asserts the real adapters still satisfy the narrow
// interfaces in deps.go, and policy_test.go pins the §9.6 backoff sequence
// exactly. The others that answer a question the source layout does not ask:
//
//	claim_test.go       Claim's several answers (§8.4) — only the refusal
//	                    unwinds — and the projection that must land before any
//	                    attempt starts.
//	gone_test.go        An issue deleted from the tracker (§9.8): how the fact is
//	                    learned — including from a write that can never land — and
//	                    the failures that must not be mistaken for it.
//	exit_test.go        What a record on its way out still owes, and what may
//	                    neither drop it nor restart it.
//	tick_test.go        §9.4's order: reconcile before dispatch, and what a slow
//	                    read may not stall.
//	reload_test.go      The §5.4 configuration boundary — a read may be applied
//	                    only if the configuration has not moved since it began.
//	run_budget_test.go  The §9.9 cost and §9.6 turn budgets, and the park at the
//	                    end of one. budget_test.go is the §8.5 per-tick tracker
//	                    request budget, which is a different budget.
//	hook_test.go        One §6.5 after_run per attempt that ran a process.
//	prompt_test.go      The §5.6 render ceiling, as the loop applies it.
//	fake_test.go        internal/fake's fidelity to the adapter it stands in for.
//
// §9.10 is five files rather than one. Everything in the original recover_test.go
// was genuinely about startup recovery, but 72 tests is past what anybody holds
// in their head, and three of its subjects had grown their own rules (#160):
//
//	recover_test.go           The driver and its verdicts — the restart table, the
//	                          four gates, unknown launches, the retry pass, the
//	                          startup warnings — and the fixtures the rest of the
//	                          family shares, `harness.restart` among them.
//	marker_test.go            The run marker, beside the marker.go #157 split it
//	                          out into: written before a launch, removed only on
//	                          proof the run is gone, and the removals owed when
//	                          one cannot be.
//	sweep_test.go             Step 5's workspace sweep: what it may delete, what
//	                          it may not touch, and what a pass costs.
//	recover_reason_test.go    Step 6's failure reason, and the cases whose honest
//	                          answer is that it did not survive. translog_test.go
//	                          is that log's write path (§9.11) — another subject.
//	recover_classify_test.go  The pure half, as recover_classify.go is.
//
// There is deliberately no file for tests that fit nowhere. regression_test.go
// was that file — 53 tests over nine subjects, which is where a test lands when
// nobody decides where it belongs — and #159 distributed it into the topics
// above. A test whose subject genuinely has no home earns a new topic file; it
// does not earn a second pile.
//
// The §12.3 invariants live one level out, in internal/integration, whose doc.go
// maps them.
package orchestrator
