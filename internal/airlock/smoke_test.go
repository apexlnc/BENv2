package airlock

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The optional, credential-gated kind smoke: one fixture command, in one real
// sandbox, against a real Airlock.
//
// It skips unless every variable below is set, which is what keeps unit and
// contract CI offline and deterministic — the same call docs/SMOKE.md makes
// about the §12.4 smoke, and for the same reason. `make check` never reaches
// this code path.
//
// What it proves that the contract fake cannot: that this client still matches
// the world. The fake is faithful to the frozen contract as of the last time
// somebody checked it, and a contract revision — a renamed field, a new required
// header, a changed status — is invisible to every other test in this repository
// until this one runs.
//
// scripts/airlock-smoke.sh is the wrapper; see docs/AIRLOCK.md.
const (
	smokeURLVar     = "BEN_AIRLOCK_URL"
	smokeTokenVar   = "BEN_AIRLOCK_TOKEN" //nolint:gosec // the variable's name, not a credential
	smokeProfileVar = "BEN_AIRLOCK_PROFILE"
)

func TestAirlockSmoke(t *testing.T) {
	baseURL, token, profile := os.Getenv(smokeURLVar), os.Getenv(smokeTokenVar), os.Getenv(smokeProfileVar)
	if baseURL == "" || token == "" || profile == "" {
		t.Skipf("set %s, %s and %s to run the Airlock smoke", smokeURLVar, smokeTokenVar, smokeProfileVar)
	}

	sub, err := New(Options{
		BaseURL: baseURL,
		Auth:    &tokenSource{value: token},
		Profile: profile,
		Store:   NewDirStore(t.TempDir()),
		Timeouts: Timeouts{
			Request: 30 * time.Second, Poll: 70 * time.Second, PollWait: 30 * time.Second,
			Settle: 5 * time.Minute, Retries: 4,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// A claim cycle nothing else can collide with: the epoch is the process's
	// own start time, so a re-run never replays a previous run's key.
	claim := remote.Claim{Repository: "srhg-ai/ben", Issue: "airlock-smoke", Epoch: time.Now().UnixNano()}
	id, err := sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: claim, Branch: "ben/airlock-smoke", BaseSHA: strings.Repeat("0", 40),
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Logf("acquired sandbox %s at profile revision %s", id.SandboxID, id.ProfileRevision)

	// Whatever happens below, the sandbox is released. A smoke that leaked one
	// would be a bill rather than a test.
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()
		if err := sub.Workspaces.Delete(cleanup, claim); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}()

	const marker = "ben-airlock-smoke-ok"
	runSpec := remote.ProcessSpec{
		Identity: id,
		Argv:     []string{"/bin/sh", "-c", "echo " + marker},
		Limits:   core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: 5 * time.Minute},
	}
	digest, err := remote.ProcessRequestDigest(runSpec)
	if err != nil {
		t.Fatalf("ProcessRequestDigest: %v", err)
	}
	ref := remote.ProcessRef{Identity: id, RunID: "smoke-1", RequestDigest: digest}

	if _, err := sub.Processes.Start(ctx, ref, runSpec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var stdout bytes.Buffer
	cursor := remote.Cursor(0)
	for {
		batch, err := sub.Processes.Events(ctx, ref, cursor)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(batch) == 0 {
			break // the stream is sealed
		}
		for _, env := range batch {
			if env.Stream == remote.StreamStdout {
				stdout.Write(env.Payload)
			}
			cursor = remote.Cursor(env.Seq)
		}
	}
	if !bytes.Contains(stdout.Bytes(), []byte(marker)) {
		t.Fatalf("the fixture command's output did not reach BEN: %q", stdout.String())
	}

	status, err := sub.Processes.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Reaped() {
		t.Errorf("the process was not reported reaped: %+v", status)
	}
	// The one that matters: only an explicit domain-quiet observation authorizes
	// reusing the workspace.
	if !remote.MayReuse(status) {
		t.Fatalf("the run's execution domain was never observed quiet: %+v", status)
	}
}
