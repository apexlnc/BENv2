package airlock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// eventPageLimit is how many events one JSON page may carry. The contract's
// ceiling is 1000; a page is bounded in bytes as well as in count by the
// profile's per-chunk limit, and a smaller page shortens the window between a
// durable consumer commit and the cursor that follows it.
const eventPageLimit = 256

// Processes is remote.ProcessBackend over Airlock runs.
//
// Every method is addressed by remote.ProcessRef, which is BEN's identity for
// one dispatch, and every method has to resolve that to Airlock's own run id
// before it can ask anything. That resolution is the durable binding this type
// owns (Store): Airlock scopes runs under their sandbox precisely so one
// consumer cannot address another's, and BEN's ref is an idempotency address
// rather than a resource name.
type Processes struct {
	client   *client
	store    Store
	timeouts Timeouts
	labels   map[string]string
	binding  SubstrateBinding
	// workspaces supplies the sandbox's single-active-run observation used to
	// resolve a Start whose response was lost. It is the sibling built over the
	// same client and durable store by New.
	workspaces *Workspaces

	// stdinOffsets is the byte position each run's stdin stream has reached.
	// In-memory: a streaming stdin write is within one attempt's lifetime, and
	// the contract's offset receipts make an exact resend a no-op that returns
	// the recorded next_offset — so a restart re-derives the offset from the
	// server rather than from a file BEN would have to keep in step.
	mu           sync.Mutex
	stdinOffsets map[string]int64
}

