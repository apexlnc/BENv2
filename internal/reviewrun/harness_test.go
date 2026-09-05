package reviewrun

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// fakeExec is the scripted executor the session's recovery rules are proven
// against.
//
// It is deliberately not a mock of a happy path with error knobs bolted on. The
// cases #204 asks for are all *ambiguity* — a start whose response was lost, a
// stream that replays, a sequence that never arrives — and each of those is a
// state the executor is genuinely in rather than an error it returns. So this
// keeps a real per-address run and lets a fixture decide which responses reach
// the caller.
type fakeExec struct {
	mu sync.Mutex

	// output is what the run emits, delivered in chunks of size chunk.
	output []byte
	chunk  int
	// profile is what every state reports, so a fixture can move it under a
	// pinned run.
	profile string
	sandbox string

	// loseStarts is how many Start responses are dropped *after* the run has
	// been created — the lost-response state, which is the one a replay must
	// resolve rather than duplicate.
	loseStarts int
	// failStarts is how many Start calls fail before creating anything.
	failStarts int
	// failAttaches is how many Attach calls fail (an Airlock API restart).
	failAttaches int
	// sealAfter is how many Events calls precede sealing. Zero seals at once.
	sealAfter int
	// gapAt drops one sequence from every Events answer, and conflictAt rewrites
	// one already-admitted payload.
	gapAt      int64
	conflictAt int64
	truncateAt int64
	// domainActive lets the stream seal while descendants remain live.
	domainActive bool
	// forget models a backend that lost the run (a cross-tenant collision, or an
	// address nobody holds).
	forgetAttach bool
	// refuse makes Start answer with a definite refusal and create nothing, and
	// keep answering so — from its own record, as the Airlock backend does —
	// until admit is set, which models the executor now being able to deliver
	// the very same request (#284).
	refuse *RefusedError
	admit  bool

	runs       map[string]int // address -> how many times a run was created there
	starts     int
	refusals   int
	attaches   int
	statuses   int
	eventCalls int
}

var errFakeUnavailable = errors.New("fake: the control plane did not answer")

func newFakeExec(output string) *fakeExec {
	return &fakeExec{
		output: []byte(output), chunk: 16, profile: "profile-1", sandbox: "sandbox-1",
		runs: map[string]int{},
	}
}

func (f *fakeExec) Start(ctx context.Context, ref Ref, req Request) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.failStarts > 0 {
		f.failStarts--
		return State{}, errFakeUnavailable
	}
	if f.refuse != nil && !f.admit {
		f.refusals++
		return State{}, f.refuse
	}
	if f.runs[ref.Run] == 0 {
		f.runs[ref.Run] = 1
	}
	if f.loseStarts > 0 {
		f.loseStarts--
		// The run exists; the caller never learns its id. Exactly the state a
		// replay of the same idempotency address has to resolve.
		return State{}, errFakeUnavailable
	}
	return f.state(ref), nil
}

func (f *fakeExec) ReplayStart(ctx context.Context, ref Ref, req Request) (State, error) {
	return f.Start(ctx, ref, req)
}

func (f *fakeExec) Attach(ctx context.Context, ref Ref, backendRunID string) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attaches++
	if f.failAttaches > 0 {
		f.failAttaches--
		return State{}, errFakeUnavailable
	}
	if f.forgetAttach || f.runs[ref.Run] == 0 || backendRunID != "backend-"+ref.Run {
		return State{}, ErrNoRun
	}
	return f.state(ref), nil
}

func (f *fakeExec) Status(ctx context.Context, ref Ref) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	if f.runs[ref.Run] == 0 {
		return State{}, nil
	}
	return f.state(ref), nil
}

func (f *fakeExec) Events(ctx context.Context, ref Ref, after int64) ([]Chunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventCalls++
	if f.runs[ref.Run] == 0 {
		return nil, nil
	}
	var out []Chunk
	for seq, off := int64(1), 0; off < len(f.output); seq, off = seq+1, off+f.chunk {
		end := min(off+f.chunk, len(f.output))
		payload := append([]byte(nil), f.output[off:end]...)
		switch {
		case seq == f.truncateAt:
			out = append(out, Chunk{Seq: seq, Stream: ChunkControl, Payload: []byte("output truncated"), Truncated: true})
			continue
		case seq == f.gapAt:
			continue
		case seq == f.conflictAt && seq <= after:
			payload = []byte("a different stream")
		case seq <= after && seq != f.conflictAt:
			continue
		}
		out = append(out, Chunk{Seq: seq, Payload: payload})
	}
	return out, nil
}

// state reports the run as sealed once the fixture's Events budget is spent.
func (f *fakeExec) state(ref Ref) State {
	sealed := f.eventCalls >= f.sealAfter
	return State{
		BackendRunID: "backend-" + ref.Run,
		Reachable:    true,
		Sealed:       sealed,
		Reaped:       sealed,
		Quiet:        sealed && !f.domainActive,
		Profile:      f.profile,
		Sandbox:      f.sandbox,
	}
}

func (f *fakeExec) created(run string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[run]
}

// boundedSleep advances the poll loop without elapsed time, n times, then
// reports the deadline.
func boundedSleep(n int) Sleeper {
	var calls int
	return func(ctx context.Context, d time.Duration) error {
		if calls++; calls > n {
			return context.DeadlineExceeded
		}
		return nil
	}
}

// envelope wraps a verdict object in the machine-verdict delimiters.
func envelope(body string) string {
	return "reading the diff now\n" + VerdictOpen + "\n" + body + "\n" + VerdictClose + "\n"
}

func testSubject() Subject {
	return Subject{
		Repository: "acme/ben", Issue: "11", Cycle: 700, Occurrence: 800, Claim: 900,
		PR: 42, TargetBranch: "main", Base: base1, Head: head1, Diff: "--- a/x\n+++ b/x\n",
	}
}

// newSession wires a session over a scripted executor and a real on-disk store,
// so the durability rules are exercised against files rather than a map.
func newSession(t *testing.T, exec Executor, opts ...func(*Options)) (*Session, Store) {
	t.Helper()
	store := NewDirStore(t.TempDir())
	o := Options{
		Executor: exec,
		Store:    store,
		Compose: func(sub Subject) (Request, error) {
			return Request{Argv: []string{"codex", "exec"}, Stdin: []byte(sub.Diff)}, nil
		},
		Poll:     time.Millisecond,
		Deadline: 2 * time.Second,
		// No elapsed time, and a hard bound: a fixture whose run never seals must
		// reach the deadline branch rather than spin. Two polls is one more than
		// any scripted case needs.
		Sleep:  boundedSleep(2),
		Logger: testLogger(t),
	}
	for _, f := range opts {
		f(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}

// remotely turns a session's options into a backend-executed one, pinned to the
// fake's sandbox and profile.
func remotely(f *fakeExec) func(*Options) {
	return func(o *Options) {
		o.Sandbox = func(context.Context, Subject) (Placement, error) {
			return Placement{Branch: "ben/11", BaseSHA: base1, TargetBranch: "main", Sandbox: f.sandbox, Profile: f.profile}, nil
		}
	}
}

// Digest is the fake's own dispatch address. It follows the local rule — the
// request's canonical digest — because nothing in this fake enforces limits or
// an identity of its own.
func (f *fakeExec) Digest(ref Ref, req Request) (string, error) { return req.Digest() }
