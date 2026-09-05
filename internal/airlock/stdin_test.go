package airlock

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// How a prompt reaches a run, and what a refused one leaves behind (#284).
//
// The deployed reviewer profile admits 64 KiB of inline stdin. A reviewer
// prompt for a 64 KB diff is larger than the diff, so the start was refused
// with 413 on every sweep and the refusal was read as an unanswered start.
// These tests hold the client to the contract's second path — streaming,
// offset-addressed and receipted — and to the reading of a pre-claim refusal
// as the definite, recorded answer it is.

func ready(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.sub.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}

func promptSpec(id remote.Identity, prompt string) remote.ProcessSpec {
	s := spec(id)
	s.Stdin = []byte(prompt)
	return s
}

func countRequests(f *fixture, method, suffix string) int {
	n := 0
	for _, r := range f.srv.Requests() {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

func TestReadyReadsTheProfileStdinEnvelope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got := f.sub.StdinLimits(); got.Known() {
		t.Fatalf("limits before Ready = %+v, want unknown", got)
	}
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 100, Chunk: 40, Total: 1000, RequestBody: 4096})
	ready(t, f)
	want := StdinLimits{Inline: 100, Chunk: 40, Total: 1000, RequestBody: 4096}
	if got := f.sub.StdinLimits(); got != want || !got.Known() {
		t.Fatalf("limits after Ready = %+v, want %+v", got, want)
	}
	// The sandbox records the envelope it pinned, which is what dispatch reads.
	id := f.acquire(context.Background())
	rec := f.store.(*memStore).sandbox(testClaim)
	if rec.Limits == nil || stdinLimitsOf(rec.Limits) != want || rec.ProfileRevision != id.ProfileRevision {
		t.Fatalf("sandbox record after acquire = %+v; want the pinned envelope %+v", rec, want)
	}
	if !want.Admits(1000) || want.Admits(1001) {
		t.Fatal("Admits does not hold the total bound")
	}
	if !(StdinLimits{Chunk: 1}).Admits(1 << 40) {
		t.Fatal("a zero total bound must read as unbounded, as the server reads it")
	}
}

func TestAPromptWithinTheInlineBoundTravelsInline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 64, Chunk: 16, Total: 0})
	ready(t, f)
	ctx := context.Background()
	id := f.acquire(ctx)
	prompt := strings.Repeat("p", 64)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-inline", req)

	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "inline" {
		t.Fatalf("stdin mode = %q, want inline for a prompt exactly at the bound", mode)
	}
	if !bytes.Equal(f.srv.Stdin(st.BackendRunID), []byte(prompt)) {
		t.Fatal("the run did not receive the prompt")
	}
	if n := countRequests(f, "POST", "/stdin"); n != 0 {
		t.Fatalf("an inline prompt caused %d stdin writes", n)
	}
	if b := f.store.(*memStore).binding(ref.String()); b.StdinPending || b.RunID != st.BackendRunID {
		t.Fatalf("binding after an inline start = %+v", b)
	}
}

