package orchestrator

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The run-marker subsystem: the §9.10 file that says a workspace may hold a live
// agent, and the removals this daemon owes against it.
//
// Split out of pipeline.go, which is the state machine. This is not a phase of it —
// it has its own state (pendingClear, held off the record because every caller is
// about to be forgotten), its own worker driver, and a lifetime measured against a
// file rather than against a run. It sits here so that "two workers unlinking one
// path is the race this whole file is about" is true of the file it is written in.
//
// The one invariant the whole file serves: a marker is removed only when the run
// that held the workspace is confirmed gone, and never across a newer run's write.
// Free a live workspace and a second agent lands in the worktree.

// markerStore is one run-marker store: the provider that reaches its files, and
// the identity of the tree they live in.
//
// Two fields, because neither answers on its own.
//
// The *provider* is what performs the removal, and it has to be the one that wrote
// the file. An identity reload installs a new workspace.root, so clearing the same
// key through whatever is in force later would remove nothing and leave the
// original marker standing — a workspace parked at every future start, for a run
// this daemon watched finish.
//
// The *root* is what says whether two providers are the same store, and a provider
// instance cannot say it. A hook edit or a credential rotation rebuilds the
// provider while keeping workspace.root — not an identity change, so §5.4 gives it
// to the reload however busy the daemon is — and the two instances then write the
// very same file. Comparing instances there leaves a clear owed to the old one live
// across a new run's marker write, and its retry deletes the marker of a running
// agent.
//
// Root and not the whole Bundle.Identity, because the principal and the repository
// do not appear in a marker's path: two bundles differing only in those write the
// same file, so treating them as different stores is the same bug again.
//
// The root is compared as a **string**, and that is only sufficient because a pending
// clear now blocks an identity change (identityWorkOutstanding). `config.Load`
// cleans and absolutizes the root but does not resolve symlinks, so `/var/x` and
// `/private/var/x` are one directory under two names and would compare unequal — a
// root-alias reload would then leave a clear neither abandoned nor awaited, and its
// retry would delete the new run's marker. Canonicalizing here is not the fix:
// EvalSymlinks is filesystem I/O, this runs on the authority goroutine, and the
// directory need not exist yet. Refusing to move the root while any clear is owed
// removes the case instead of trying to compare through it.
type markerStore struct {
	ws   Workspaces
	root string
}

// markerStoreFor names the store a bundle's markers live in.
func markerStoreFor(b *Bundle) markerStore {
	return markerStore{ws: b.Workspaces, root: b.Identity().Root}
}

// sameStore reports whether two stores address the same files. A method rather
// than `==` because markerStore holds an interface, and comparing that is exactly
// the provider-instance test this type exists to replace.
func (m markerStore) sameStore(other markerStore) bool { return m.root == other.root }

// pendingClear is a run-marker removal this daemon owes, bound to the store whose
// file it is about.
//
// It has a lifetime rather than only a presence, because the removal runs in a
// worker: the real one removes a file and fsyncs its directory (workspace
// ClearRun), and the authority goroutine is the one goroutine in BEN that must
// never block on that.
type pendingClear struct {
	issue core.Issue
	store markerStore
	// inFlight says a worker is executing this removal. One at a time per pending
	// clear, so no two workers race for the same file.
	inFlight bool
	// abandoned says a new run has committed to writing its own marker at this key
	// while the removal was already executing. The loop can drop a clear that has
	// not started; it cannot recall one that has, so this says "let it finish and
	// then forget it" rather than retrying it against a file that is no longer the
	// one it was owed for.
	abandoned bool
	// done is closed when an in-flight removal has returned. It is what orders a
	// launch's own marker write after a removal the launch could only abandon too
	// late — see beginStart, which waits on it in the worker rather than on the loop.
	done chan struct{}
}

// matches reports whether a pending clear is for this issue in this store. Both
// halves: see Orchestrator.markerClears.
func (p *pendingClear) matches(identifier string, store markerStore) bool {
	return p.issue.Identifier == identifier && p.store.sameStore(store)
}