// Start dispatches the exact persisted request, or resolves the one already
// dispatched under the same key.
//
// Before the first request, a StartBinding records when keyed replay became
// possible. A known RunID is read directly; an unanswered start replays only
// while Airlock must still retain the keyed result. That local fence is what
// keeps remote.Runner's lost-response recovery from becoming a second dispatch
// after the server's 24-hour record expires.
func (p *Processes) Start(ctx context.Context, ref remote.ProcessRef, spec remote.ProcessSpec) (remote.Status, error) {
	if !ref.Complete() {
		return remote.Status{}, fmt.Errorf("%w: incomplete process reference", remote.ErrProcessMismatch)
	}
	if spec.Identity != ref.Identity {
		return remote.Status{}, fmt.Errorf("%w: %s names a different workspace than its request", remote.ErrProcessMismatch, ref)
	}
	if len(spec.Argv) == 0 {
		return remote.Status{}, fmt.Errorf("%w: %s has no argv", remote.ErrProcessMismatch, ref)
	}
	if err := spec.Git.Validate(); err != nil {
		return remote.Status{}, fmt.Errorf("%w: %s: %v", remote.ErrProcessMismatch, ref, err)
	}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		return remote.Status{}, fmt.Errorf("%w: %s: %v", remote.ErrProcessMismatch, ref, err)
	}
	if digest != ref.RequestDigest {
		return remote.Status{}, fmt.Errorf("%w: %s carries a different request digest", remote.ErrProcessMismatch, ref)
	}
	rec, err := loadBoundSandbox(p.store, p.binding, ref.Identity.Claim)
	if err != nil {
		return remote.Status{}, err
	}
	if rec.Limits == nil {
		// A record from before the envelope was recorded, or one whose pinned
		// revision was not the current one at every earlier chance. Unknown is
		// tolerated (planStdin); a failed read here is not fatal.
		if learned, err := p.workspaces.learnLimits(ctx, rec); err == nil {
			rec = learned
		}
	}
	limits := stdinLimitsOf(rec.Limits)
	// ProcessSpec.Env is the already-composed provider invocation, not the
	// orchestrator's RunSpec.Env. The latter is BEN_-only; the former also
	// carries the provider block's explicit env and documented API-key surface
	// (core.RemoteInvocation). Reapplying the RunSpec namespace rule here would
	// refuse every configured provider environment at the real backend while a
	// permissive ProcessBackend fake accepted it.
	env := maps.Clone(spec.Env)

	body := startRunRequest{
		Argv:   spec.Argv,
		Env:    env,
		Labels: p.runLabels(ref, spec.Git),
		Timeouts: &runTimeoutsRequest{
			// Both cross the boundary because Airlock enforces them while BEN is
			// disconnected, and reconnecting must not reset either
			// (remote.ProcessSpec.Limits).
			HardSeconds:        seconds(spec.Limits.AttemptTimeout),
			OutputStallSeconds: seconds(spec.Limits.StallTimeout),
		},
	}
	// Omitted stdin resolves to `{mode: closed}` by contract. Stated explicitly
	// anyway: the prompt travels on stdin for both provider adapters, and a
	// request whose stdin shape depended on whether a field was omitted would
	// fingerprint differently from one that spelled it. Which mode is the
	// pinned revision's decision as much as BEN's (planStdin): a prompt the
	// envelope admits inline goes inline, a larger one streams after the run
	// exists — and "admits" is judged on the encoded body the server will
	// measure, not on the decoded prompt alone.
	plan := planStdin(limits, spec.Stdin)
	body.Stdin = plan.request(spec.Stdin)
	encoded, err := json.Marshal(body)
	if err != nil {
		return remote.Status{}, fmt.Errorf("%w: %s: encoding the start body: %v", remote.ErrProcessMismatch, ref, err)
	}
	if plan.mode == StdinInline && limits.RequestBody > 0 && int64(len(encoded)) > limits.RequestBody {
		// The decoded prompt fits the inline bound and its base64 inside the JSON
		// body does not fit the request-body bound. Streaming is the path that
		// exists for exactly this, and choosing it here rather than learning it
		// from a 413 keeps a deliverable prompt from being recorded as refused.
		plan = stdinPlan{mode: StdinStreaming, chunk: limits.streamChunk()}
		body.Stdin = plan.request(spec.Stdin)
		if encoded, err = json.Marshal(body); err != nil {
			return remote.Status{}, fmt.Errorf("%w: %s: encoding the start body: %v", remote.ErrProcessMismatch, ref, err)
		}
	}
	fingerprint := fingerprintOf(encoded)
	binding, auth, replayCtx, cancel, err := prepareStart(ctx, p.client, p.store, p.binding, ref.String())
	if err != nil {
		return remote.Status{}, err
	}
	defer cancel()
	if binding.RunID != "" {
		run, err := p.get(ctx, ref, binding.RunID)
		if err != nil {
			return remote.Status{}, err
		}
		if binding.StdinPending {
			// A process died between creating this streaming run and closing its
			// stdin. Same bytes at the same address, and the server's offset
			// receipts make every resend exact, so finishing the delivery here is
			// a resume rather than a second prompt.
			if err := p.completeStdin(ctx, ref, run.RunID, spec.Stdin, plan.chunk); err != nil {
				return remote.Status{}, err
			}
		}
		return statusOf(run), nil
	}
	if binding.Refusal != nil {
		if binding.Refusal.Fingerprint == fingerprint {
			// The backend already refused this exact body. A pre-claim refusal
			// stores nothing under the key, so sending it again could only be
			// refused again; answering locally is what keeps a caller that
			// re-offers an unchanged request from spending a request per offer.
			return remote.Status{}, refusalError(ref, *binding.Refusal)
		}
		// A different body under the same key is a first use of it — the
		// refused one was never claimed — so the reservation is re-armed: the
		// refusal is cleared and the replay fence restarts now, and a lost
		// response to this send reads as an unanswered start rather than as the
		// old refusal.
		now := time.Now().UTC()
		binding, err = p.store.RenewStart(ref.String(), p.binding, auth.principalBinding, now)
		if err != nil {
			return remote.Status{}, err
		}
		cancel()
		replayCtx, cancel = context.WithDeadline(ctx, now.Add(idempotencyReplayWindow))
		defer cancel()
	}
	if plan.refusal != nil {
		// The prompt cannot be delivered under the profile's total stdin bound
		// by either path. Recorded exactly as the backend's refusal would be, so
		// the same body is not re-offered while a different one still is.
		refusal := *plan.refusal
		refusal.Fingerprint, refusal.RefusedAt = fingerprint, time.Now().UTC()
		if err := p.store.RecordRefusal(ref.String(), p.binding, refusal); err != nil {
			return remote.Status{}, err
		}
		return remote.Status{}, refusalError(ref, refusal)
	}
	if plan.mode == StdinStreaming && !binding.StdinPending {
		// Owed before the run can exist. Marking it afterwards would leave a
		// crash window in which a streaming run waits on a prompt its binding
		// says has arrived.
		if err := p.store.SetStdinPending(ref.String(), p.binding, true); err != nil {
			return remote.Status{}, err
		}
	}

	var run Run
	err = p.client.do(replayCtx, request{
		method:    "POST",
		path:      "/v2/sandboxes/" + url.PathEscape(ref.Identity.SandboxID) + "/runs",
		idem:      runKey(ref),
		body:      body,
		authToken: auth.token,
		out:       &run,
	})
	if err != nil {
		err = p.classifyStart(ref, err)
		var refused *remote.ProcessRefusal
		if errors.As(err, &refused) {
			// Durable before it is returned: the refusal is what every later read
			// of this address answers with, and a daemon that crashed holding it
			// only in memory would come back reading the address as unanswered.
			refusal := StartRefusal{
				Code: refused.Code, Message: refused.Message, LimitBytes: refused.LimitBytes,
				Fingerprint: fingerprint, RefusedAt: time.Now().UTC(),
			}
			if saveErr := p.store.RecordRefusal(ref.String(), p.binding, refusal); saveErr != nil {
				// The backend's answer was definite; this daemon's record of it is
				// not. Returned as the persistence failure *alone*, so no caller
				// reads a refusal here and releases the run while the binding on
				// disk still says "unanswered". Nothing is lost by that: a pre-claim
				// refusal stored nothing server-side, so the next Start — which
				// re-sends, because nothing is recorded — meets the same answer and
				// records it then.
				return remote.Status{}, fmt.Errorf("airlock: %s: the backend refused the start (%s) but the refusal could not be recorded: %w",
					ref, refused.Code, saveErr)
			}
		}
		return remote.Status{}, err
	}
	st, err := p.adopt(ref, run)
	if err != nil {
		return remote.Status{}, err
	}
	if plan.mode == StdinStreaming {
		if err := p.completeStdin(ctx, ref, run.RunID, spec.Stdin, plan.chunk); err != nil {
			return remote.Status{}, err
		}
	}
	return st, nil
}