func TestAPromptOverTheInlineBoundStreamsInChunksAndCloses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 64, Chunk: 16, Total: 0})
	ready(t, f)
	ctx := context.Background()
	id := f.acquire(ctx)
	prompt := strings.Repeat("0123456789", 10) // 100 bytes: six full chunks and a four-byte tail
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-stream", req)

	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("runs = %v, want exactly one", got)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "streaming" {
		t.Fatalf("stdin mode = %q, want streaming for a prompt over the inline bound", mode)
	}
	if !bytes.Equal(f.srv.Stdin(st.BackendRunID), []byte(prompt)) {
		t.Fatalf("the run received %d bytes of a %d-byte prompt", len(f.srv.Stdin(st.BackendRunID)), len(prompt))
	}
	if !f.srv.StdinClosed(st.BackendRunID) {
		t.Fatal("stdin was never closed; the reviewer would wait for EOF until the hard timeout")
	}
	if writes := f.srv.StdinWrites(st.BackendRunID); writes != 7 {
		t.Fatalf("stdin writes = %d, want 7 chunks of at most 16 bytes", writes)
	}
	for _, r := range f.srv.Requests() {
		if r.Method != "POST" || !strings.HasSuffix(r.Path, "/runs") {
			continue
		}
		if !strings.Contains(string(r.Body), `"mode":"streaming"`) || strings.Contains(string(r.Body), "inline_b64") {
			t.Fatalf("the start body carried the wrong stdin shape: %s", r.Body)
		}
	}
	b := f.store.(*memStore).binding(ref.String())
	if b.StdinPending || b.RunID != st.BackendRunID {
		t.Fatalf("binding after a delivered streaming start = %+v", b)
	}

	// A replayed Start is a read of the same run: no second run, no second
	// delivery, no reopened stdin.
	again, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil || again.BackendRunID != st.BackendRunID {
		t.Fatalf("replayed Start = (%+v, %v), want the same run", again, err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("a replayed streaming start produced %d runs", len(got))
	}
	if writes := f.srv.StdinWrites(st.BackendRunID); writes != 7 {
		t.Fatalf("a replayed start wrote stdin again: %d writes", writes)
	}
}

// A process that dies between creating a streaming run and closing its stdin
// leaves a run waiting for a prompt that will never finish arriving. The
// binding says the delivery is owed, and the next Start at the address — this
// process or the next — completes it: exact resends for what landed, fresh
// writes for what did not, one close.
func TestAStreamingStartResumesAnInterruptedDeliveryOnReplay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 16, Chunk: 16, Total: 0})
	ready(t, f)
	ctx := context.Background()
	id := f.acquire(ctx)
	prompt := strings.Repeat("abcd", 16) // 64 bytes: four chunks
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-resume", req)

	// Two chunks land; the third meets a refusal the writer cannot retry past.
	// A daemon dying at that instant leaves exactly this binding.
	f.srv.FailStdinAt(32, 403, "forbidden", "scripted interruption")
	if _, err := f.sub.Processes.Start(ctx, ref, req); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("interrupted Start = %v, want the write's refusal", err)
	}
	runID := f.srv.RunIDs()[0]
	if got := f.srv.Stdin(runID); len(got) != 32 || f.srv.StdinClosed(runID) {
		t.Fatalf("after the interruption the run holds %d bytes, closed=%v; want 32 and open", len(got), f.srv.StdinClosed(runID))
	}
	b := f.store.(*memStore).binding(ref.String())
	if !b.StdinPending || b.RunID != runID {
		t.Fatalf("binding after the interruption = %+v; want the run bound and its stdin still owed", b)
	}

	// A fresh process over the same store: its Start is the resume.
	sub := f.rebuild()
	if err := sub.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("resumed Start: %v", err)
	}
	if st.BackendRunID != runID || len(f.srv.RunIDs()) != 1 {
		t.Fatalf("the resume addressed %s over runs %v; want the one interrupted run", st.BackendRunID, f.srv.RunIDs())
	}
	if !bytes.Equal(f.srv.Stdin(runID), []byte(prompt)) || !f.srv.StdinClosed(runID) {
		t.Fatalf("after the resume the run holds %d of %d bytes, closed=%v", len(f.srv.Stdin(runID)), len(prompt), f.srv.StdinClosed(runID))
	}
	// Two writes landed before the interruption; the resume walked both as
	// receipted no-ops and appended the remaining two.
	if writes := f.srv.StdinWrites(runID); writes != 6 {
		t.Fatalf("stdin writes = %d, want 2 originals + 2 exact resends + 2 fresh", writes)
	}
	if b := f.store.(*memStore).binding(ref.String()); b.StdinPending {
		t.Fatalf("the completed delivery left the binding owing: %+v", b)
	}
}

