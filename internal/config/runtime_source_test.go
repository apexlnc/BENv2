package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// discardLogs keeps the intentional failure paths below from writing to the
// test's stderr; what they assert is state, not log text.
func discardLogs() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The startup build goes through the same seam as every reload's, and builds
// everything: nothing has been constructed yet, so there is nothing to carry
// forward and no previous runtime to be handed.
//
// One seam rather than two is the point. A separate startup path is a second
// place where "Ready has passed before this is published" would have to be true,
// and the second place is where it stops being true (SPEC §5.7).
func TestStartupBuildsTheRuntimeThroughTheSameSeam(t *testing.T) {
	rec := &reloadRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: rec.build,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	calls := rec.seen()
	if len(calls) != 1 {
		t.Fatalf("BuildRuntime called %d times at startup, want exactly 1", len(calls))
	}
	if want := (AdapterChange{Tracker: true, Agent: true, Workspace: true}); calls[0] != want {
		t.Errorf("startup changed = %+v, want %+v — at startup every adapter is new", calls[0], want)
	}
	if prev := rec.prevSeen()[0]; prev != nil {
		t.Errorf("startup was handed prev = %+v, want nil: there is nothing to carry forward", prev)
	}

	snap := w.Snapshot()
	if snap.Revision != 1 {
		t.Errorf("startup revision = %d, want 1", snap.Revision)
	}
	if snap.Runtime == nil || !snap.Runtime.pairedWith(snap.Definition) {
		t.Error("startup published a definition the runtime was not built from")
	}
	if snap.Blocked != nil {
		t.Errorf("startup published a block: %v", snap.Blocked)
	}
}

// A runtime that cannot be built and made ready at startup is a refusal to
// start, not a blocked state: there is no last-known-good to serve in-flight
// work, because there is no in-flight work and no previous configuration
// (SPEC §5.7).
func TestWatchRefusesAnUnbuildableStartingRuntime(t *testing.T) {
	notReady := errors.New("harness not installed")
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour,
		BuildRuntime: func(context.Context, *WorkflowDefinition, *testRuntime, AdapterChange) (*testRuntime, error) {
			return nil, notReady
		},
	})
	if err == nil {
		w.Close()
		t.Fatal("Watch started with a runtime that could not be made ready; §5.7 refuses instead")
	}
	if !errors.Is(err, notReady) {
		t.Errorf("error = %v, want the builder's refusal", err)
	}
}

// The acceptance criterion: there is no observable interleaving in which the
// published definition and the live adapters disagree.
//
// Every observation is checked, not a sampled one, so the assertion does not
// depend on where the readers happen to land — which is what makes it a claim
// about the publication rather than about this machine's scheduler. Two writers
// keep the adapter-bound configuration moving so that a two-step publication
// would have a window to be caught in; with one store there is none.
//
// The pairing tested is `pairedWith`, not pointer equality: a definition-only
// edit legitimately carries the runtime forward, and demanding identity would
// fail on the correct behaviour.
func TestNoReaderEverSeesADefinitionItsRuntimeWasNotBuiltFrom(t *testing.T) {
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Millisecond, BuildRuntime: noRuntime, Logger: discardLogs(),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	src := w.RuntimeSource()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writers: adapter-bound edits, so every one of them must rebuild.
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets",
					"acme/repo"+strconv.Itoa(i)+"-"+strconv.Itoa(n), 1))
				w.Revalidate(context.Background())
			}
		}()
	}

	// Readers: every snapshot they see must be internally consistent, and the
	// revision they see must never go backwards.
	var failures sync.Map
	var observations int64
	var obsMu sync.Mutex
	for r := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			local := 0
			for {
				select {
				case <-stop:
					obsMu.Lock()
					observations += int64(local)
					obsMu.Unlock()
					return
				default:
				}
				snap, _ := src.Load()
				local++
				if !snap.Runtime.pairedWith(snap.Definition) {
					failures.Store(r, fmt.Sprintf("revision %d published a definition whose tracker config the live runtime was not built from: %+v",
						snap.Revision, adapterChange(snap.Runtime.builtFrom, snap.Definition)))
					return
				}
				if snap.Revision < last {
					failures.Store(r, fmt.Sprintf("revision went backwards: %d after %d", snap.Revision, last))
					return
				}
				last = snap.Revision
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	failures.Range(func(reader, why any) bool {
		t.Errorf("reader %v: %v", reader, why)
		return true
	})
	obsMu.Lock()
	defer obsMu.Unlock()
	if observations < 100 {
		t.Errorf("only %d observations; the readers barely ran, so a clean result proves little", observations)
	}
	if got := w.Snapshot().Revision; got < 2 {
		t.Errorf("final revision = %d; nothing was ever adopted, so the readers had nothing to catch", got)
	}
}