// freeWorkspaceMarker removes the run marker, at the one moment §9.10 permits:
// the run that held the workspace is confirmed gone.
//
// Called from every place groupGone becomes true, plus the launch that never
// happened, which is what keeps the marker's lifetime identical to the in-process
// fact it stands in for — the same linearization point that frees a workspace at
// run time (SPEC §9.8). Any other removal rule ("probably finished", "the read
// failed", "it has been a while") is a guess, and the guess that frees a live
// workspace puts a second agent in a worktree.
//
// A failure is retried on later ticks, and the retry is held **off the record**.
// Every one of these callers is on a path that ends in the record being forgotten
// — a terminal exit releases the claim and drops it — so a flag on the record
// would be discarded within the same turn on exactly the paths that reach here.
// The consequence of losing it is not abstract: the marker survives into the next
// start, which reads it as unknown_launch and parks an issue whose run this daemon
// watched finish.
func (o *Orchestrator) freeWorkspaceMarker(ctx context.Context, r *Record) {
	// The store *in force now*, captured with the pending clear rather than looked
	// up again later. A marker is a file under one workspace root, and AdoptIdentity
	// permits an identity change — a new workspace.root — as soon as no record is
	// outstanding, which every caller of this is about to make true by being
	// forgotten. Re-resolving at retry time would create the same key under the *new*
	// root and leave the original marker standing forever, which is the one state
	// §9.10 can never resolve on its own.
	o.clearMarkerWith(ctx, markerStoreFor(o.bundle()), r.Issue)
}

// clearMarkerWith **orders** one workspace's marker removal in a named store.
//
// Ordered rather than performed, and that is the whole of what this function is:
// the real removal deletes a file and fsyncs its directory (workspace ClearRun),
// and the authority goroutine is the one goroutine in BEN that must never block on
// I/O. Every caller reaches here from a terminal event — a confirmed stop, a launch
// that never happened, a drain — so performed here it put two fsyncs between a live
// agent's event and the loop that routes it, between the budget enforcer and a cost
// event, and inside every shutdown; and the retry pass put one there per pending
// clear per tick, for as long as the state directory stayed unwritable.
func (o *Orchestrator) clearMarkerWith(ctx context.Context, store markerStore, issue core.Issue) {
	o.rememberPendingClear(&pendingClear{issue: issue, store: store})
	o.driveMarkerClears(ctx)
}

// rememberPendingClear records a removal this daemon owes.
//
// Deduplicated per store, so two callers reaching the same conclusion about one
// workspace put one worker on the file rather than two.
//
// An **abandoned** entry is deliberately never reused, and that is the whole of what
// keeps a removal's outcome attached to the obligation it was for. An abandoned entry
// stands for a removal already executing against a file a launch has since replaced,
// and its result is still coming. Reuse it — un-abandon it and return — and that
// result lands on *this* obligation instead: the launch's own run can write its
// marker, end, and owe a fresh clear all before the loop gets round to the old
// result, and the old result then retires the new obligation. What survives is the
// marker the new run left, which §9.10 reads as unknown_launch and parks for a human.
// A pre-evidence launch failure parks it permanently, because nothing else ever looks
// at that key again.
//
// So a successor gets its own object. Two entries for one file never race: see
// clearExecutingFor.
func (o *Orchestrator) rememberPendingClear(p *pendingClear) {
	for _, existing := range o.markerClears {
		if existing.matches(p.issue.Identifier, p.store) && !existing.abandoned {
			return
		}
	}
	o.markerClears = append(o.markerClears, p)
}

// forgetPendingClear drops one clear by identity. By pointer rather than by
// issue-and-store, because that is what the result signal carries and the two must
// not be able to disagree.
func (o *Orchestrator) forgetPendingClear(p *pendingClear) {
	kept := o.markerClears[:0]
	for _, existing := range o.markerClears {
		if existing != p {
			kept = append(kept, existing)
		}
	}
	o.markerClears = kept
}