// unreadableEnvelope puts a fixture's sandbox in the state a pre-#284 record
// is in on a rolled-forward profile: no recorded envelope, and the profile's
// current revision is not the one the sandbox pinned, so none can be learned.
// The sandbox itself is still judged by its pinned limits — the fake keeps
// them on the sandbox, as the contract does.
func unreadableEnvelope(f *fixture) {
	f.store.(*memStore).forgetLimits(testClaim)
	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", airlocktest.Revision("rolled-forward"))
}

// The incident's shape: an envelope the client cannot read, a prompt over
// the inline bound, and the server's 413. That answer is definite, and every
// later read of the address says so — never "unresolved".
func TestAnInlineRefusalIsDefiniteAndAnsweredLocallyThereafter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	unreadableEnvelope(f)
	prompt := strings.Repeat("x", int(airlocktest.DefaultLimits.Inline)+1)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-413", req)

	_, err := f.sub.Processes.Start(ctx, ref, req)
	var refused *remote.ProcessRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("Start over the inline bound = %v, want a remote.ProcessRefusal", err)
	}
	if !errors.Is(err, remote.ErrProcessRefused) || !errors.Is(err, remote.ErrNoProcess) {
		t.Fatalf("a refusal must be both ErrProcessRefused and ErrNoProcess; got %v", err)
	}
	if errors.Is(err, remote.ErrProcessUnresolved) {
		t.Fatalf("a definite refusal read as an unanswered start: %v", err)
	}
	if refused.Code != "payload_too_large" || refused.LimitBytes != airlocktest.DefaultLimits.Inline {
		t.Fatalf("refusal = %+v; want payload_too_large at the inline bound", refused)
	}
	if strings.Contains(refused.Message, "xxxx") {
		t.Fatal("the refusal carried prompt bytes")
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("a refused start created %d runs", len(got))
	}

	b := f.store.(*memStore).binding(ref.String())
	if b.Refusal == nil || b.Refusal.Code != "payload_too_large" || b.Refusal.Fingerprint == "" || b.RunID != "" {
		t.Fatalf("binding after a refusal = %+v; want the refusal recorded and no run", b)
	}

	reads := map[string]func() error{
		"status": func() error { _, err := f.sub.Processes.Status(ctx, ref); return err },
		"events": func() error { _, err := f.sub.Processes.Events(ctx, ref, 0); return err },
		"stdin":  func() error { return f.sub.Processes.Stdin(ctx, ref, []byte("x")) },
		"stop": func() error {
			_, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopDiscard})
			return err
		},
		"wait": func() error { _, err := f.sub.Processes.Wait(ctx, ref); return err },
	}
	for name, read := range reads {
		if err := read(); !errors.Is(err, remote.ErrProcessRefused) || errors.Is(err, remote.ErrProcessUnresolved) {
			t.Errorf("%s after a refusal = %v; want refused, never unresolved", name, err)
		}
	}

	// Re-offering the same body is answered from the record: no request goes out
	// for an answer the backend has already given.
	posts := countRequests(f, "POST", "/runs")
	if _, err := f.sub.Processes.Start(ctx, ref, req); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("re-offered Start = %v, want the recorded refusal", err)
	}
	if got := countRequests(f, "POST", "/runs"); got != posts {
		t.Fatalf("re-offering a refused body sent %d more startRun requests", got-posts)
	}
}

