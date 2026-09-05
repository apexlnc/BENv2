package airlock

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// How a prompt reaches a run (#284).
//
// Airlock offers two stdin paths and bounds each from the profile: `inline`
// carries the bytes in the start request itself, under
// `max_stdin_inline_bytes`; `streaming` creates the run with stdin open and
// accepts offset-addressed writes afterwards, each under
// `max_stdin_chunk_bytes` and all of them under `max_stdin_total_bytes`. Over
// both sits `max_request_body_bytes`, enforced on the whole request before it
// is parsed — and an inline prompt is base64 inside JSON, so a body can exceed
// that while the decoded prompt is under the inline bound. The deployed
// reviewer profile admits 64 KiB inline, and a reviewer prompt for a 64 KB
// diff is larger than the diff — so a client that only knew the first path
// was refused with `413 payload_too_large` on every sweep and read the
// refusal as an unanswered start.
//
// This file is the second path and the rule for choosing between them. The
// choice is a function of the prompt's length and the envelope of the
// revision the sandbox is pinned to — recorded on the sandbox, never read off
// the profile's current revision — and of nothing else, so the same address
// composes the same start body on every replay, which is what the idempotency
// key requires.

// defaultStdinChunk bounds one streaming write when the profile did not say —
// which a published profile cannot do, since the schema gives the limit a
// minimum of one. Stated so a zero can never become a loop of empty writes.
const defaultStdinChunk = 64 << 10

// minStdinChunk is the floor a write shrinks to when the backend refuses one as
// too large without naming a smaller per-write bound. Below it the refusal is
// not about the chunk.
const minStdinChunk = 512

// stdinWriteOverhead is the JSON framing of one streaming write around an
// empty payload, with the offset at its widest — measured from the request
// type rather than assumed, so a renamed field cannot silently shift it.
var stdinWriteOverhead = func() int64 {
	encoded, err := json.Marshal(writeStdinRequest{Offset: math.MaxInt64, Close: true})
	if err != nil {
		panic(err) // a fixed struct of two ints, a string and a bool always encodes
	}
	return int64(len(encoded))
}()

// stdinPlan is how one prompt travels: the mode the start body names, the
// chunk streaming writes use, or the refusal that stands in for both when no
// path can deliver it.
type stdinPlan struct {
	mode    StdinMode
	chunk   int64
	refusal *StartRefusal
}

// planStdin chooses the path for a prompt under a pinned envelope.
//
// Inline while the decoded prompt fits the inline bound, because it is one
// request and the run starts with its input already in hand; streaming above
// it. Whether the *encoded* inline body fits the request-body bound is the
// caller's to check with the body in hand (Processes.Start), and it streams
// when it does not. A prompt over the total bound has no path, and that is
// answered as the refusal the backend would return rather than as a request
// the backend would refuse — the same recorded fact, without the round trip.
//
// Unknown limits — no profile read has yet matched the sandbox's pinned
// revision — plan inline, which is what this client always sent. The backend
// judges it, and a refusal is now a definite answer rather than an unresolved
// start.
func planStdin(limits StdinLimits, stdin []byte) stdinPlan {
	n := int64(len(stdin))
	if n == 0 {
		return stdinPlan{mode: StdinClosed}
	}
	if !limits.Known() {
		return stdinPlan{mode: StdinInline}
	}
	if !limits.Admits(n) {
		return stdinPlan{mode: StdinStreaming, refusal: &StartRefusal{
			Code:       string(CodePayloadTooLarge),
			Message:    fmt.Sprintf("stdin of %d bytes exceeds the profile's max_stdin_total_bytes", n),
			LimitBytes: limits.Total,
		}}
	}
	if n <= limits.Inline {
		return stdinPlan{mode: StdinInline}
	}
	return stdinPlan{mode: StdinStreaming, chunk: limits.streamChunk()}
}

// streamChunk is the largest write the envelope admits: the per-write bound,
// cut further so that the encoded write — base64 plus its JSON framing — fits
// the request-body bound the server measures first.
func (l StdinLimits) streamChunk() int64 {
	chunk := l.Chunk
	if chunk <= 0 {
		chunk = defaultStdinChunk
	}
	if fit := l.bodyFit(); fit > 0 && fit < chunk {
		chunk = fit
	}
	return chunk
}

// bodyFit is the most decoded stdin one write request may carry under the
// request-body bound, rounded down to whole base64 groups, or zero when the
// bound is unknown.
func (l StdinLimits) bodyFit() int64 {
	if l.RequestBody <= 0 {
		return 0
	}
	room := l.RequestBody - stdinWriteOverhead
	if room < 4 {
		return 3
	}
	return room / 4 * 3
}

// request is the start body's stdin field for this plan.
func (plan stdinPlan) request(stdin []byte) *runStdinRequest {
	switch plan.mode {
	case StdinInline:
		return &runStdinRequest{Mode: StdinInline, InlineB64: base64.StdEncoding.EncodeToString(stdin)}
	case StdinStreaming:
		return &runStdinRequest{Mode: StdinStreaming}
	}
	return &runStdinRequest{Mode: StdinClosed}
}

