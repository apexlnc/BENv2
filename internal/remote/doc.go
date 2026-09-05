// Package remote is BEN's v2 execution-substrate boundary: the smallest set of
// seams a remote workspace and a durable foreign process need, with no HTTP, no
// provider schema, and no change to the locked v1 local behaviour (#192, #46).
//
// # What moves and what does not
//
// BEN keeps everything it already owns — claims, retries, budgets, prompts, the
// Claude/Codex argv and raw-stream translation, the §9.3 state projection, and
// the §9.7 verification. A backend supplies two opaque things and nothing else:
// a workspace/sandbox reference (WorkspaceBackend) and a foreign process whose
// bytes, events and status are opaque until an adapter translates them
// (ProcessBackend). SPEC §3 invariant 6 is unchanged by construction: an
// Envelope carries arbitrary raw chunks. Attempt frames stdout into complete
// lines and durably checkpoints the unfinished tail, but only the provider
// adapter's Translator parses those lines into core.Event values.
//
// The composition is deliberately behind the seams that exist. Attempt is a
// core.RunHandle and Runner is a core.AgentRunner, so the orchestrator's loop,
// its states, and its retry policy are reached unchanged (SPEC §7.1, §9). There
// is no second control loop. A required DurableConsumer is the acknowledgement
// boundary the receive-only core event channel cannot supply.
//
// # The four rules the shape encodes
//
// A consumer disconnect is not a remote cancel. Events returning because a
// context ended says the *reader* stopped, and nothing at all about the run: the
// only calls that act on a backend run are Stop and Delete.
//
// A read that failed is not a fact about the run either. The backend's event log
// is cursor-addressed and retained, so a transport error means BEN could not
// reach an otherwise untouched process — and the correct move is to ask again
// from the admitted cursor under ReconnectPolicy. Past the budget the attempt is
// *held*, its stream open and its cursor unmoved, because there is no number of
// failed reads from which a verdict about the process follows. Only a backend
// that *answered* ends a stream: a sealed log with no terminal event, a
// discontinuity BEN cannot measure or resume from, or a durable envelope its
// backend adapter cannot decode (#275).
//
// Interrupt delivery is not termination. A TERM request may move a run to
// PhaseSignaled, which is not quiet and never authorizes touching the workspace
// — the same distinction core.Termination already draws locally between a ladder
// that ran and a group that is gone (SPEC §7.5, §9.8, #79).
//
// Stream sealed, direct process reaped, and descendant domain quiet are
// independent. Events follows the first, Done the second, and workspace reuse
// only an explicit DomainStateQuiet observation. Every diagnostic phase, an
// unreachable backend, and the zero Status report
// core.TerminationUnconfirmed, so the fail-closed park behaviour §9.8 already
// specifies is reached by the same route a local unconfirmed stop reaches it.
// MayReuse is the one predicate that gates acquire/attach/delete on it.
//
// # Durability
//
// Journal owns the two orderings a restart depends on, and they point in
// opposite directions on purpose. A sandbox-scoped ProcessRef and canonical
// request digest are persisted *before* start, and an unknown result is resolved
// by a request-bound backend resource or by replaying that exact Start request.
// Each event is acknowledged by a
// DurableConsumer before the attach journal advances cursor, decoder tail and
// terminal bit together. Recover reprojects accepted events after a daemon crash
// and rebuilds the complete replay-digest history, so a changed committed
// sequence fails closed. HookJournal applies the same reserve, exact StartScript
// replay, and result ordering to lifecycle hooks.
//
// # Construction boundary
//
// This package is unreachable from the v1 workflow and daemon assembly. #192
// exposes only direct construction seams for tests; #194 owns the real config,
// credentials, client, and assembly. The v1 loader and effective output have no
// substrate field. docs/REMOTE.md has the long-form boundary.
package remote