// A definition-only edit publishes the new definition and carries the adapters
// forward — the same instances, not equal copies.
//
// Rebuilding here would be worse than wasteful: every unrelated knob would
// re-run New and Ready, so editing a poll interval would cost a credential check
// and could block dispatch on a tracker that happened to be briefly unreachable.
func TestADefinitionOnlyChangeCarriesTheRuntimeForward(t *testing.T) {
	rec := &reloadRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: rec.build,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	before := w.Snapshot()

	atomicSave(t, path, strings.Replace(changedPrompt, "max_turns: 9", "max_turns: 5", 1))
	after := w.Revalidate(context.Background())

	if got := rec.reloads(); len(got) != 0 {
		t.Errorf("BuildRuntime called %+v for a limits-only edit; no adapter is bound to limits", got)
	}
	if after.Runtime != before.Runtime {
		t.Errorf("runtime replaced (serial %d → %d) for an edit that moved no adapter's configuration",
			before.Runtime.serial, after.Runtime.serial)
	}
	if after.Definition == before.Definition {
		t.Error("the new definition was not adopted")
	}
	if after.Revision != before.Revision+1 {
		t.Errorf("revision = %d, want %d: the definition moved, so work read under the old one is superseded",
			after.Revision, before.Revision+1)
	}
	if !after.Runtime.pairedWith(after.Definition) {
		t.Error("the carried-forward runtime is not paired with the published definition")
	}
	if got := after.Definition.Config.Limits.MaxTurns; got != 5 {
		t.Errorf("max_turns = %d, want the edited 5", got)
	}
}

// The builder is handed the runtime in force, so a dependency whose own
// configuration did not move can be carried forward by name.
//
// As a parameter rather than something the builder closes over: a builder
// holding its own copy of the last runtime it returned would be a second owner
// of the published state, which is the ownership bug this seam exists to remove
// — one level further in, and invisible from here.
func TestTheBuilderIsHandedTheRuntimeInForce(t *testing.T) {
	rec := &reloadRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: rec.build,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	startup := w.Snapshot().Runtime

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	w.Revalidate(context.Background())

	prev := rec.prevSeen()
	if len(prev) != 2 {
		t.Fatalf("%d builder calls, want startup plus one reload", len(prev))
	}
	if prev[1] != startup {
		t.Errorf("the rebuild was handed %+v, want the runtime in force", prev[1])
	}
}