// classifyStart maps the start refusals BEN routes on.
//
// `idempotency_key_conflict` is the sharpest: the same key with a different
// payload. That is precisely remote.ErrProcessMismatch — the same address with a
// different request — and it must never be retried, because a retry either
// conflicts again or, after the replay window, creates a second run.
//
// The three request-validation codes are the contract's *pre-claim* outcomes:
// the body was refused before the idempotency claim, so nothing was created and
// nothing is stored under the key. They are definite, never retryable as
// composed, and carried as remote.ProcessRefusal so a caller can say why (#284).
// Leaving them as a bare APIError is how a 413 was read as an unanswered start
// and replayed on every sweep for five hours.
func (p *Processes) classifyStart(ref remote.ProcessRef, err error) error {
	switch {
	case hasCode(err, CodeIdempotencyKeyConflict):
		return fmt.Errorf("%w: %s: %w", remote.ErrProcessMismatch, ref, err)
	case hasCode(err, CodeForbidden, CodeNotFound):
		return fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
	case hasCode(err, CodePayloadTooLarge, CodeInvalidRequest, CodeEnvRejected):
		var apiErr *APIError
		_ = errors.As(err, &apiErr) // hasCode has just proven it
		return &remote.ProcessRefusal{
			Code: string(apiErr.Code), Message: apiErr.Message, LimitBytes: apiErr.LimitBytes,
			Cause: fmt.Errorf("airlock: %s: %w", ref, err),
		}
	}
	return err
}

// adopt records the Airlock run id before the caller may act on the status.
//
// The write is not best-effort. Once a run exists, its id is the only permanent
// handle to it: the idempotency key that produced it expires after 24 hours,
// and a daemon that lost the id in that window would have a live run it can
// neither observe nor stop. Failing the call keeps the ambiguity honest, and
// remote.Runner's replay resolves the same run again on the next tick.
func (p *Processes) adopt(ref remote.ProcessRef, run Run) (remote.Status, error) {
	if run.RunID == "" {
		return remote.Status{}, fmt.Errorf("%w: %s answered with no run id", ErrUnexpectedRun, ref)
	}
	if run.SandboxID != "" && run.SandboxID != ref.Identity.SandboxID {
		return remote.Status{}, fmt.Errorf("%w: %s answered for sandbox %s", ErrUnexpectedRun, ref, run.SandboxID)
	}
	if err := p.store.SaveBinding(ref.String(), p.binding, run.RunID); err != nil {
		return remote.Status{}, err
	}
	return statusOf(run), nil
}

