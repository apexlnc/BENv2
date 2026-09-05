package airlocktest

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Route templates. A fault or a dropped response is queued against one of
// these, so a test names "the second startRun" rather than a concrete path
// containing identifiers it did not choose.
const (
	RouteGetProfile     = "GET /v2/profiles/{profile_id}"
	RouteCreateSandbox  = "POST /v2/sandboxes"
	RouteGetSandbox     = "GET /v2/sandboxes/{sandbox_id}"
	RouteDeleteSandbox  = "DELETE /v2/sandboxes/{sandbox_id}"
	RouteSuspendSandbox = "POST /v2/sandboxes/{sandbox_id}/suspend"
	RouteResumeSandbox  = "POST /v2/sandboxes/{sandbox_id}/resume"
	RouteStartRun       = "POST /v2/sandboxes/{sandbox_id}/runs"
	RouteGetRun         = "GET /v2/sandboxes/{sandbox_id}/runs/{run_id}"
	RouteGetRunEvents   = "GET /v2/sandboxes/{sandbox_id}/runs/{run_id}/events"
	RouteWriteStdin     = "POST /v2/sandboxes/{sandbox_id}/runs/{run_id}/stdin"
	RouteSignalRun      = "POST /v2/sandboxes/{sandbox_id}/runs/{run_id}/signal"
	RouteWaitForRun     = "POST /v2/sandboxes/{sandbox_id}/runs/{run_id}/wait"
)

// resolved is one request reduced to a route template plus its path parameters.
type resolved struct {
	route     string
	sandboxID string
	runID     string
	profileID string
}

func resolve(method, path string) (resolved, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 || segs[0] != "v2" {
		return resolved{}, false
	}
	switch {
	case segs[1] == "profiles" && len(segs) == 3 && method == "GET":
		return resolved{route: RouteGetProfile, profileID: segs[2]}, true
	case segs[1] == "sandboxes" && len(segs) == 2 && method == "POST":
		return resolved{route: RouteCreateSandbox}, true
	case segs[1] != "sandboxes":
		return resolved{}, false
	}
	if len(segs) < 3 {
		return resolved{}, false
	}
	r := resolved{sandboxID: segs[2]}
	tail := segs[3:]
	switch {
	case len(tail) == 0 && method == "GET":
		r.route = RouteGetSandbox
	case len(tail) == 0 && method == "DELETE":
		r.route = RouteDeleteSandbox
	case len(tail) == 1 && tail[0] == "suspend" && method == "POST":
		r.route = RouteSuspendSandbox
	case len(tail) == 1 && tail[0] == "resume" && method == "POST":
		r.route = RouteResumeSandbox
	case len(tail) == 1 && tail[0] == "runs" && method == "POST":
		r.route = RouteStartRun
	case len(tail) == 2 && tail[0] == "runs" && method == "GET":
		r.route, r.runID = RouteGetRun, tail[1]
	case len(tail) == 3 && tail[0] == "runs" && tail[2] == "events" && method == "GET":
		r.route, r.runID = RouteGetRunEvents, tail[1]
	case len(tail) == 3 && tail[0] == "runs" && tail[2] == "stdin" && method == "POST":
		r.route, r.runID = RouteWriteStdin, tail[1]
	case len(tail) == 3 && tail[0] == "runs" && tail[2] == "signal" && method == "POST":
		r.route, r.runID = RouteSignalRun, tail[1]
	case len(tail) == 3 && tail[0] == "runs" && tail[2] == "wait" && method == "POST":
		r.route, r.runID = RouteWaitForRun, tail[1]
	default:
		return resolved{}, false
	}
	return r, true
}