// The recovery an existing refused record gets from a daemon that can now
// deliver the prompt: the same address, the same key, a different body — which
// the contract admits, because a pre-claim refusal stored nothing under the key.
func TestARefusedAddressAcceptsADifferentBody(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	unreadableEnvelope(f)
	prompt := strings.Repeat("y", int(airlocktest.DefaultLimits.Inline)+1)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-upgrade", req)
	if _, err := f.sub.Processes.Start(ctx, ref, req); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("Start with an unreadable envelope = %v, want the server's refusal", err)
	}

	// The pinned envelope becomes readable — here the profile's current revision
	// is the pin again — and the very next Start learns it: the same prompt now
	// streams, as a different body under the same address.
	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", id.ProfileRevision)
	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start after learning the limits: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 || got[0] != st.BackendRunID {
		t.Fatalf("runs = %v, want exactly the one just started", got)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "streaming" {
		t.Fatalf("stdin mode = %q, want streaming", mode)
	}
	if !bytes.Equal(f.srv.Stdin(st.BackendRunID), []byte(prompt)) || !f.srv.StdinClosed(st.BackendRunID) {
		t.Fatal("the run did not receive the whole prompt")
	}
	b := f.store.(*memStore).binding(ref.String())
	if b.Refusal != nil || b.RunID != st.BackendRunID {
		t.Fatalf("binding after the accepted start = %+v; want the run bound and the refusal cleared", b)
	}
	// Both starts went out under the one key the address derives.
	keys := map[string]int{}
	for _, r := range f.srv.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/runs") {
			keys[r.Key]++
		}
	}
	if len(keys) != 1 {
		t.Fatalf("startRun requests used %d idempotency keys, want one: %v", len(keys), keys)
	}
	for _, n := range keys {
		if n != 2 {
			t.Fatalf("startRun requests under the key = %d, want the refused one and the accepted one", n)
		}
	}
}

// A refused address holds no unanswered start, so the replay fence written for
// the refused attempt must not expire it: an unchanged body is still answered
// from the record a day later, and a different body re-arms the fence for its
// own attempt rather than inheriting one that has run out.
func TestARefusedAddressOutlivesTheReplayWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	unreadableEnvelope(f)
	prompt := strings.Repeat("w", int(airlocktest.DefaultLimits.Inline)+1)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-aged", req)
	if _, err := f.sub.Processes.Start(ctx, ref, req); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("Start = %v, want the server's refusal", err)
	}
	store := f.store.(*memStore)
	store.setBindingAttemptedAt(ref.String(), time.Now().Add(-2*idempotencyReplayWindow))

	// Unchanged: still the recorded refusal, not an expired window.
	posts := countRequests(f, "POST", "/runs")
	_, err := f.sub.Processes.Start(ctx, ref, req)
	if !errors.Is(err, remote.ErrProcessRefused) || errors.Is(err, ErrStartReplayExpired) {
		t.Fatalf("re-offered Start a day later = %v, want the recorded refusal", err)
	}
	if got := countRequests(f, "POST", "/runs"); got != posts {
		t.Fatalf("an aged refused address sent %d requests for an unchanged body", got-posts)
	}
	if _, err := f.sub.Processes.Status(ctx, ref); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("Status a day later = %v, want refused", err)
	}

	// Changed: sent, and under a fence that starts now.
	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", id.ProfileRevision)
	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start with a different body a day later: %v", err)
	}
	b := store.binding(ref.String())
	if b.Refusal != nil || b.RunID != st.BackendRunID {
		t.Fatalf("binding after the renewed start = %+v", b)
	}
	if time.Since(b.StartAttemptedAt) > time.Minute {
		t.Fatalf("the renewed start kept the refused attempt's fence: %s", b.StartAttemptedAt)
	}
}