// Attach addresses a run by the backend id a successful Start returned.
func (p *Processes) Attach(ctx context.Context, ref remote.ProcessRef, backendRunID string) (remote.Status, error) {
	if backendRunID == "" {
		return remote.Status{}, fmt.Errorf("%w: %s has no permanent backend run id", remote.ErrProcessUnresolved, ref)
	}
	if _, err := loadBoundSandbox(p.store, p.binding, ref.Identity.Claim); err != nil {
		return remote.Status{}, err
	}
	// Recorded before the call. remote.Journal is handing back an id it durably
	// holds, and this store is what every *other* method on this type resolves
	// through — so an attach that did not write it would leave Status, Events and
	// Stop unable to name the run they are attached to.
	if err := p.store.SaveBinding(ref.String(), p.binding, backendRunID); err != nil {
		return remote.Status{}, err
	}
	run, err := p.get(ctx, ref, backendRunID)
	if err != nil {
		return remote.Status{}, err
	}
	return statusOf(run), nil
}

func (p *Processes) Status(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	runID, err := p.resolve(ref)
	if errors.Is(err, remote.ErrProcessUnresolved) {
		return p.reconcileUnanswered(ctx, ref)
	}
	if err != nil {
		return remote.Status{}, err
	}
	run, err := p.get(ctx, ref, runID)
	if err != nil {
		return remote.Status{}, err
	}
	return statusOf(run), nil
}

// reconcileUnanswered observes a Start whose response never supplied the
// permanent run id. It may recover an already-active request-bound resource,
// but it never replays creation: Status is used by the pre-tracker startup
// survey, where a request that takes effect could launch revoked work.
//
// Airlock's sandbox record is a second durable observation: active_run_id is
// the one non-terminal run, and ready/suspended with no active run positively
// means no unsafe execution domain remains *at that instant*. The active run is
// adopted only when its immutable label names this exact ProcessRef; an
// unrelated or absent run remains unresolved rather than becoming an
// attachment oracle or permission to dispatch.
func (p *Processes) reconcileUnanswered(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	rec, err := loadBoundSandbox(p.store, p.binding, ref.Identity.Claim)
	if err != nil {
		return remote.Status{}, err
	}
	sandbox, err := p.workspaces.get(ctx, rec.SandboxID)
	if err != nil {
		return remote.Status{}, p.workspaces.classify(rec, err)
	}
	if err := p.workspaces.checkPin(rec, ref.Identity.ProfileRevision, sandbox); err != nil {
		return remote.Status{}, err
	}

	if sandbox.ActiveRunID == nil || *sandbox.ActiveRunID == "" {
		// Absence is only a snapshot: the unanswered request could still own an
		// in-flight idempotency attempt and commit after this read. Status is an
		// observation used by startup before tracker authority has been checked,
		// so it may neither infer absence nor replay the creation request here.
		return remote.Status{}, fmt.Errorf("%w: %s has no request-bound active run", remote.ErrProcessUnresolved, ref)
	}

	run, err := p.get(ctx, ref, *sandbox.ActiveRunID)
	if err != nil {
		return remote.Status{}, err
	}
	if run.Labels[processRefLabel] != runKey(ref) {
		return remote.Status{}, fmt.Errorf("%w: active run %s does not name %s",
			remote.ErrProcessUnresolved, run.RunID, ref)
	}
	return p.adopt(ref, run)
}