// fingerprintOf digests the exact encoded start body — the same bytes
// client.attempt puts on the wire, so it identifies what the server saw.
func fingerprintOf(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// refusalError restates a recorded refusal in the boundary's vocabulary. The
// cause names the address and when the answer was given, and carries nothing
// from the body it refused.
func refusalError(ref remote.ProcessRef, r StartRefusal) error {
	return &remote.ProcessRefusal{
		Code: r.Code, Message: r.Message, LimitBytes: r.LimitBytes,
		Cause: fmt.Errorf("airlock: %s: the backend refused this exact start body at %s",
			ref, r.RefusedAt.UTC().Format(time.RFC3339)),
	}
}

// completeStdin delivers the whole prompt to a streaming run, closes its stdin
// and clears the owed mark — in that order, so the mark outlives any crash
// that leaves bytes undelivered.
func (p *Processes) completeStdin(ctx context.Context, ref remote.ProcessRef, runID string, data []byte, chunk int64) error {
	if err := p.deliverStdin(ctx, ref, runID, data, chunk); err != nil {
		return err
	}
	return p.store.SetStdinPending(ref.String(), p.binding, false)
}

// deliverStdin writes the prompt from offset zero in chunks and closes with the
// last one, following the server's receipts rather than its own count.
//
// Starting at zero is what makes a resume safe without a durable offset: an
// exact resend at a receipted offset is a no-op that returns the recorded
// next_offset, so a process picking up after a crash walks the already
// delivered prefix at one request per chunk and appends from where it ends. A
// receipted offset holding *different* bytes — chunking that changed under a
// pinned envelope, which the pin makes unreachable — is answered with
// expected_offset, and the writer resumes from there because the bytes behind
// it are this same prompt.
//
// Two answers end delivery without completing it, and both are the run's
// evidence to report rather than this writer's: `run_not_accepting_stdin` (the
// run is terminating, terminal, or its stdin was already closed by an earlier
// process) and `stdin_delivery_outcome_unknown` (the compute generation that
// held the pending write is gone). Neither is a reason to hold the start.
func (p *Processes) deliverStdin(ctx context.Context, ref remote.ProcessRef, runID string, data []byte, chunk int64) error {
	if chunk <= 0 {
		chunk = defaultStdinChunk
	}
	total := int64(len(data))
	deadline := time.Now().Add(p.timeouts.Settle)
	var offset int64
	for {
		end := min(offset+chunk, total)
		closing := end == total
		var resp writeStdinResponse
		err := p.client.do(ctx, request{
			method: "POST",
			path:   p.runPath(ref, runID) + "/stdin",
			body: writeStdinRequest{
				Offset: offset, DataB64: base64.StdEncoding.EncodeToString(data[offset:end]), Close: closing,
			},
			out: &resp,
		})
		var apiErr *APIError
		switch {
		case err == nil:
			if resp.Closed {
				return nil
			}
			if resp.NextOffset <= offset || resp.NextOffset > total {
				return fmt.Errorf("%w: %s receipted stdin offset %d after a write at %d of %d bytes",
					ErrUnexpectedRun, ref, resp.NextOffset, offset, total)
			}
			offset = resp.NextOffset
		case hasCode(err, CodeRunNotReadyForStdin):
			// Queued: the runner has not acknowledged the dispatch. The client's
			// own retries have already absorbed a blip; this waits out a slow
			// acceptance under the same bound a settling sandbox gets.
			if time.Now().After(deadline) {
				return fmt.Errorf("airlock: %s was not ready for stdin within %s: %w", ref, p.timeouts.Settle, err)
			}
			if err := p.client.sleep(ctx, settlePollInterval); err != nil {
				return err
			}
		case hasCode(err, CodeStdinOffsetMismatch):
			_ = errors.As(err, &apiErr)
			if apiErr.ExpectedOffset != nil && *apiErr.ExpectedOffset > offset && *apiErr.ExpectedOffset <= total {
				offset = *apiErr.ExpectedOffset
				continue
			}
			return fmt.Errorf("%w: %s: %w", ErrUnexpectedRun, ref, err)
		case hasCode(err, CodePayloadTooLarge):
			// The write was too large for a bound the envelope did not carry, or
			// carried differently: re-chunk to the per-write bound the server named
			// when it named one under the chunk, otherwise halve, down to a floor
			// below which the refusal is not about this write.
			_ = errors.As(err, &apiErr)
			next := chunk / 2
			if apiErr.LimitBytes > 0 && apiErr.LimitBytes < chunk {
				next = apiErr.LimitBytes
			}
			if next < minStdinChunk {
				return fmt.Errorf("airlock: %s: %w", ref, err)
			}
			chunk = next
		case hasCode(err, CodeRunNotAcceptingStdin, CodeStdinOutcomeUnknown):
			return nil
		case hasCode(err, CodeNotFound):
			return fmt.Errorf("%w: %s: %w", remote.ErrProcessUnavailable, ref, err)
		case hasCode(err, CodeForbidden):
			return fmt.Errorf("%w: %s: %w", ErrNotOwned, ref.Identity.SandboxID, err)
		default:
			return err
		}
	}
}