func (s *Server) serve(w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)
	if err != nil {
		s.mu.Lock()
		s.errorf(w, badRequest("invalid_request", "unreadable body", nil))
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	if s.partitioned {
		s.mu.Unlock()
		// An abandoned connection: the client sees a transport error, which is
		// what an unreachable control plane is. Deliberately not a 503 — a
		// refusal is an answer, and this case is the absence of one.
		panic(http.ErrAbortHandler)
	}
	s.requests = append(s.requests, Request{
		Method: req.Method, Path: req.URL.Path,
		Key:  req.Header.Get("Idempotency-Key"),
		Auth: req.Header.Get("Authorization"),
		Body: append([]byte(nil), body...),
	})
	auth := req.Header.Get("Authorization")
	token := s.token
	s.mu.Unlock()

	if auth != "Bearer "+token {
		s.mu.Lock()
		s.errorf(w, fault{Status: 401, Code: "unauthenticated", Message: "missing or invalid access token"})
		s.mu.Unlock()
		return
	}

	route, ok := resolve(req.Method, req.URL.Path)
	if !ok {
		s.mu.Lock()
		s.errorf(w, notFound("no such route"))
		s.mu.Unlock()
		return
	}

	// The events long poll is the one route that must not hold the lock, so it
	// is dispatched before the locked section below.
	if route.route == RouteGetRunEvents {
		s.serveEvents(w, req, route)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.takeFaultLocked(route.route); ok {
		s.errorf(w, f)
		return
	}
	switch route.route {
	case RouteGetProfile:
		s.getProfile(w, route)
	case RouteCreateSandbox:
		s.createSandbox(w, req, route, body)
	case RouteGetSandbox:
		s.getSandbox(w, route)
	case RouteDeleteSandbox:
		s.deleteSandbox(w, route)
	case RouteSuspendSandbox:
		s.suspendSandbox(w, route)
	case RouteResumeSandbox:
		s.resumeSandbox(w, route)
	case RouteStartRun:
		s.startRun(w, req, route, body)
	case RouteGetRun:
		s.getRun(w, route)
	case RouteWriteStdin:
		s.writeStdin(w, route, body)
	case RouteSignalRun:
		s.signalRun(w, req, route, body)
	case RouteWaitForRun:
		s.waitForRun(w, route)
	}
}

func (s *Server) takeFaultLocked(route string) (fault, bool) {
	queued := s.faults[route]
	if len(queued) == 0 {
		return fault{}, false
	}
	s.faults[route] = queued[1:]
	return queued[0], true
}

// dropLocked reports whether this response must be abandoned. Called *after*
// the side effect has committed, which is the whole point: the resource exists
// and the client has no way to learn its identifier except by replaying its key.
func (s *Server) dropLocked(route string) bool {
	if s.drop[route] <= 0 {
		return false
	}
	s.drop[route]--
	return true
}

func (s *Server) getProfile(w http.ResponseWriter, r resolved) {
	p, ok := s.profiles[r.profileID]
	if !ok {
		s.errorf(w, notFound("no such profile"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"profile_id":       r.profileID,
		"profile_revision": p.Revision,
		"display_name":     r.profileID,
		"status":           p.Status,
		"limits": map[string]any{
			"max_concurrent_runs":    1,
			"max_stdin_inline_bytes": p.Limits.Inline,
			"max_stdin_chunk_bytes":  p.Limits.Chunk,
			"max_stdin_total_bytes":  p.Limits.Total,
			"max_request_body_bytes": p.Limits.RequestBody,
		},
		"defaults":          map[string]any{"cwd": "/workspace"},
		"persistence":       []any{},
		"credential_grants": []any{},
		"reserved_env_keys": []any{},
		"audit_full_argv":   false,
	})
}

// keyed applies the contract's idempotency rules to a keyed route.
//
// It returns the key's scope, the resource a completed key already produced,
// and whether this is a replay; `ok` is false when it has already written the
// refusal itself. The scope is tenant plus subject plus method, resolved path
// and key. client_id is deliberately absent: it is audit metadata, and OAuth
// client rotation does not change ownership.
func (s *Server) keyed(
	w http.ResponseWriter, req *http.Request, path string, body []byte,
) (scope idemScope, resource string, replayed, ok bool) {
	key := req.Header.Get("Idempotency-Key")
	if key == "" {
		s.errorf(w, badRequest("idempotency_key_required", "this route requires an Idempotency-Key", nil))
		return idemScope{}, "", false, false
	}
	if !validKey(key) {
		s.errorf(w, badRequest("invalid_request", "Idempotency-Key does not match the contract's pattern", nil))
		return idemScope{}, "", false, false
	}
	scope = idemScope{
		TenantID: s.owner.TenantID, Subject: s.owner.Subject,
		Method: req.Method, Path: path, Key: key,
	}
	rec, known := s.idem[scope]
	if !known {
		return scope, "", false, true
	}
	if rec.Fingerprint != fingerprint(body) {
		s.errorf(w, conflict("idempotency_key_conflict", "this key was first used with a different payload",
			map[string]any{"first_seen_at": "2026-01-01T00:00:00.000Z"}))
		return idemScope{}, "", false, false
	}
	if rec.Failure != nil {
		// A completed non-retryable failure replays its stored outcome with a
		// fresh request id, and never retries the work.
		s.errorf(w, *rec.Failure)
		return idemScope{}, "", false, false
	}
	return scope, rec.Resource, true, true
}

// validKey enforces `^[A-Za-z0-9_.:-]{16,255}$`.
func validKey(key string) bool {
	if len(key) < 16 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) createSandbox(w http.ResponseWriter, req *http.Request, r resolved, body []byte) {
	scope, resource, replayed, ok := s.keyed(w, req, req.URL.Path, body)
	if !ok {
		return
	}
	if replayed {
		sb, live := s.sandboxes[resource]
		if !live {
			// The target tombstone expired before the key did: the completed key
			// keeps a redacted snapshot while getSandbox stays 404.
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, 200, map[string]any{
				"sandbox_id": resource, "state": "deleted", "owner": s.owner,
			})
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, 200, s.sandboxJSON(sb))
		return
	}

	var request struct {
		ProfileID string            `json:"profile_id"`
		Labels    map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.ProfileID == "" {
		s.errorf(w, badRequest("invalid_request", "profile_id is required", nil))
		return
	}
	p, known := s.profiles[request.ProfileID]
	if !known || p.Status == "withdrawn" {
		// Every unusable profile is the same 404, deliberately: anything else
		// turns a supplied identifier into an existence oracle.
		s.errorf(w, notFound("no such profile"))
		return
	}
	sb := &sandboxRec{
		ID: s.nextID("sbx"), Owner: s.owner, State: "ready",
		ProfileID: request.ProfileID, Revision: p.Revision, Limits: p.Limits,
		WorkspaceID: s.nextID("wsp"), Labels: request.Labels,
	}
	// The record and its idempotency result commit together, before the response
	// is written. That ordering is what makes a dropped response recoverable by
	// replaying the key instead of by minting a new one.
	s.sandboxes[sb.ID] = sb
	s.idem[scope] = &idemRecord{Fingerprint: fingerprint(body), Resource: sb.ID}
	if s.dropLocked(RouteCreateSandbox) {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Idempotency-Replayed", "false")
	writeJSON(w, 201, s.sandboxJSON(sb))
}

func (s *Server) lookupSandbox(w http.ResponseWriter, id string) (*sandboxRec, bool) {
	sb, ok := s.sandboxes[id]
	if !ok {
		s.errorf(w, notFound("no such sandbox"))
		return nil, false
	}
	// Ownership before state. A different tenant cannot observe existence at
	// all; a different subject inside the owning tenant gets 403. client_id is
	// audit metadata, not ownership: rotating the OAuth client while tenant and
	// subject stay fixed must retain access.
	switch {
	case sb.Owner.TenantID != s.owner.TenantID:
		s.errorf(w, notFound("no such sandbox"))
		return nil, false
	case sb.Owner.Subject != s.owner.Subject:
		s.errorf(w, fault{Status: 403, Code: "forbidden", Message: "not the owner of this sandbox"})
		return nil, false
	}
	return sb, true
}

func (s *Server) getSandbox(w http.ResponseWriter, r resolved) {
	sb, ok := s.lookupSandbox(w, r.sandboxID)
	if !ok {
		return
	}
	writeJSON(w, 200, s.sandboxJSON(sb))
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r resolved) {
	sb, ok := s.lookupSandbox(w, r.sandboxID)
	if !ok {
		return
	}
	if sb.Deletion == nil {
		if p := s.pendingDeletion; p != nil {
			// Scripted: the volume is on its way out and the evidence has not all
			// arrived. `deleting` rather than `deleted`, which is what the contract
			// says a 202 means.
			sb.Deletion = &deletion{Compute: p.Compute, Volume: p.Volume, Tombstone: p.Tombstone}
			sb.State = "deleting"
		} else {
			sb.Deletion = &deletion{Compute: "confirmed", Volume: "confirmed", Tombstone: "confirmed"}
			sb.State = "deleted"
		}
		if sb.ActiveRun != "" {
			if r, live := s.runs[sb.ActiveRun]; live {
				r.State, r.Reason = "lost", "sandbox_deleted"
			}
			sb.ActiveRun = ""
		}
	}
	writeJSON(w, 202, s.sandboxJSON(sb))
}

func (s *Server) suspendSandbox(w http.ResponseWriter, r resolved) {
	sb, ok := s.lookupSandbox(w, r.sandboxID)
	if !ok {
		return
	}
	if sb.ActiveRun != "" {
		// Suspend never kills a run implicitly: the caller must signal it and
		// observe a terminal state first.
		s.errorf(w, conflict("run_conflict", "the sandbox has an active run",
			map[string]any{"active_run_id": sb.ActiveRun}))
		return
	}
	switch sb.State {
	case "ready", "suspending", "suspended":
		sb.State = "suspended"
		writeJSON(w, 202, s.sandboxJSON(sb))
	default:
		s.errorf(w, conflict("invalid_state_transition", fmtf("cannot suspend from %s", sb.State), nil))
	}
}

func (s *Server) resumeSandbox(w http.ResponseWriter, r resolved) {
	sb, ok := s.lookupSandbox(w, r.sandboxID)
	if !ok {
		return
	}
	switch sb.State {
	case "suspended", "resuming", "ready":
		// The pinned revision, not the profile's current one: a newer revision
		// never rewrites a sandbox in place.
		if p, known := s.profiles[sb.ProfileID]; !known || p.Status == "withdrawn" || p.Revision != sb.Revision {
			s.errorf(w, conflict("profile_revision_unavailable", "the pinned revision has been withdrawn",
				map[string]any{"profile_revision": sb.Revision}))
			return
		}
		sb.State = "ready"
		writeJSON(w, 202, s.sandboxJSON(sb))
	default:
		s.errorf(w, conflict("invalid_state_transition", fmtf("cannot resume from %s", sb.State), nil))
	}
}

func (s *Server) startRun(w http.ResponseWriter, req *http.Request, r resolved, body []byte) {
	sb, ok := s.lookupSandbox(w, r.sandboxID)
	if !ok {
		return
	}
	// The whole body is measured before anything is parsed or claimed, as the
	// server does: the schema alone would admit an argv large enough that every
	// per-field limit needed a parse first. A pre-claim outcome.
	if sb.Limits.RequestBody > 0 && int64(len(body)) > sb.Limits.RequestBody {
		s.errorf(w, tooLarge("request body exceeds the profile's limit", sb.Limits.RequestBody))
		return
	}
	scope, resource, replayed, ok := s.keyed(w, req, req.URL.Path, body)
	if !ok {
		return
	}
	if replayed {
		if s.dropLocked(RouteStartRun) {
			panic(http.ErrAbortHandler)
		}
		run, live := s.runs[resource]
		if !live {
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, 200, map[string]any{"run_id": resource, "sandbox_id": sb.ID, "state": "lost"})
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, 200, s.runJSON(run))
		return
	}
	if sb.State == "suspended" {
		s.errorf(w, conflict("sandbox_suspended", "the sandbox is suspended", nil))
		return
	}
	if sb.State != "ready" {
		s.errorf(w, conflict("sandbox_not_ready", fmtf("the sandbox is %s", sb.State), nil))
		return
	}
	if sb.ActiveRun != "" {
		// The single active slot, claimed serializably under a `ready` and
		// null-active predicate. Not an invitation to signal the other run.
		s.errorf(w, conflict("run_conflict", "the sandbox already has an active run",
			map[string]any{"active_run_id": sb.ActiveRun, "active_run_state": s.runs[sb.ActiveRun].State}))
		return
	}
	var request struct {
		Argv   []string          `json:"argv"`
		Labels map[string]string `json:"labels"`
		Stdin  *struct {
			Mode      string  `json:"mode"`
			InlineB64 *string `json:"inline_b64"`
		} `json:"stdin"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Argv) == 0 {
		s.errorf(w, badRequest("invalid_request", "argv is required", nil))
		return
	}
	// Stdin is validated *before* the keyed claim commits, exactly as the
	// contract orders it: body and profile-limit validation are pre-claim
	// outcomes, so a refusal here stores nothing under the key and the same key
	// with a different body is a first use rather than a conflict (#284).
	mode, inline := "closed", []byte(nil)
	if request.Stdin != nil {
		mode = request.Stdin.Mode
		switch {
		case mode != "closed" && mode != "inline" && mode != "streaming":
			s.errorf(w, badRequest("invalid_request", "stdin.mode must be one of closed, inline, streaming", nil))
			return
		case mode == "inline" && request.Stdin.InlineB64 == nil:
			s.errorf(w, badRequest("invalid_request", "stdin.inline_b64 is required when mode is inline", nil))
			return
		case mode != "inline" && request.Stdin.InlineB64 != nil:
			s.errorf(w, badRequest("invalid_request", "stdin.inline_b64 is only valid when mode is inline", nil))
			return
		case mode == "inline":
			decoded, err := base64.StdEncoding.Strict().DecodeString(*request.Stdin.InlineB64)
			if err != nil {
				s.errorf(w, badRequest("invalid_request", "stdin.inline_b64 is not canonical base64", nil))
				return
			}
			if int64(len(decoded)) > sb.Limits.Inline {
				s.errorf(w, tooLarge("inline stdin exceeds the profile's limit", sb.Limits.Inline))
				return
			}
			inline = decoded
		}
	}
	created := &runRec{
		ID: s.nextID("run"), SandboxID: sb.ID, State: "queued",
		Labels: request.Labels,
		Sealed: "unknown", Reaped: "unknown", Quiet: "unknown",
		StdinMode: mode, Stdin: inline, StdinClosed: mode != "streaming",
		StdinOffset:   int64(len(inline)),
		StdinReceipts: map[int64]stdinReceipt{},
		changed:       make(chan struct{}),
	}
	// The run record, the active-slot claim and the idempotency result commit in
	// one step, so two keys cannot race through a preliminary check.
	s.runs[created.ID] = created
	sb.ActiveRun = created.ID
	s.idem[scope] = &idemRecord{Fingerprint: fingerprint(body), Resource: created.ID}
	if s.dropLocked(RouteStartRun) {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Idempotency-Replayed", "false")
	writeJSON(w, 201, s.runJSON(created))
}

func (s *Server) lookupRun(w http.ResponseWriter, r resolved) (*runRec, bool) {
	if _, ok := s.lookupSandbox(w, r.sandboxID); !ok {
		return nil, false
	}
	run, ok := s.runs[r.runID]
	if !ok || run.SandboxID != r.sandboxID {
		// No route resolves a run without its sandbox, which is what stops one
		// consumer attaching to another's.
		s.errorf(w, notFound("no such run in this sandbox"))
		return nil, false
	}
	return run, true
}

func (s *Server) getRun(w http.ResponseWriter, r resolved) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, s.runJSON(run))
}

func (s *Server) waitForRun(w http.ResponseWriter, r resolved) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	// Elapsing is not an error: the caller distinguishes the two by reading
	// state. This fake never blocks here, which is the elapsed case every time
	// for a non-terminal run.
	writeJSON(w, 200, s.runJSON(run))
}

func (s *Server) writeStdin(w http.ResponseWriter, r resolved, body []byte) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	limits := s.sandboxes[run.SandboxID].Limits
	if limits.RequestBody > 0 && int64(len(body)) > limits.RequestBody {
		s.errorf(w, tooLarge("request body exceeds the profile's limit", limits.RequestBody))
		return
	}
	var request struct {
		Offset  int64  `json:"offset"`
		DataB64 string `json:"data_b64"`
		Close   bool   `json:"close"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		s.errorf(w, badRequest("invalid_request", "malformed body", nil))
		return
	}
	// The contract's order, step by step (docs/v2/03-run-lifecycle.md, Stdin).
	// Decoding comes first and is strict; an empty chunk is valid only as an
	// explicit close.
	decoded, err := base64.StdEncoding.Strict().DecodeString(request.DataB64)
	if err != nil {
		s.errorf(w, badRequest("invalid_request", "data_b64 is not canonical base64", nil))
		return
	}
	if len(decoded) == 0 && !request.Close {
		s.errorf(w, badRequest("invalid_request", "an empty chunk is valid only with close", nil))
		return
	}
	digest := fingerprint(decoded)
	// 1. A completed receipt at this offset answers before any state check. An
	//    exact resend is a no-op returning the recorded next_offset even after
	//    close or termination; different bytes or a different close flag at a
	//    receipted offset is a mismatch, never a redelivery.
	if receipt, ok := run.StdinReceipts[request.Offset]; ok {
		run.StdinWrites++
		if receipt.Count != len(decoded) || receipt.Digest != digest || receipt.Close != request.Close {
			s.errorf(w, conflict("stdin_offset_mismatch", "a different write is receipted at this offset",
				map[string]any{"expected_offset": run.StdinOffset}))
			return
		}
		writeJSON(w, 200, map[string]any{
			"accepted_bytes": receipt.Count, "next_offset": receipt.Next, "closed": receipt.Close,
		})
		return
	}
	// 4. A fresh write needs streaming mode and open stdin.
	if run.StdinMode != "streaming" || run.StdinClosed {
		s.errorf(w, conflict("run_not_accepting_stdin", "the run's stdin is not open for writes", nil))
		return
	}
	// 5. The state partition. `queued` is transiently not ready. The runner's
	//    acknowledgement, which the contract permits at any moment, is modelled
	//    as arriving right after this first premature write — so the client's
	//    retry path is exercised by a real refusal without a test-side clock.
	switch run.State {
	case "queued":
		run.State = "accepted"
		s.errorf(w, conflict("run_not_ready_for_stdin", "the runner has not acknowledged the dispatch", nil))
		return
	case "accepted", "running":
	default:
		s.errorf(w, conflict("run_not_accepting_stdin", fmtf("the run is %s", run.State), nil))
		return
	}
	if f, ok := s.stdinFaults[request.Offset]; ok {
		// A scripted interruption: the write never reaches the stdin state, so
		// it is not counted among the writes that did.
		delete(s.stdinFaults, request.Offset)
		s.errorf(w, f)
		return
	}
	run.StdinWrites++
	// 6. Offsets are exact.
	if request.Offset != run.StdinOffset {
		s.errorf(w, conflict("stdin_offset_mismatch", "unexpected offset",
			map[string]any{"expected_offset": run.StdinOffset}))
		return
	}
	if int64(len(decoded)) > limits.Chunk {
		s.errorf(w, tooLarge("a stdin chunk exceeds the profile's limit", limits.Chunk))
		return
	}
	if limits.Total > 0 && int64(len(run.Stdin)+len(decoded)) > limits.Total {
		s.errorf(w, tooLarge("the run's total stdin exceeds the profile's limit", limits.Total))
		return
	}
	run.Stdin = append(run.Stdin, decoded...)
	run.StdinOffset += int64(len(decoded))
	run.StdinReceipts[request.Offset] = stdinReceipt{
		Count: len(decoded), Digest: digest, Close: request.Close, Next: run.StdinOffset,
	}
	if request.Close {
		run.StdinClosed = true
		if run.State == "running" {
			s.appendLocked(run, map[string]any{"kind": "stdin.closed", "reason": "client_close"})
		}
	}
	writeJSON(w, 200, map[string]any{
		"accepted_bytes": len(decoded), "next_offset": run.StdinOffset, "closed": request.Close,
	})
}

func (s *Server) signalRun(w http.ResponseWriter, req *http.Request, r resolved, body []byte) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	scope, resource, replayed, ok := s.keyed(w, req, req.URL.Path, body)
	if !ok {
		return
	}
	if replayed {
		// A replay returns the original signal id and appends nothing. This is
		// what keeps a stop ladder that asks repeatedly from exhausting the
		// per-run signal quota.
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, 200, s.signalJSON(run, resource))
		return
	}
	var request struct {
		Signal string `json:"signal"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.Signal == "" {
		s.errorf(w, badRequest("invalid_request", "signal is required", nil))
		return
	}
	if stateTerminal(run.State) {
		// A completed no-op: 200, a null signal id, no process or history change.
		s.idem[scope] = &idemRecord{Fingerprint: fingerprint(body)}
		writeJSON(w, 200, s.signalJSON(run, ""))
		return
	}
	if run.Signals >= 64 {
		f := conflict("signal_limit_exceeded", "the per-run client signal quota is exhausted", nil)
		s.idem[scope] = &idemRecord{Fingerprint: fingerprint(body), Failure: &f}
		s.errorf(w, f)
		return
	}
	run.Signals++
	if request.Signal != "KILL" {
		run.State = "terminating"
	}
	id := s.nextID("sig")
	s.idem[scope] = &idemRecord{Fingerprint: fingerprint(body), Resource: id}
	if s.dropLocked(RouteSignalRun) {
		panic(http.ErrAbortHandler)
	}
	// 202 acknowledges the request. It is not delivery and certainly not
	// termination: the evidence fields are untouched here, on purpose.
	writeJSON(w, 202, s.signalJSON(run, id))
}

func (s *Server) serveEvents(w http.ResponseWriter, req *http.Request, r resolved) {
	after, err := strconv.ParseInt(orDefault(req.URL.Query().Get("after"), "0"), 10, 64)
	if err != nil || after < 0 {
		s.mu.Lock()
		s.errorf(w, badRequest("invalid_request", "after must be a non-negative integer", nil))
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	wait := s.maxWait
	s.mu.Unlock()
	if requested := req.URL.Query().Get("wait_seconds"); requested == "0" {
		wait = 0
	}

	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		if f, ok := s.takeFaultLocked(RouteGetRunEvents); ok {
			s.errorf(w, f)
			s.mu.Unlock()
			return
		}
		run, ok := s.lookupRun(w, r)
		if !ok {
			s.mu.Unlock()
			return
		}
		latest := int64(len(run.Events))
		switch {
		case after > latest:
			// A cursor past this run's log means the client is holding one from
			// a different run.
			s.errorf(w, badRequest("cursor_ahead", "cursor is ahead of this run's log",
				map[string]any{"requested_after": after, "latest_seq": latest}))
			s.mu.Unlock()
			return
		case run.Floor > 0 && after < run.Floor-1:
			s.errorf(w, conflict("cursor_too_old", "the requested cursor has expired",
				map[string]any{"requested_after": after, "oldest_available_seq": run.Floor}))
			s.mu.Unlock()
			return
		}
		if latest > after {
			page := run.Events[after:]
			if len(page) > 256 {
				page = page[:256]
			}
			body := map[string]any{
				"events":               page,
				"cursor":               toInt64(page[len(page)-1]["seq"]),
				"latest_seq":           latest,
				"oldest_available_seq": maxInt64(run.Floor, 1),
				"has_more":             toInt64(page[len(page)-1]["seq"]) < latest,
				"expires_at":           nil,
			}
			writeJSON(w, 200, body)
			s.mu.Unlock()
			return
		}
		changed, floor := run.changed, run.Floor
		s.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Elapsing returns 200 with an empty page, never an error. The run
			// fields were copied under the lock: nothing below may touch the
			// record, or a test emitting from another goroutine would race the
			// response it is waiting for.
			writeJSON(w, 200, map[string]any{
				"events": []any{}, "cursor": after, "latest_seq": latest,
				"oldest_available_seq": floor, "has_more": false, "expires_at": nil,
			})
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-changed:
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

func (s *Server) sandboxJSON(sb *sandboxRec) map[string]any {
	var active any
	if sb.ActiveRun != "" {
		active = sb.ActiveRun
	}
	var del any
	if sb.Deletion != nil {
		del = map[string]any{
			"requested_at":         "2026-01-01T00:00:00.000Z",
			"compute_released":     sb.Deletion.Compute,
			"volume_destroyed":     sb.Deletion.Volume,
			"record_tombstoned":    sb.Deletion.Tombstone,
			"confirmed_at":         "2026-01-01T00:00:00.000Z",
			"tombstone_expires_at": nil,
		}
	}
	return map[string]any{
		"sandbox_id":       sb.ID,
		"owner":            sb.Owner,
		"state":            sb.State,
		"profile_id":       sb.ProfileID,
		"profile_revision": sb.Revision,
		"workspace_id":     sb.WorkspaceID,
		"active_run_id":    active,
		"labels":           sb.Labels,
		"retention": map[string]any{
			"idle_since": nil, "idle_suspend_after_seconds": 900,
			"delete_after_idle_seconds": 86400, "tombstone_seconds": 3600,
		},
		"deletion":     del,
		"state_reason": nil,
		"created_at":   "2026-01-01T00:00:00.000Z",
		"updated_at":   "2026-01-01T00:00:00.000Z",
		"ready_at":     "2026-01-01T00:00:00.000Z",
		"suspended_at": nil,
	}
}

func (s *Server) runJSON(r *runRec) map[string]any {
	return map[string]any{
		"run_id":     r.ID,
		"sandbox_id": r.SandboxID,
		"state":      r.State,
		"spec": map[string]any{
			"argv0": "/usr/bin/true", "argv_entries": 1,
			"argv_sha256": strings.Repeat("0", 64), "cwd": "/workspace",
			"env_keys": []any{},
			"stdin":    map[string]any{"mode": orDefault(r.StdinMode, "closed"), "inline_bytes": inlineBytes(r)},
			"timeouts": map[string]any{"hard_seconds": 3600, "output_stall_seconds": 300},
			"output":   map[string]any{"max_bytes": 1 << 20, "overflow_policy": "truncate"},
		},
		"termination": s.terminationLocked(r),
		"signals":     []any{},
		"events": map[string]any{
			"latest_seq": int64(len(r.Events)), "oldest_available_seq": maxInt64(r.Floor, 0),
			"expires_at": nil, "total_output_bytes": 0, "dropped_output_bytes": 0,
		},
		"exit_code":   r.ExitCode,
		"signal":      r.Signal,
		"labels":      r.Labels,
		"created_at":  "2026-01-01T00:00:00.000Z",
		"accepted_at": nil,
		"started_at":  nil,
		"terminal_at": nil,
	}
}

func (s *Server) signalJSON(r *runRec, signalID string) map[string]any {
	var id any
	if signalID != "" {
		id = signalID
	}
	return map[string]any{
		"signal_id":   id,
		"run_id":      r.ID,
		"state":       r.State,
		"signals":     []any{},
		"termination": s.terminationLocked(r),
	}
}

// inlineBytes is the spec's echo of an inline payload's length — the length and
// never the bytes, which audit and API responses exclude by default.
func inlineBytes(r *runRec) int {
	if r.StdinMode != "inline" {
		return 0
	}
	return len(r.Stdin)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
