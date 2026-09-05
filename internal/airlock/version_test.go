package airlock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The durable fields #284 added — a sandbox's stdin envelope, a binding's owed
// delivery and its recorded refusal — are each something an older daemon would
// load and silently ignore: planning inline against an envelope it cannot see,
// attaching to a streaming run whose prompt it never finishes, or replaying a
// refused body as an unanswered start. The record version is what turns each
// of those into a loud refusal on rollback, so a record that carries one of the
// fields must carry the version that names it, and a record that never gained
// one must stay readable by either binary.

// rewriteVersion rewrites a stored record's version in place, standing in for
// a file the previous binary wrote.
func rewriteVersion(t *testing.T, path string, version int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["version"] = version
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestASandboxRecordCarriesTheVersionThatNamesItsEnvelope(t *testing.T) {
	t.Parallel()
	store := NewDirStore(t.TempDir())
	f := newFixture(t, func(o *Options) { o.Store = store })
	ctx := context.Background()

	// A fresh acquire learns the envelope and writes the current version.
	id := f.acquire(ctx)
	rec, err := store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != SandboxRecordVersion || rec.Limits == nil {
		t.Fatalf("fresh record = version %d, limits %v; want %d with the envelope", rec.Version, rec.Limits, SandboxRecordVersion)
	}

	// The previous binary's record: version 4, no envelope. Readable by this
	// binary, and re-stamped the moment it gains the field.
	rec.Limits = nil
	if err := store.SaveSandbox(rec); err != nil {
		t.Fatal(err)
	}
	rewriteVersion(t, store.sandboxPath(testClaim), 4)
	legacy, err := store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatalf("a version-4 record is refused by the binary that succeeds it: %v", err)
	}
	if legacy.Version != 4 || legacy.Limits != nil {
		t.Fatalf("legacy record = %+v; want version 4 with no envelope", legacy)
	}
	if _, err := f.sub.Workspaces.Attach(ctx, testClaim); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	learned, err := store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatal(err)
	}
	if learned.Limits == nil || learned.Version != SandboxRecordVersion || learned.ProfileRevision != id.ProfileRevision {
		t.Fatalf("record after learning the envelope = %+v; want version %d", learned, SandboxRecordVersion)
	}

	// A record whose envelope stays unreadable keeps the version it had, so a
	// rollback can still address the sandbox.
	rec.Limits = nil
	if err := store.SaveSandbox(rec); err != nil {
		t.Fatal(err)
	}
	rewriteVersion(t, store.sandboxPath(testClaim), 4)
	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", airlocktest.Revision("rolled-forward"))
	if _, err := f.sub.Workspaces.Attach(ctx, testClaim); err != nil {
		t.Fatalf("Attach under a rolled-forward profile: %v", err)
	}
	if kept, _ := store.LoadSandbox(testClaim); kept.Version != 4 || kept.Limits != nil {
		t.Fatalf("a record that gained nothing was re-stamped: %+v", kept)
	}

	// And the guard itself: a record from a binary this one does not know is
	// refused, never half-read.
	rewriteVersion(t, store.sandboxPath(testClaim), SandboxRecordVersion+1)
	if _, err := store.LoadSandbox(testClaim); err == nil || !strings.Contains(err.Error(), "understands") {
		t.Fatalf("a newer record loaded: %v", err)
	}
}

func TestAStartBindingCarriesTheVersionThatNamesItsFields(t *testing.T) {
	t.Parallel()
	store := NewDirStore(t.TempDir())
	f := newFixture(t, func(o *Options) { o.Store = store })
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 16, Chunk: 16, Total: 0})
	ctx := context.Background()
	id := f.acquire(ctx)

	// The previous binary's binding: version 3, an unanswered start.
	reserve := func(run string) remote.ProcessRef {
		req := promptSpec(id, strings.Repeat("s", 64))
		ref := mustRef(t, id, remote.RunID(run), req)
		auth, err := f.sub.client.keyedAuth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReserveBinding(ref.String(), f.sub.binding, auth.principalBinding, time.Now()); err != nil {
			t.Fatal(err)
		}
		rewriteVersion(t, store.bindingPath(ref.String()), 3)
		if b, err := store.LoadBinding(ref.String()); err != nil || b.Version != 3 {
			t.Fatalf("legacy binding = (%+v, %v); want version 3 readable", b, err)
		}
		return ref
	}

	t.Run("recording a refusal", func(t *testing.T) {
		ref := reserve("v-refused")
		refusal := StartRefusal{Code: "payload_too_large", Fingerprint: "sha256:00", RefusedAt: time.Now()}
		if err := store.RecordRefusal(ref.String(), f.sub.binding, refusal); err != nil {
			t.Fatal(err)
		}
		b, err := store.LoadBinding(ref.String())
		if err != nil || b.Version != StartBindingVersion || b.Refusal == nil {
			t.Fatalf("refused binding = (%+v, %v); want version %d", b, err, StartBindingVersion)
		}
		// Re-arming for a different body keeps the new version: the refusal is
		// gone, but the reader that wrote it is the one that understands the fence.
		if _, err := store.RenewStart(ref.String(), f.sub.binding, b.PrincipalBinding, time.Now()); err != nil {
			t.Fatal(err)
		}
		if renewed, _ := store.LoadBinding(ref.String()); renewed.Version != StartBindingVersion || renewed.Refusal != nil {
			t.Fatalf("renewed binding = %+v", renewed)
		}
	})

	t.Run("owing a streaming delivery", func(t *testing.T) {
		ref := reserve("v-pending")
		if err := store.SetStdinPending(ref.String(), f.sub.binding, true); err != nil {
			t.Fatal(err)
		}
		b, err := store.LoadBinding(ref.String())
		if err != nil || b.Version != StartBindingVersion || !b.StdinPending {
			t.Fatalf("owed binding = (%+v, %v); want version %d", b, err, StartBindingVersion)
		}
	})

	t.Run("a start that gains nothing keeps its version", func(t *testing.T) {
		ref := reserve("v-plain")
		if err := store.SaveBinding(ref.String(), f.sub.binding, "run_x"); err != nil {
			t.Fatal(err)
		}
		if b, _ := store.LoadBinding(ref.String()); b.Version != 3 || b.RunID != "run_x" {
			t.Fatalf("a binding that gained no new field was re-stamped: %+v", b)
		}
	})

	t.Run("a newer binding is refused", func(t *testing.T) {
		ref := reserve("v-newer")
		rewriteVersion(t, store.bindingPath(ref.String()), StartBindingVersion+1)
		_, err := store.LoadBinding(ref.String())
		if err == nil || errors.Is(err, ErrNoRunBinding) || !strings.Contains(err.Error(), "understands") {
			t.Fatalf("a newer binding loaded: %v", err)
		}
	})

	// End to end through the backend: a fresh streaming start writes the
	// current version, and so does a refused one.
	req := promptSpec(id, strings.Repeat("e", 64))
	ref := mustRef(t, id, "v-stream", req)
	if _, err := f.sub.Processes.Start(ctx, ref, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if b, _ := store.LoadBinding(ref.String()); b.Version != StartBindingVersion || b.StdinPending {
		t.Fatalf("binding after a streaming start = %+v", b)
	}
}