// abandonPendingClears drops every clear owed for an issue in one store, and
// reports the removals a caller must wait out before writing a marker of its own.
//
// Called from beginStart, before the worker that writes this attempt's marker. The
// pending clear is about a file that write is about to replace, so retrying it would
// delete the marker of the run this attempt is starting — and a crash after that
// reads the workspace as free and puts a second agent in it, which is the exact
// failure this precondition exists to prevent.
//
// Scoped to one store, because only that store's file is being replaced. A clear
// owed against a previous root refers to a different file under a different path,
// and dropping it would leave a marker nothing ever removes.
//
// The returned channels are the removals that were *already executing*. The loop can
// drop a clear that has not started; it cannot recall one that has, and a removal
// completing after a launch's marker write is the same deletion arriving a goroutine
// later. So those are not dropped but *ordered*: beginStart's worker waits for each
// before calling BeginRunMarkerFor, which puts the removal strictly before the write.
func (o *Orchestrator) abandonPendingClears(identifier string, store markerStore) []chan struct{} {
	var waits []chan struct{}
	kept := o.markerClears[:0]
	for _, p := range o.markerClears {
		if !p.matches(identifier, store) {
			kept = append(kept, p)
			continue
		}
		o.log.Warn("abandoning a pending run-marker clear: a new run is about to write its own marker for "+
			"this issue, and removing that would free a workspace with a live agent in it (SPEC §9.10)",
			"issue", identifier, "in_flight", p.inFlight)
		if p.inFlight {
			// Too late to drop. Kept in the set so its result is still accounted for, and
			// marked so onMarkerCleared retires it rather than retrying it against a file
			// that is no longer the one it was owed for.
			p.abandoned = true
			kept = append(kept, p)
			waits = append(waits, p.done)
		}
	}
	o.markerClears = kept
	return waits
}

// driveMarkerClears starts every owed removal that nothing is already executing.
//
// Independent of the record set, because the record is usually gone by now: these are
// workspaces already proven free whose *state file* has not caught up. Each goes back
// to the store that wrote it — see freeWorkspaceMarker.
func (o *Orchestrator) driveMarkerClears(ctx context.Context) {
	for _, p := range o.markerClears {
		if p.inFlight || o.clearExecutingFor(p) {
			continue
		}
		p.inFlight = true
		p.done = make(chan struct{})
		pending := p
		go func() {
			err := pending.store.ws.ClearRunMarkerFor(pending.issue)
			// Closed before the signal, not after. A launch waiting on this is waiting for
			// the *removal* to be over — the ordering it needs is against the file, and
			// nothing about correctness rests on when the loop applies the result (see
			// rememberPendingClear, which is what makes that true).
			close(pending.done)
			o.send(ctx, signal{kind: sigMarkerCleared, clear: pending, err: err})
		}()
	}
}

// clearExecutingFor reports whether a *different* entry is already removing the same
// file.
//
// It exists because a successor can be queued behind an abandoned removal
// (rememberPendingClear), and two workers unlinking one path is the race this whole
// file is about. The successor loses nothing by waiting: onMarkerCleared drives the
// queue again the moment its predecessor lands, so it starts on that turn rather than
// on the next tick.
func (o *Orchestrator) clearExecutingFor(p *pendingClear) bool {
	for _, other := range o.markerClears {
		if other != p && other.inFlight && other.matches(p.issue.Identifier, p.store) {
			return true
		}
	}
	return false
}

// onMarkerCleared applies one removal's outcome on the authority goroutine.
func (o *Orchestrator) onMarkerCleared(ctx context.Context, s signal) {
	p := s.clear
	p.inFlight = false
	p.done = nil
	switch {
	case p.abandoned:
		// A new run's marker write overtook this removal while it was executing.
		// Whatever it did to the old file, it is not owed again: retrying it now would
		// aim at the marker of the run that is presently going. Only *this* entry
		// retires — a successor for the same key is a different obligation.
		o.forgetPendingClear(p)
	case s.err != nil:
		// Stays owed, and is deliberately **not** re-driven from here. The next tick
		// retries it (driveMarkerClears, from onReconciled), which is what the sentence
		// below promises and what bounds the cost: restarting the removal the instant it
		// failed spends a worker and a log line per failure, continuously, for as long
		// as the state directory stays unwritable.
		//
		// It also breaks shutdown outright. drained() refuses while a removal is
		// executing, and this runs inside the same handler whose deferred driveShutdown
		// makes that check — so re-arming here means the check never sees a turn with
		// nothing in flight, and a graceful shutdown never completes.
		//
		// Nothing is waiting behind it: a successor is only ever queued behind an
		// *abandoned* clear (rememberPendingClear), and that one always retires.
		o.log.Error("could not clear the run marker for a workspace whose run is confirmed gone; "+
			"retrying next tick, and until it lands the next start will park this issue "+
			"rather than reuse the workspace (SPEC §9.10)",
			"issue", p.issue.Identifier, "error", s.err)
		return
	default:
		o.forgetPendingClear(p)
	}
	// Retired, so a successor that was waiting on this file may now run.
	o.driveMarkerClears(ctx)
}