// workspace.* and hooks.* bind to the workspace strategy, so moving either has
// to rebuild it.
//
// The bug this pins is a live one: adapterChange compared only the tracker and
// agent slices while the unchanged-file check compared the whole config, so
// editing after_create published a new definition, rebuilt nothing, and logged as
// adopted — leaving the provider running the previous scripts under the previous
// root.
func TestWorkspaceAndHookEditsRebuildTheWorkspaceStrategy(t *testing.T) {
	const withHooks = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
workspace:
  root: /tmp/ben-root
hooks:
  after_create: "echo one"
  timeout_ms: 30000
deployment:
  mode: attended
---
Do {{ issue.title }}.
`
	tests := []struct {
		name string
		to   string
		want AdapterChange
	}{
		{"hook script", strings.Replace(withHooks, "echo one", "echo two", 1), AdapterChange{Workspace: true}},
		{"hook timeout", strings.Replace(withHooks, "timeout_ms: 30000", "timeout_ms: 45000", 1), AdapterChange{Workspace: true}},
		{"workspace root", strings.Replace(withHooks, "/tmp/ben-root", "/tmp/ben-elsewhere", 1), AdapterChange{Workspace: true}},
		{"prompt only", strings.Replace(withHooks, "Do {{", "Redo {{", 1), AdapterChange{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &reloadRecorder{}
			path := writeWorkflow(t, withHooks)
			w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
				Debounce: time.Hour, BuildRuntime: rec.build,
			})
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			defer w.Close()

			atomicSave(t, path, tt.to)
			if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
				t.Fatalf("Revalidate: %v", snap.Blocked)
			}

			got := rec.reloads()
			if !tt.want.Any() {
				if len(got) != 0 {
					t.Fatalf("BuildRuntime called with %+v for an edit no adapter is bound to", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("%d rebuilds, want 1", len(got))
			}
			if got[0] != tt.want {
				t.Errorf("changed = %+v, want %+v", got[0], tt.want)
			}
		})
	}
}

// The publish credential binds to the runner at New (SPEC §5.2.8, §7.1), so
// editing it has to rebuild that runner and re-check Ready.
//
// This is the third instance of one bug class, and the reason the agent leg now
// compares Config.AgentBinding() instead of listing sections: the unchanged-file
// check compares the whole config, so an edit the rebuild predicate does not
// mention publishes a new definition, rebuilds nothing, and logs as adopted. Twice
// before it left the workspace provider on stale hooks; here it would leave the
// runner holding the *previous publish identity* while `config effective` reported
// the new one — the failure #83 would hit on its one-line identity change, and the
// worst shape of it, since the daemon would keep publishing as the human reviewing
// its work with nothing in the log to say so.
func TestPublishEditsRebuildTheRunner(t *testing.T) {
	const withPublish = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
publish:
  kind: token
  env: GH_TOKEN
  value: $BEN_OLD_IDENTITY
deployment:
  mode: attended
---
Do {{ issue.title }}.
`
	tests := []struct {
		name string
		to   string
		want AdapterChange
	}{
		// The identity change itself: the same child variable, a different
		// credential behind it. Nothing else in the file moves.
		{"publish.value", strings.Replace(withPublish, "$BEN_OLD_IDENTITY", "$BEN_NEW_IDENTITY", 1), AdapterChange{Agent: true}},
		// The child variable the credential is injected as is equally bound: the
		// adapter composed the previous one into every child environment.
		{"publish.env", strings.Replace(withPublish, "env: GH_TOKEN", "env: GITHUB_TOKEN", 1), AdapterChange{Agent: true}},
		// Removing the block is a change of identity too — to none.
		{"publish removed", strings.Replace(withPublish, "publish:\n  kind: token\n  env: GH_TOKEN\n  value: $BEN_OLD_IDENTITY\n", "", 1), AdapterChange{Agent: true}},
		// The control, so this is not just "any edit rebuilds the runner".
		{"prompt only", strings.Replace(withPublish, "Do {{", "Redo {{", 1), AdapterChange{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &reloadRecorder{}
			path := writeWorkflow(t, withPublish)
			w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
				Debounce: time.Hour, BuildRuntime: rec.build,
			})
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			defer w.Close()

			atomicSave(t, path, tt.to)
			if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
				t.Fatalf("Revalidate: %v", snap.Blocked)
			}

			got := rec.reloads()
			if !tt.want.Any() {
				if len(got) != 0 {
					t.Fatalf("BuildRuntime called with %+v for an edit no adapter is bound to", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("%d rebuilds, want 1", len(got))
			}
			if got[0] != tt.want {
				t.Errorf("changed = %+v, want %+v", got[0], tt.want)
			}
		})
	}
}

// The rebuild predicate and the value the adapter is bound to are one expression,
// and this is the assertion that keeps them so (config.AgentBinding).
//
// Anchored on the projection rather than on a list of sections: it does not name
// `publish`, so it stays true for whatever core-owned field the agent binds next.
// A field added to the binding without being a reason to rebuild fails here, which
// is the only way a test can cover the omission that caused this bug three times —
// a table of "these edits rebuild" can only ever check the entries somebody
// remembered to write.
func TestAgentRebuildCoversEverythingTheBindingCarries(t *testing.T) {
	base, err := Load(writeWorkflow(t, workflowWithPublish(tokenPublishBlock)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Every distinguishable field of the binding, mutated one at a time. Built by
	// walking the projection, so the loop cannot fall behind the type.
	binding := base.Config.AgentBinding()
	for _, tc := range []struct {
		field  string
		mutate func(*Config)
	}{
		{"Provider", func(c *Config) { c.Agent.Provider = map[string]any{"model": "opus"} }},
		{"Publish.Env", func(c *Config) { c.Publish.Env = "GITHUB_TOKEN" }},
		{"Publish.Var", func(c *Config) { c.Publish.ValueVar = "OTHER_TOKEN" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			next := *base
			next.Config = base.Config
			tc.mutate(&next.Config)

			if reflect.DeepEqual(binding, next.Config.AgentBinding()) {
				t.Fatalf("mutating %s did not change the binding, so this row proves nothing", tc.field)
			}
			if !adapterChange(base, &next).Agent {
				t.Errorf("%s moved in the binding but adapterChange says the runner need not be rebuilt", tc.field)
			}
		})
	}
}

// barrierRecorder is a caller's serialization point: it decides whether the
// commit may run, and — like the real one — runs it exactly once or not at all.
type barrierRecorder struct {
	mu    sync.Mutex
	asked int
	ran   int
	// pairs records what each publication moved between, which is how a caller
	// tells an identity change from a policy edit — the watcher must not decide
	// that for it.
	pairs      [][2]*testRuntime
	refuseWith error
}

func (b *barrierRecorder) seenPairs() [][2]*testRuntime {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][2]*testRuntime(nil), b.pairs...)
}