// A sandbox is judged by the revision it pinned, and a profile rolls forward
// without rewriting the sandboxes already on the old one. So the envelope a
// prompt is planned against has to be the sandbox's, recorded when it was
// readable, and never what the profile says at the moment of dispatch — or a
// rollout that loosens the inline bound sends every in-flight review a prompt
// its own sandbox refuses.
func TestLimitsAreThePinnedRevisionsNotTheCurrentProfiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 64, Chunk: 16, Total: 0})
	ctx := context.Background()
	old := f.acquire(ctx) // pinned at the 64-byte inline envelope

	// The operator publishes a revision that admits far more inline, and the
	// daemon reads it at its next readiness probe.
	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", airlocktest.Revision("v2"))
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 4096, Chunk: 4096, Total: 0})
	ready(t, f)
	if got := f.sub.StdinLimits(); got.Inline != 4096 {
		t.Fatalf("the current profile's inline bound = %d, want the rolled-forward 4096", got.Inline)
	}

	prompt := strings.Repeat("0123456789", 10) // 100 bytes: inline under v2, streaming under the pin
	req := promptSpec(old, prompt)
	ref := mustRef(t, old, "run-pinned", req)
	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start in the old-revision sandbox = %v; the current profile's bound was used for a sandbox that does not have it", err)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "streaming" {
		t.Fatalf("stdin mode in the pinned sandbox = %q, want streaming under its own 64-byte inline bound", mode)
	}
	if !bytes.Equal(f.srv.Stdin(st.BackendRunID), []byte(prompt)) || !f.srv.StdinClosed(st.BackendRunID) {
		t.Fatal("the pinned sandbox did not receive the whole prompt")
	}

	// A sandbox created after the rollout pins the new revision and takes the
	// same prompt inline.
	fresh, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim:  remote.Claim{Repository: testClaim.Repository, Issue: testClaim.Issue, Epoch: testClaim.Epoch + 1},
		Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire after the rollout: %v", err)
	}
	if fresh.ProfileRevision == old.ProfileRevision {
		t.Fatal("the fixture did not roll the profile forward")
	}
	freshReq := promptSpec(fresh, prompt)
	freshRef := mustRef(t, fresh, "run-fresh", freshReq)
	freshSt, err := f.sub.Processes.Start(ctx, freshRef, freshReq)
	if err != nil {
		t.Fatalf("Start in the new-revision sandbox: %v", err)
	}
	if mode := f.srv.StdinMode(freshSt.BackendRunID); mode != "inline" {
		t.Fatalf("stdin mode in the new sandbox = %q, want inline under its 4096-byte bound", mode)
	}
}

// A record written before the envelope existed learns it at the first start
// that finds the profile still at the pinned revision, and the plan follows.
func TestALegacyRecordLearnsItsEnvelopeAtTheFirstStart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 64, Chunk: 16, Total: 0})
	ctx := context.Background()
	id := f.acquire(ctx)
	store := f.store.(*memStore)
	store.forgetLimits(testClaim)
	if store.sandbox(testClaim).Limits != nil {
		t.Fatal("the fixture did not forget the envelope")
	}

	prompt := strings.Repeat("0123456789", 10)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-legacy", req)
	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start over a legacy record: %v", err)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "streaming" {
		t.Fatalf("stdin mode = %q, want streaming: the start should have learned the 64-byte inline bound first", mode)
	}
	if rec := store.sandbox(testClaim); rec.Limits == nil || rec.Limits.MaxStdinInlineBytes != 64 {
		t.Fatalf("the learned envelope was not recorded: %+v", rec.Limits)
	}
}

// The request-body bound is measured on the encoded request, before parsing.
// A prompt whose decoded length fits the inline bound can still, as base64
// inside JSON, exceed the body bound — and streaming writes are bodies too.
// Neither is discovered from a 413; both are planned.
func TestAnInlinePromptThatOverflowsTheRequestBodyStreams(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 1000, Chunk: 1000, Total: 0, RequestBody: 900})
	ctx := context.Background()
	id := f.acquire(ctx)
	prompt := strings.Repeat("z", 800) // under the inline bound decoded; over the body bound encoded
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-body", req)

	st, err := f.sub.Processes.Start(ctx, ref, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mode := f.srv.StdinMode(st.BackendRunID); mode != "streaming" {
		t.Fatalf("stdin mode = %q, want streaming for a body the request bound refuses", mode)
	}
	if !bytes.Equal(f.srv.Stdin(st.BackendRunID), []byte(prompt)) || !f.srv.StdinClosed(st.BackendRunID) {
		t.Fatalf("the run holds %d of %d bytes, closed=%v", len(f.srv.Stdin(st.BackendRunID)), len(prompt), f.srv.StdinClosed(st.BackendRunID))
	}
	writes := 0
	for _, r := range f.srv.Requests() {
		if r.Method != "POST" || (!strings.HasSuffix(r.Path, "/runs") && !strings.HasSuffix(r.Path, "/stdin")) {
			continue
		}
		if int64(len(r.Body)) > 900 {
			t.Fatalf("a %d-byte request body was sent under a 900-byte bound: %s", len(r.Body), r.Path)
		}
		if strings.HasSuffix(r.Path, "/stdin") {
			writes++
		}
	}
	if writes < 2 {
		t.Fatalf("stdin writes = %d; an 800-byte prompt cannot fit one 900-byte body once encoded", writes)
	}
	if b := f.store.(*memStore).binding(ref.String()); b.Refusal != nil || b.StdinPending {
		t.Fatalf("binding = %+v; want no refusal and nothing owed", b)
	}
}

