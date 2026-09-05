package remote_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The tests in this package are an *external* test package on purpose: the
// scripted fake imports internal/remote, so anything driving it from inside the
// package would be an import cycle. Everything asserted here is therefore
// asserted through the exported boundary, which is also what a real adapter sees.

const (
	testRepo    = "acme/widgets"
	testIssue   = "42"
	testEpoch   = int64(7)
	testBranch  = "ben/issue-42"
	testBaseSHA = "0000000000000000000000000000000000000001"
	testProfile = "profile-rev-1"
	testSession = "session-abc"
)

func testClaim() remote.Claim {
	return remote.Claim{Repository: testRepo, Issue: testIssue, Epoch: testEpoch}
}

// rig is one daemon's view of a remote substrate: a backend, a durable store, and
// the runner assembly would build over them.
//
// The store is separate from the runner deliberately, because a restart is
// modelled by building a *second* runner over the *same* store — which is exactly
// what happens on the host, and what makes "no duplicate dispatch" a property of
// the durable record rather than of an object that survived.
type rig struct {
	backend  *remotetest.Backend
	store    *remotetest.MemStore
	consumer *remotetest.Consumer
	run      remote.RunID
	claim    remote.Claim
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return &rig{
		backend:  remotetest.New(testProfile),
		store:    remotetest.NewMemStore(),
		consumer: remotetest.NewConsumer(),
		run:      "run-42-1",
		claim:    testClaim(),
	}
}

// acquire takes the workspace, which is what any dispatch must happen inside.
func (r *rig) acquire(t *testing.T) remote.Identity {
	t.Helper()
	id, err := r.backend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
		Claim:   r.claim,
		Branch:  testBranch,
		BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return id
}

// runner builds a runner over this rig's backend and store. Called twice by the
// restart tests; the second one is the new daemon.
func (r *rig) runner(t *testing.T, id remote.Identity) *remote.Runner {
	return r.runnerWithEnv(t, id, map[string]string{"BEN_RUN_ID": "42-1"})
}

// runnerWith is the same runner with the assembly seams a test needs to set
// directly — the reconnect window above all, whose real defaults are sized in
// wall-clock seconds for an API rollout.
func (r *rig) runnerWith(t *testing.T, id remote.Identity, mutate ...func(*remote.Options)) *remote.Runner {
	return r.runnerWithEnv(t, id, map[string]string{"BEN_RUN_ID": "42-1"}, mutate...)
}

func (r *rig) runnerWithEnv(
	t *testing.T,
	id remote.Identity,
	env map[string]string,
	mutate ...func(*remote.Options),
) *remote.Runner {
	t.Helper()
	opts := remote.Options{
		Backend:   r.backend,
		Store:     r.store,
		Consumer:  r.consumer,
		Translate: remotetest.Translate,
		Bind: func(core.RunSpec) (remote.Binding, error) {
			return remote.Binding{
				Identity: id,
				Run:      r.run,
				Meta: remote.Meta{
					TemplateRevision: "tmpl-1",
					PromptDigest:     remote.PromptDigest("do the thing"),
					Provider:         "claude-code",
					Model:            "claude-opus-5",
					Transcript:       "transcripts/run-42-1.jsonl",
				},
			}, nil
		},
		Invoke: func(spec core.RunSpec) (remote.Invocation, error) {
			return remote.Invocation{
				Argv:  []string{"claude", "--print"},
				Env:   env,
				Stdin: []byte(spec.Prompt),
			}, nil
		},
		Capabilities: core.Capabilities{Resume: true, Usage: true},
		// Several cases here drive a backend that cannot be read, and a failed
		// read logs. The rig discards it so a case that is *about* the logging
		// installs its own recorder; the mutators below run after this, so
		// withLogger still wins.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	runner, err := remote.New(opts)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	return runner
}

func testSpec() core.RunSpec {
	return core.RunSpec{Prompt: "do the thing", Limits: core.RunLimits{
		StallTimeout: 37 * time.Second, AttemptTimeout: 11 * time.Minute,
		MaxTurns: 4, MaxCostUSD: 2.5,
	}}
}

func testProcessRef(id remote.Identity, run remote.RunID) remote.ProcessRef {
	spec := remote.ProcessSpec{
		Identity: id,
		Argv:     []string{"claude", "--print"},
		Env:      map[string]string{"BEN_RUN_ID": "42-1"},
		Stdin:    []byte(testSpec().Prompt),
		Limits:   testSpec().Limits,
	}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		panic(err)
	}
	return remote.ProcessRef{Identity: id, RunID: run, RequestDigest: digest}
}

// collect drains a handle's stream and returns the event types, in order.
func collect(t *testing.T, h core.RunHandle) []core.Event {
	t.Helper()
	var out []core.Event
	for ev := range h.Events() {
		out = append(out, ev)
	}
	<-h.Done()
	return out
}

func types(evs []core.Event) []core.EventType {
	out := make([]core.EventType, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func sameTypes(got []core.EventType, want ...core.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
