package config

import "sync"

// Snapshot is the configuration in force: the definition, the adapter set built
// from it, whether new dispatches are blocked, and the revision that names the
// three together (SPEC §5.4, §5.7).
//
// One value because they are one fact. Held apart they tear — an adapter checked
// Ready against one configuration and then operating under another makes the
// check meaningless (§5.7), and a reader that assembled the pieces itself could
// observe a definition from one state paired with adapters from the next. There
// is no assembling to do here: a reader gets all four or none.
type Snapshot[R any] struct {
	// Definition is the last definition that loaded cleanly. Never nil in a
	// published snapshot: startup with an invalid config refuses (SPEC §5.7).
	Definition *WorkflowDefinition
	// Runtime is the adapter set a caller built for Definition — opaque here on
	// purpose, since constructing adapters needs the kind registry and that is
	// not the watcher's business (SPEC §5.7).
	Runtime R
	// Blocked is the reload failure withholding new dispatches, or nil. A
	// non-nil Blocked alongside a valid Definition is the ordinary state of a
	// daemon whose config file is currently broken: last-known-good serves
	// in-flight work while new dispatch waits (SPEC §5.4).
	Blocked error
	// Revision names this configuration. It advances on a transition and never
	// otherwise, so work in flight can carry the revision it began under and be
	// discarded when it no longer matches. Zero is never published, which makes
	// a zero-value Snapshot detectably "never observed".
	Revision uint64
}

// RuntimeSource is the one cell holding the configuration in force. The Watcher
// is its only writer; everything else reads.
//
// One writer and one cell, rather than a published copy per reader. A second
// copy has to be installed, and installing two things is a window in which a
// reader can see one of them — the tear this package exists to prevent, moved
// one level out. It also makes adoption a single store, which is what lets the
// commit be infallible: there is no "between" for a failure to land in.
type RuntimeSource[R any] struct {
	mu   sync.RWMutex
	snap Snapshot[R]
	// wake is closed and replaced on every revision advance. Publishing the
	// new configuration is not enough on its own: a ticker already asleep on
	// the old poll interval would honour it once more, and 5m → 1s would take
	// five minutes to take effect. This is how that sleep is abandoned.
	wake chan struct{}
}

// NewRuntimeSource returns a source fixed at one configuration.
//
// For a caller that runs without hot reload — an acceptance harness, or a daemon
// mode that does not watch the file. There is deliberately no exported way to
// write to a source: the Watcher publishes to the one it owns, and a source built
// here is simply never republished. That is what keeps "one writer" a property of
// the type rather than a convention.
func NewRuntimeSource[R any](def *WorkflowDefinition, runtime R) *RuntimeSource[R] {
	s := &RuntimeSource[R]{}
	s.store(def, runtime, nil)
	return s
}

// Load returns the configuration in force and the channel that closes when it
// next moves.
//
// Both under one acquisition, deliberately. Read apart, a reload landing between
// them hands back an interval from one revision and a wake from the next — and
// the next one has not fired, so a ticker waiting on it sleeps out the old
// interval it was just told to abandon.
func (s *RuntimeSource[R]) Load() (Snapshot[R], <-chan struct{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.wake
}

// store installs an observation and reports what is now in force.
//
// Nothing else happens here: no callbacks, no logging, no I/O. This is the one
// operation a commit is allowed to consist of, so it has to be infallible — a
// caller-supplied listener called from inside it would put a fallible step in
// the middle of a publication, which is how a transaction reported as failed
// leaves the state half moved. Notification belongs after the commit is
// established, at the caller.
//
// The revision advances on a *transition* — a different definition, or a change
// in whether dispatch is blocked — and on nothing else. Not on a change in a
// fault's wording: the same broken file reported twice is one state observed
// twice, and re-versioning it would discard reads for a fact that has not moved.
// The fresher message is still recorded, because this is the record of the
// configuration and it should say what is currently wrong.
//
// The runtime needs no comparison of its own. A runtime is only ever replaced
// alongside the definition it was built from — an unchanged file never reaches a
// build (see Watcher.transaction) — so "a different definition" already covers
// every rebuild there can be.
func (s *RuntimeSource[R]) store(def *WorkflowDefinition, runtime R, blocked error) Snapshot[R] {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := Snapshot[R]{
		Definition: def,
		Runtime:    runtime,
		Blocked:    blocked,
		Revision:   s.snap.Revision,
	}
	if def == s.snap.Definition && (blocked == nil) == (s.snap.Blocked == nil) {
		s.snap = next
		return next
	}
	next.Revision++
	s.snap = next
	if s.wake != nil {
		close(s.wake)
	}
	s.wake = make(chan struct{})
	return next
}