// A refusal the daemon could not make durable must not escape as a refusal:
// a caller that read one would release the run while the binding on disk still
// says the start is unanswered. The persistence failure is what is returned,
// and the next start — which re-sends, because nothing is recorded — meets the
// same pre-claim answer and records it then.
func TestARefusalThatCannotBeRecordedStaysAmbiguous(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	unreadableEnvelope(f)
	store := f.store.(*memStore)
	disk := errors.New("scripted: the state directory is full")
	store.setRefusalFault(func(string) error { return disk })
	prompt := strings.Repeat("q", int(airlocktest.DefaultLimits.Inline)+1)
	req := promptSpec(id, prompt)
	ref := mustRef(t, id, "run-unrecorded", req)

	_, err := f.sub.Processes.Start(ctx, ref, req)
	if !errors.Is(err, disk) {
		t.Fatalf("Start = %v, want the persistence failure", err)
	}
	if errors.Is(err, remote.ErrProcessRefused) || errors.Is(err, remote.ErrNoProcess) {
		t.Fatalf("an unrecorded refusal escaped as a definite one: %v", err)
	}
	if b := store.binding(ref.String()); b.Refusal != nil || b.RunID != "" {
		t.Fatalf("binding after the failed write = %+v; want an unanswered start", b)
	}
	if _, err := f.sub.Processes.Status(ctx, ref); !errors.Is(err, remote.ErrProcessUnresolved) {
		t.Fatalf("Status after the failed write = %v; want the ambiguity retained", err)
	}

	// The disk recovers; the re-sent body is refused again and recorded.
	store.setRefusalFault(nil)
	if _, err := f.sub.Processes.Start(ctx, ref, req); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("second Start = %v, want the recorded refusal", err)
	}
	if b := store.binding(ref.String()); b.Refusal == nil {
		t.Fatalf("the refusal was not recorded once the store recovered: %+v", b)
	}
	if got := countRequests(f, "POST", "/runs"); got != 2 {
		t.Fatalf("startRun requests = %d, want the refused send and its re-send", got)
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("a refused start created %d runs", len(got))
	}
}

func TestAPromptOverTheTotalBoundIsRefusedWithoutARequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.SetProfileLimits(airlocktest.DefaultProfile, airlocktest.Limits{Inline: 8, Chunk: 8, Total: 32})
	ready(t, f)
	ctx := context.Background()
	id := f.acquire(ctx)
	req := promptSpec(id, strings.Repeat("z", 40))
	ref := mustRef(t, id, "run-total", req)

	_, err := f.sub.Processes.Start(ctx, ref, req)
	var refused *remote.ProcessRefusal
	if !errors.As(err, &refused) || refused.Code != "payload_too_large" || refused.LimitBytes != 32 {
		t.Fatalf("Start over the total bound = %v, want a payload_too_large refusal naming 32", err)
	}
	if n := countRequests(f, "POST", "/runs"); n != 0 {
		t.Fatalf("a prompt no path can deliver caused %d startRun requests", n)
	}
	if _, err := f.sub.Processes.Status(ctx, ref); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("Status after a local refusal = %v, want refused", err)
	}
	if b := f.store.(*memStore).binding(ref.String()); b.Refusal == nil || b.Refusal.Fingerprint == "" {
		t.Fatalf("a local refusal was not recorded: %+v", b)
	}
}