func (b *barrierRecorder) barrier(_ context.Context, prev, next *testRuntime, commit func()) error {
	b.mu.Lock()
	b.asked++
	b.pairs = append(b.pairs, [2]*testRuntime{prev, next})
	refuse := b.refuseWith
	b.mu.Unlock()
	if refuse != nil {
		// Refused: the commit does not run, so nothing is published. This is the
		// half of the contract that makes a reported failure trustworthy.
		return refuse
	}
	commit()
	b.mu.Lock()
	b.ran++
	b.mu.Unlock()
	return nil
}

func (b *barrierRecorder) counts() (asked, ran int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asked, b.ran
}

func (b *barrierRecorder) refuse(err error) {
	b.mu.Lock()
	b.refuseWith = err
	b.mu.Unlock()
}

// Every publication goes through the caller's barrier, and the startup one does
// not: there is no caller yet, and nothing outstanding for it to protect.
func TestCommitsGoThroughTheBarrierExceptAtStartup(t *testing.T) {
	bar := &barrierRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime, Barrier: bar.barrier,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if asked, _ := bar.counts(); asked != 0 {
		t.Errorf("the barrier was asked %d times at startup, want 0", asked)
	}
	before := w.Snapshot()

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	after := w.Revalidate(context.Background())

	asked, ran := bar.counts()
	if asked != 1 || ran != 1 {
		t.Errorf("barrier asked %d times, commit ran %d, want 1 and 1", asked, ran)
	}
	if after.Revision != before.Revision+1 || after.Definition == before.Definition {
		t.Errorf("the commit ran but nothing was published: revision %d → %d", before.Revision, after.Revision)
	}
}

// A barrier that refuses for outstanding work publishes no new configuration —
// and does publish the block.
//
// The block is the part worth pinning. Leaving the snapshot untouched would be
// "nothing changed", which reads as dispatch permitted, while the file on disk is
// one this daemon has refused to adopt.
func TestARefusedAdoptionPublishesTheBlockAndNothingElse(t *testing.T) {
	bar := &barrierRecorder{}
	bar.refuse(fmt.Errorf("2 claims held: %w", ErrWorkOutstanding))

	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime, Barrier: bar.barrier,
		Logger: discardLogs(),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	before := w.Snapshot()

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	after := w.Revalidate(context.Background())

	if _, ran := bar.counts(); ran != 0 {
		t.Fatal("the commit ran despite the barrier refusing; a reported failure must mean nothing was published")
	}
	if after.Definition != before.Definition || after.Runtime != before.Runtime {
		t.Error("a refused adoption replaced the definition or runtime")
	}
	if !errors.Is(after.Blocked, ErrWorkOutstanding) {
		t.Errorf("blocked = %v, want the refusal — an unpublished block leaves dispatch permitted under a config this daemon refused", after.Blocked)
	}
	if after.Revision != before.Revision+1 {
		t.Errorf("revision = %d, want %d: raising the block is a transition, and reads begun before it must be superseded",
			after.Revision, before.Revision+1)
	}
}