// Events returns durable envelopes after the cursor, and returns an empty
// result only when the event stream is genuinely sealed.
//
// That last clause is the whole of this method. remote.Attempt reads an empty
// successful result as "the stream is sealed" and synthesizes an outcome from
// it, so a long poll that merely elapsed must not return empty — it loops. The
// seal is proven positively: the run is terminal *and* its highest sequence is
// at or below what the caller has already consumed. A terminal run with events
// still unread is not sealed, and a quiet running run is not sealed either.
func (p *Processes) Events(ctx context.Context, ref remote.ProcessRef, after remote.Cursor) ([]remote.Envelope, error) {
	runID, err := p.resolve(ref)
	if err != nil {
		return nil, err
	}
	for {
		page, err := p.page(ctx, ref, runID, int64(after))
		if err != nil {
			return nil, err
		}
		if len(page.Events) > 0 {
			return envelopesOf(page.Events)
		}
		run, err := p.get(ctx, ref, runID)
		if err != nil {
			return nil, err
		}
		if run.State.Terminal() && run.Events.LatestSeq <= int64(after) {
			return nil, nil
		}
		// A context that ended is checked after the drain test rather than
		// before it, so a shutdown racing the final page still delivers what
		// the backend had already sequenced.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (p *Processes) page(ctx context.Context, ref remote.ProcessRef, runID string, after int64) (runEventPage, error) {
	query := url.Values{}
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(eventPageLimit))
	query.Set("wait_seconds", strconv.Itoa(int(p.timeouts.PollWait/time.Second)))
	var page runEventPage
	err := p.client.do(ctx, request{
		method: "GET",
		path:   p.runPath(ref, runID) + "/events",
		query:  query,
		long:   true,
		out:    &page,
	})
	switch {
	case hasCode(err, CodeCursorTooOld):
		// Retention expired under BEN's cursor: the provider output in that
		// range is gone, and nothing read afterwards may be translated into
		// success. Whether it is *recordable* is the distinction this makes —
		// an expiry that names the cursor and the retention floor is a range
		// the daemon may durably accept as data loss and fail the attempt over
		// (remote.RetentionGap); one that names neither leaves BEN unable to
		// say what it would be skipping, which is what remote.ErrEventGap
		// refuses outright.
		if gap := retentionGapOf(err, after); gap != nil {
			return runEventPage{}, fmt.Errorf("%s: %w", ref, gap)
		}
		return runEventPage{}, fmt.Errorf("%w: %s: %w", remote.ErrEventGap, ref, err)
	case hasCode(err, CodeCursorAhead):
		// The contract's own reading: a cursor past the run's log means the
		// client is holding a cursor from a *different* run. That is an address
		// error, not a gap, and it must park rather than resume from anywhere.
		return runEventPage{}, fmt.Errorf("%w: %s: %w", remote.ErrProcessMismatch, ref, err)
	case hasCode(err, CodeNotFound):
		return runEventPage{}, fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
	case hasCode(err, CodeForbidden):
		return runEventPage{}, fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
	case err != nil:
		return runEventPage{}, err
	}
	return page, nil
}

// retentionGapOf reads a cursor_too_old refusal as a measured range, or reports
// that it cannot be read as one.
//
// Four conditions, and each is a way the answer would not be evidence about
// this request. The status must be the contract's 409 conflict; the same code
// on a server failure is not an authoritative retention statement. Both
// numbers must be present, because a range needs two ends. requested_after must
// be the cursor this call actually sent, because an expiry stated about some
// other position says nothing about the hole under *this* cursor — and a stale
// or cross-run answer is exactly what a client polling one log with a cursor
// from another would receive. And the pair must describe at least one missing
// sequence: a floor at or below the next sequence BEN would have read is a
// contradiction rather than a loss.
func retentionGapOf(err error, after int64) *remote.RetentionGap {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict ||
		apiErr.RequestedAfter == nil || apiErr.OldestAvailableSeq == nil {
		return nil
	}
	if *apiErr.RequestedAfter != after {
		return nil
	}
	gap := &remote.RetentionGap{
		RequestedAfter:  *apiErr.RequestedAfter,
		OldestAvailable: *apiErr.OldestAvailableSeq,
		Cause:           err,
	}
	if _, ok := gap.Range(); !ok {
		return nil
	}
	return gap
}

