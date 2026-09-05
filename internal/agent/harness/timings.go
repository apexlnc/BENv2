package harness

import "time"

// Timings are every lifecycle window this package enforces (SPEC §7.4, §7.5),
// in one place because they compose into a budget a caller has to be able to
// reason about — and, before this seam existed, could not.
//
// The composition is what matters more than any single value. From the moment a
// run's outcome is decided, the orchestrator holds its slot and claim until the
// execution domain is confirmed quiet. It asks Probe while the record is still
// being written and may ask Stop after Done; the domain provider owns Stop's own
// teardown windows. This stream layer can additionally spend:
//
//   - up to PostExitDrain for Wait itself, which bounds the stdin copy;
//   - then up to two PostExitDrain windows in boundStream — the first spent
//     hoping the stream ends on its own, the second after the readers are
//     stranded — before the read ends are closed and the transcript can finish;
//   - plus StopGrace for direct execution to exit naturally after a terminal
//     event before reap asks the domain provider to intervene.
//
// With the defaults that is tens of seconds of teardown for a run that has
// already been decided. Nothing here changes those numbers — they are the ones
// SPEC §7.5 argues for against a real harness — but a caller that needs a
// different budget can now state one, and a test can drive the windows instead
// of sleeping through them.
//
// A zero field means "unset" and takes the default (see withDefaults), so a
// partial Timings is a legal way to pin one window and leave the rest alone.
type Timings struct {
	// StopGrace is how long direct execution gets to exit naturally after a
	// terminal event before the harness asks the domain provider to tear down.
	StopGrace time.Duration
	// PostExitDrain bounds how long the harness's output pipes may stay open
	// after the process itself has exited. Only a descendant still holding the
	// write end reaches it; when it elapses this package closes the read ends
	// itself, so the stream always ends and Done always closes.
	PostExitDrain time.Duration
	// ProbeWait bounds a readiness probe's output pipes after its process is
	// gone, so a leaked descendant cannot outlast the readiness context.
	ProbeWait time.Duration
}

// DefaultTimings are the production windows. The SPEC fixes the stall and
// attempt windows as config (§5.2.7) but leaves these to the adapter.
func DefaultTimings() Timings {
	return Timings{
		StopGrace:     10 * time.Second,
		PostExitDrain: 5 * time.Second,
		ProbeWait:     500 * time.Millisecond,
	}
}

// withDefaults fills the windows a caller left unset. Non-positive is unset:
// a zero duration would disable a bound rather than shorten it, and a bound
// that can be switched off by omission is not a bound.
func (t Timings) withDefaults() Timings {
	d := DefaultTimings()
	if t.StopGrace <= 0 {
		t.StopGrace = d.StopGrace
	}
	if t.PostExitDrain <= 0 {
		t.PostExitDrain = d.PostExitDrain
	}
	if t.ProbeWait <= 0 {
		t.ProbeWait = d.ProbeWait
	}
	return t
}