// A deferred candidate is not rebuilt on every tick.
//
// Revalidate runs per dispatch cycle and the deferred candidate still differs
// from what is in force, so without the memo the daemon would re-run New and
// Ready — a credential check and a tracker round trip, against the §8.5 budget —
// once per tick for as long as the operator's edit is refused. And it is a
// deferral, not a latch: once the caller reports quiescence the same candidate is
// rebuilt and adopted, with no further edit to the file.
func TestADeferredCandidateIsNotRebuiltUntilTheCallerIsQuiescent(t *testing.T) {
	bar := &barrierRecorder{}
	bar.refuse(fmt.Errorf("one claim held: %w", ErrWorkOutstanding))
	rec := &reloadRecorder{}
	quiet := false

	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: rec.build, Barrier: bar.barrier,
		Quiescent: func() bool { return quiet },
		Logger:    discardLogs(),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	if snap := w.Revalidate(context.Background()); !errors.Is(snap.Blocked, ErrWorkOutstanding) {
		t.Fatalf("blocked = %v, want the refusal", snap.Blocked)
	}
	if got := len(rec.reloads()); got != 1 {
		t.Fatalf("%d rebuilds for the first sight of the candidate, want 1", got)
	}

	// Ticks with the same candidate refused: no rebuild, and the block stands.
	for range 5 {
		if snap := w.Revalidate(context.Background()); !errors.Is(snap.Blocked, ErrWorkOutstanding) {
			t.Fatalf("the block lapsed while the candidate was still refused: %v", snap.Blocked)
		}
	}
	if got := len(rec.reloads()); got != 1 {
		t.Errorf("%d rebuilds over 5 further ticks, want 1: each one is New plus Ready, per tick, for an answer that cannot have changed", got)
	}
	if asked, _ := bar.counts(); asked != 1 {
		t.Errorf("the barrier was asked %d times, want 1: the memo answers without a round trip", asked)
	}

	// The caller's work drains. The same candidate — no further edit — is
	// rebuilt and adopted, so the deferral was a deferral and not a latch.
	quiet = true
	bar.refuse(nil)
	after := w.Revalidate(context.Background())
	if after.Blocked != nil {
		t.Fatalf("the edit was never adopted after the caller went quiescent: %v", after.Blocked)
	}
	if got := len(rec.reloads()); got != 2 {
		t.Errorf("%d rebuilds, want 2: readiness is a point-in-time fact, so adoption must re-check it rather than reuse the discarded runtime", got)
	}
	if got := after.Definition.Config.Tracker.Provider["repo"]; got != "acme/gadgets" {
		t.Errorf("repo = %v, want the deferred edit adopted", got)
	}
	if !after.Runtime.pairedWith(after.Definition) {
		t.Error("the adopted definition and its runtime disagree")
	}
}

// A refusal for any other reason is an ordinary failed reload: it is retried
// from scratch, because unlike outstanding work the world may have changed.
func TestAnOrdinaryBarrierFailureIsNotDeferred(t *testing.T) {
	bar := &barrierRecorder{}
	bar.refuse(errors.New("the loop is gone"))
	rec := &reloadRecorder{}

	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: rec.build, Barrier: bar.barrier,
		Quiescent: func() bool { return false },
		Logger:    discardLogs(),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	for range 3 {
		if snap := w.Revalidate(context.Background()); snap.Blocked == nil {
			t.Fatal("a barrier failure was adopted anyway")
		}
	}
	if got := len(rec.reloads()); got != 3 {
		t.Errorf("%d rebuilds over 3 ticks, want 3: only ErrWorkOutstanding defers, because only it cannot change on its own", got)
	}
}

// The barrier is told what the publication moves between, so a caller can tell an
// identity change from a policy edit.
//
// Without it, every commit looks alike to the caller, and a caller that refuses
// while it has work outstanding refuses *all* of them — no limits edit, no hook
// change, no credential rotation lands while the daemon is busy, which is the
// opposite of what §5.4 gives the reload.
func TestTheBarrierIsToldWhatThePublicationMovesBetween(t *testing.T) {
	bar := &barrierRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime, Barrier: bar.barrier,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	startup := w.Snapshot().Runtime

	// A limits-only edit: the runtime is carried forward, so prev and next are the
	// same instance and no caller can mistake it for an identity change.
	atomicSave(t, path, strings.Replace(changedPrompt, "max_turns: 9", "max_turns: 5", 1))
	w.Revalidate(context.Background())

	pairs := bar.seenPairs()
	if len(pairs) != 1 {
		t.Fatalf("%d barrier calls, want 1", len(pairs))
	}
	if pairs[0][0] != startup {
		t.Errorf("prev = %+v, want the runtime in force", pairs[0][0])
	}
	if pairs[0][1] != startup {
		t.Error("a limits-only edit was announced as a new runtime; a caller would read it as an identity change and refuse it while busy")
	}

	// An adapter-moving edit: a genuinely new runtime, and prev is still what it
	// replaces.
	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	w.Revalidate(context.Background())

	pairs = bar.seenPairs()
	if len(pairs) != 2 {
		t.Fatalf("%d barrier calls, want 2", len(pairs))
	}
	if pairs[1][0] != startup || pairs[1][1] == startup {
		t.Errorf("prev/next = %+v; want the standing runtime and a rebuilt one", pairs[1])
	}
}