// envelopesOf translates one page into the boundary's opaque envelopes.
//
// Output chunks become stdout/stderr envelopes carrying raw bytes; every other
// kind becomes a control envelope carrying the event as Airlock framed it. The
// distinction is load-bearing in one direction only: remote.Attempt frames
// stdout into lines and hands each to the provider adapter's translator, and
// never hands it a control envelope — so a run.terminal event is *retained* in
// the transcript and durable consumer without ever being parsed as if it were a
// provider record. BEN's outcome comes from the provider's own stream, which is
// what keeps Airlock unable to author one.
//
// An envelope that reached this function came from a successfully decoded event
// page. Its position is durable and cursor-addressed, so malformed envelope data
// cannot become readable on a retry. Classify it as ErrEventGap: that is the
// stream refusal for evidence BEN cannot admit, while transport and whole-page
// decode failures remain reconnectable above this boundary (#275).
func envelopesOf(raw []json.RawMessage) ([]remote.Envelope, error) {
	out := make([]remote.Envelope, 0, len(raw))
	for _, item := range raw {
		var ev runEvent
		if err := json.Unmarshal(item, &ev); err != nil {
			return nil, fmt.Errorf("%w: airlock: decoding a run event: %w", remote.ErrEventGap, err)
		}
		if ev.Seq <= 0 {
			return nil, fmt.Errorf("%w: an event carries sequence %d (a backend log starts at 1)", remote.ErrEventGap, ev.Seq)
		}
		envelope := remote.Envelope{Seq: ev.Seq, Stream: remote.StreamControl, Payload: append([]byte(nil), item...)}
		if ev.Kind == EventOutputTruncated {
			envelope.Truncated = true
		}
		if ev.Kind == EventOutput {
			data, err := base64.StdEncoding.DecodeString(ev.DataB64)
			if err != nil {
				return nil, fmt.Errorf("%w: airlock: decoding output at sequence %d: %w", remote.ErrEventGap, ev.Seq, err)
			}
			envelope.Payload = data
			switch ev.Stream {
			case StreamStdout:
				envelope.Stream = remote.StreamStdout
			case StreamStderr:
				envelope.Stream = remote.StreamStderr
			default:
				return nil, fmt.Errorf("%w: airlock: output at sequence %d names stream %q", remote.ErrEventGap, ev.Seq, ev.Stream)
			}
		}
		out = append(out, envelope)
	}
	return out, nil
}

// Stdin appends bytes to a streaming run's stdin.
//
// Offset-addressed rather than key-addressed, per the contract: an exact resend
// of the same chunk at the same offset is a durable no-op that returns the
// recorded next_offset, even after the run has become terminal. That is why the
// offset is tracked from the *response* rather than by counting bytes locally —
// the server's number is the receipt.
func (p *Processes) Stdin(ctx context.Context, ref remote.ProcessRef, data []byte) error {
	runID, err := p.resolve(ref)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	p.mu.Lock()
	offset := p.stdinOffsets[runID]
	p.mu.Unlock()

	var resp writeStdinResponse
	err = p.client.do(ctx, request{
		method: "POST",
		path:   p.runPath(ref, runID) + "/stdin",
		body:   writeStdinRequest{Offset: offset, DataB64: base64.StdEncoding.EncodeToString(data)},
		out:    &resp,
	})
	if hasCode(err, CodeNotFound) {
		return fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
	}
	if hasCode(err, CodeForbidden) {
		return fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.stdinOffsets[runID] = resp.NextOffset
	p.mu.Unlock()
	return nil
}

// Stop performs the mode-specific backend ladder and returns fresh evidence.
//
// The two modes stay different all the way down, which is the rule #192 states:
// Interrupt asks for the patient TERM/grace/KILL ladder so an agent winding up
// can still report its outcome, and Discard asks for immediate termination.
// Neither *is* termination — a 202 acknowledges the request and says nothing
// about the process — so the status is read back from the run afterwards, and
// its three evidence fields are the only answer returned. The run's durable
// principal binding is checked before signalRun because this key is reused by
// later ticks and restarts, beyond one client's retry lifetime.
func (p *Processes) Stop(ctx context.Context, ref remote.ProcessRef, req remote.StopRequest) (remote.Status, error) {
	binding, err := p.resolveBinding(ref)
	if err != nil {
		return remote.Status{}, err
	}
	runID := binding.RunID
	body := signalRequest{Signal: SignalKILL}
	if req.Mode == core.StopInterrupt {
		// Grace is only valid with TERM, and the contract resolves an omitted or
		// null grace to exactly 30 seconds. BEN's own grace is sent when it has
		// one so the two ladders agree about the window.
		body = signalRequest{Signal: SignalTERM, GraceSeconds: seconds(req.Grace)}
	}
	auth, err := p.client.keyedAuth(ctx)
	if err != nil {
		return remote.Status{}, err
	}
	if err := requirePrincipalBinding(binding.PrincipalBinding, auth.principalBinding, ref.String()); err != nil {
		return remote.Status{}, err
	}
	var signalled signalResponse
	err = p.client.do(ctx, request{
		method:    "POST",
		path:      p.runPath(ref, runID) + "/signal",
		idem:      signalKey(runID, body.Signal),
		body:      body,
		authToken: auth.token,
		out:       &signalled,
	})
	if hasCode(err, CodeNotFound) {
		return remote.Status{}, fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
	}
	if hasCode(err, CodeForbidden) {
		return remote.Status{}, fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
	}
	// A refused signal is not a reason to stop asking what the run is doing.
	// `signal_limit_exceeded` is non-retryable and is exactly the case where the
	// operator most needs the evidence, and a terminal run answers 200 with a
	// null signal id, which is a completed no-op rather than a failure.
	if err != nil && !hasCode(err, CodeSignalLimitExceeded) {
		return remote.Status{}, err
	}
	run, statusErr := p.get(ctx, ref, runID)
	if statusErr != nil {
		return remote.Status{}, errors.Join(err, statusErr)
	}
	return statusOf(run), err
}

// Wait blocks until the direct process is reaped or ctx ends.
//
// Terminal state, not domain quiet: the contract makes descendants a separate
// observation and remote.Status.Reaped is the narrower fact core.RunHandle.Done
// represents. Waiting has no effect on the run — a caller that stops waiting,
// crashes or disconnects changes nothing, which is what makes this safe to
// abandon at shutdown.
func (p *Processes) Wait(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	runID, err := p.resolve(ref)
	if err != nil {
		return remote.Status{}, err
	}
	for {
		var run Run
		err := p.client.do(ctx, request{
			method: "POST",
			path:   p.runPath(ref, runID) + "/wait",
			body:   waitForRunRequest{WaitSeconds: int(p.timeouts.PollWait / time.Second)},
			long:   true,
			out:    &run,
		})
		if hasCode(err, CodeNotFound) {
			return remote.Status{}, fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
		}
		if hasCode(err, CodeForbidden) {
			return remote.Status{}, fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
		}
		if err != nil {
			return remote.Status{}, err
		}
		if run.State.Terminal() {
			return statusOf(run), nil
		}
		// Elapsing is not an error by contract; the caller distinguishes the two
		// by reading state, which is what this loop does.
		if err := ctx.Err(); err != nil {
			return remote.Status{}, err
		}
	}
}

func (p *Processes) get(ctx context.Context, ref remote.ProcessRef, runID string) (Run, error) {
	var run Run
	err := p.client.do(ctx, request{method: "GET", path: p.runPath(ref, runID), out: &run})
	switch {
	case hasCode(err, CodeNotFound):
		return Run{}, fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
	case hasCode(err, CodeForbidden):
		return Run{}, fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
	case err != nil:
		return Run{}, err
	}
	if run.RunID != "" && run.RunID != runID {
		return Run{}, fmt.Errorf("%w: asked for %s and got %s", ErrUnexpectedRun, runID, run.RunID)
	}
	return run, nil
}

func (p *Processes) runPath(ref remote.ProcessRef, runID string) string {
	return "/v2/sandboxes/" + url.PathEscape(ref.Identity.SandboxID) + "/runs/" + url.PathEscape(runID)
}

// resolve turns BEN's dispatch address into Airlock's run id.
//
// The binding store distinguishes all three absence outcomes at this seam. No
// record means Start never crossed its write-ahead fence (ErrNoProcess); an
// empty record is an unanswered Start (ErrProcessUnresolved); a recorded id is
// the permanent handle whose later 404 is ErrProcessUnavailable. Only the first
// is a confirmed absence.
func (p *Processes) resolve(ref remote.ProcessRef) (string, error) {
	binding, err := p.resolveBinding(ref)
	if err != nil {
		return "", err
	}
	return binding.RunID, nil
}

// resolveBinding retains the principal fence beside the permanent run handle.
// Most operations need only RunID; keyed signals also need the principal that
// created it because their key is reused by later Stop calls and processes.
func (p *Processes) resolveBinding(ref remote.ProcessRef) (StartBinding, error) {
	if !ref.Complete() {
		return StartBinding{}, fmt.Errorf("%w: incomplete process reference", remote.ErrProcessMismatch)
	}
	if _, err := loadBoundSandbox(p.store, p.binding, ref.Identity.Claim); err != nil {
		return StartBinding{}, err
	}
	binding, err := p.store.LoadBinding(ref.String())
	if errors.Is(err, ErrNoRunBinding) {
		// Start writes this store's binding before its first backend request. No
		// binding therefore proves that Start never crossed the acceptance fence.
		return StartBinding{}, fmt.Errorf("%w: %s", remote.ErrNoProcess, ref)
	}
	if err != nil {
		return StartBinding{}, err
	}
	if err := requireSubstrateBinding(binding.Substrate, p.binding, ref.String()); err != nil {
		return StartBinding{}, err
	}
	if binding.RunID == "" {
		if binding.Refusal != nil {
			// Answered, and the answer was no: the backend refused the body
			// before creating anything, so this address names no process and
			// never will until a different body is offered under it.
			return StartBinding{}, refusalError(ref, *binding.Refusal)
		}
		return StartBinding{}, fmt.Errorf("%w: %s has an unanswered start", remote.ErrProcessUnresolved, ref)
	}
	return binding, nil
}

// runLabels are the opaque labels attached to an Airlock run. The claim cycle
// and the branch, exactly as on the sandbox: identifiers the tracker publishes,
// and nothing an issue author wrote.
func (p *Processes) runLabels(ref remote.ProcessRef, scope remote.GitScope) map[string]string {
	labels := map[string]string{}
	for k, v := range p.labels {
		labels[k] = v
	}
	// Set BEN's identity labels after operator labels so configuration cannot
	// replace the proof reconcileUnanswered relies on.
	labels["ben.claim"] = ref.Identity.Claim.String()
	labels["ben.branch"] = ref.Identity.Branch
	labels[processRefLabel] = runKey(ref)
	addGitLabels(labels, scope)
	return labels
}

// processRefLabel carries the same one-way identity as the idempotency key:
// sandbox, BEN run id and canonical request digest. It contains no prompt,
// environment value or credential and fits Airlock's opaque-label contract.
const processRefLabel = "ben.process"

// statusOf is the whole evidence translation, and it is a pure function of one
// Run so that a test can hold every combination to it.
//
// Three facts in, three facts out, and nothing derived across them. Airlock's
// contract and #192's are the same contract here: a sealed stream does not imply
// a reaped process, a reaped process does not imply a quiet execution domain,
// and none of the three is implied by a signal having been delivered. A run in
// `lost` reports domain quiet `unknown` by definition, so it maps to unconfirmed
// and remote.MayReuse refuses to touch its workspace.
func statusOf(run Run) remote.Status {
	st := remote.Status{
		Phase:        phaseOf(run),
		Stream:       streamStateOf(run.Termination.StreamSealed),
		Process:      processStateOf(run.Termination.ProcessReaped),
		Domain:       domainStateOf(run.Termination.DomainQuiet),
		BackendRunID: run.RunID,
		// This value came from a response, so the backend was reachable. The
		// zero Status is what an unreachable one produces, and it reports false.
		Reachable: true,
	}
	return st
}

// phaseOf is diagnostic only — it is never lifecycle evidence, and PhaseQuiet
// is deliberately reserved for a run whose execution domain Airlock has
// positively observed to be quiet. A terminal run without that observation
// reports PhaseUnknown, which is what a refusal message should say about it.
func phaseOf(run Run) Phase {
	switch run.State {
	case RunQueued, RunAccepted:
		return remote.PhaseStarting
	case RunRunning:
		return remote.PhaseRunning
	case RunTerminating:
		return remote.PhaseSignaled
	case RunExited, RunFailed, RunLost:
		if run.Termination.DomainQuiet.Confirmed() {
			return remote.PhaseQuiet
		}
		return remote.PhaseUnknown
	}
	return remote.PhaseUnknown
}

// Phase is remote.Phase, aliased so the mapping above reads as one vocabulary
// rather than as a package-qualified translation at every arm.
type Phase = remote.Phase

func streamStateOf(e Evidence) remote.StreamState {
	switch e {
	case EvidenceConfirmed:
		return remote.StreamStateSealed
	case EvidenceNotConfirmed:
		return remote.StreamStateOpen
	}
	return remote.StreamStateUnknown
}

func processStateOf(e Evidence) remote.ProcessState {
	switch e {
	case EvidenceConfirmed:
		return remote.ProcessStateReaped
	case EvidenceNotConfirmed:
		return remote.ProcessStateRunning
	}
	return remote.ProcessStateUnknown
}

func domainStateOf(e Evidence) remote.DomainState {
	switch e {
	case EvidenceConfirmed:
		return remote.DomainStateQuiet
	case EvidenceNotConfirmed:
		return remote.DomainStateActive
	}
	return remote.DomainStateUnknown
}
