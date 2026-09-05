// Package airlocktest is a contract-faithful in-process Airlock v2 server.
//
// It exists because the alternative — a hand-written stub per test — is a fake
// that invents guarantees the real server does not make, which AGENTS.md calls
// worse than a missing test: it lets code that depends on the invention pass.
// The properties modelled here are the ones BEN's correctness rests on, and each
// is the *server's* half of a rule the client claims:
//
//   - Idempotency is keyed, fingerprinted and scoped to tenant, subject, method
//     and resolved path. A replay returns the stored resource; the same key
//     with a different body is `idempotency_key_conflict` and creates nothing.
//   - A sandbox has at most one active run, claimed under a `ready` and
//     null-active predicate. A second start is `run_conflict`.
//   - Termination evidence is three independent fields that start `unknown` and
//     only positive evidence moves. Nothing here upgrades one from another, and
//     a `lost` run leaves domain quiet `unknown` forever.
//   - A terminal run whose domain quiet is not `confirmed` moves its sandbox
//     from `ready` to `failed` in the same transaction that clears the active
//     slot, so a replacement can never race into an unquiet domain.
//   - Events are per-run, contiguous from 1, and `after` is exact. A cursor
//     below the retention floor is `cursor_too_old`; one past the log is
//     `cursor_ahead`.
//   - Ownership is checked before state: another tenant is `404`, another
//     subject in the owning tenant is `403`, and client-id rotation is allowed.
//   - Deletion returns `202` and reaches `deleted` only when all three evidence
//     fields confirm.
//
// What it does not model is anything BEN cannot observe: quotas, rate limits,
// the runner protocol, encryption, audit. Those are named in the contract's
// internal-protocol section and are the server's business.
//
// Everything is deterministic. Identifiers come from a counter, no wall clock
// decides an outcome, and a run only produces output when a test says so.
//
// Airlock intentionally maintains [a separate fake] for its in-repository
// client and airlockctl. Keeping the implementations independent tests that the
// frozen wire contract can be implemented separately; sharing one fake would
// remove that evidence. [Airlock issue #506] tracks validating both fakes
// against the shared fixtures so either implementation's drift is visible.
//
// [a separate fake]: https://github.com/srhg-ai-7cef3f93/airlock/tree/main/client/airlockv2/airlocktest
// [Airlock issue #506]: https://github.com/srhg-ai-7cef3f93/airlock/issues/506
package airlocktest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Principal is the sandbox owner, derived from token claims and never from
// request content.
type Principal struct {
	TenantID string `json:"tenant_id"`
	Subject  string `json:"subject"`
	ClientID string `json:"client_id"`
}

// DefaultToken is the bearer token the server accepts unless a test changes it.
const DefaultToken = "airlocktest-access-token"

// DefaultProfile is the approved profile a fresh server publishes.
const DefaultProfile = "ben-agent"

// defaultWait caps how long a long poll blocks server-side. Short, because the
// alternative is a suite whose duration is set by a timeout constant rather than
// by the work; the client's own wait_seconds is honoured only up to this.
const defaultWait = 25 * time.Millisecond

// Server is the fake control plane.
type Server struct {
	tb testing.TB

	mu        sync.Mutex
	http      *httptest.Server
	seq       int
	token     string
	owner     Principal
	profiles  map[string]profile
	sandboxes map[string]*sandboxRec
	runs      map[string]*runRec
	idem      map[idemScope]*idemRecord
	// requests records every request body the server received, so a test can
	// prove a credential was never serialized into one.
	requests []Request
	// drop is how many further responses to abandon per route, mid-handler and
	// after the side effect has committed — the lost-response case the whole
	// idempotency contract exists for.
	drop map[string]int
	// faults injects a stored refusal for the next call to a route.
	faults map[string][]fault
	// stdinFaults refuse the fresh stdin write at one offset, once — the seam a
	// test uses to interrupt a delivery partway (FailStdinAt).
	stdinFaults map[int64]fault
	// partitioned makes every request fail at the transport, which is what an
	// unreachable backend looks like to a client.
	partitioned bool
	// pendingDeletion is the evidence a scripted DELETE records instead of an
	// immediate confirmation (PendingDeletion).
	pendingDeletion *deletion
	maxWait         time.Duration
}

// Request is one recorded call. Body is the raw bytes, so a test asserting that
// no credential crossed the wire reads exactly what crossed it.
type Request struct {
	Method string
	Path   string
	Key    string
	Auth   string
	Body   []byte
}

type profile struct {
	Revision string
	Status   string
	Limits   Limits
}

// Limits is the stdin envelope a published profile pins (#284): the largest
// prompt a start request may carry inline, the largest single streaming write,
// the most a run may receive by either path, and the largest whole request
// body — measured before parsing, base64 and framing included. Zero Total is
// unbounded, as the contract reads it; zero Inline forbids inline stdin; a zero
// RequestBody is normalised to the deployed value by SetProfileLimits, because
// the contract gives it a minimum of one.
//
// A sandbox pins the Limits of the profile as published when it was created,
// and every run in it is judged by those — never by what the profile says now.
type Limits struct {
	Inline      int64
	Chunk       int64
	Total       int64
	RequestBody int64
}

// DefaultLimits are the deployed catalogue's values, so a test that says
// nothing about limits runs against the envelope BEN's canary actually has.
var DefaultLimits = Limits{Inline: 65536, Chunk: 65536, Total: 16777216, RequestBody: 1048576}

// stdinReceipt is the byte-free durable record of one accepted stdin write:
// what the contract replays for an exact resend, and what it compares an
// altered resend against.
type stdinReceipt struct {
	Count  int
	Digest string
	Close  bool
	Next   int64
}

type sandboxRec struct {
	ID          string
	Owner       Principal
	State       string
	ProfileID   string
	Revision    string
	WorkspaceID string
	ActiveRun   string
	Labels      map[string]string
	Deletion    *deletion
	// Limits are the envelope pinned with Revision at creation; startRun and
	// writeStdin judge this sandbox's runs by them, not by the profile's
	// current publication.
	Limits Limits
}

type deletion struct {
	Compute   string
	Volume    string
	Tombstone string
}

type runRec struct {
	ID        string
	SandboxID string
	Labels    map[string]string
	State     string
	Reason    string
	Sealed    string
	Reaped    string
	Quiet     string
	ExitCode  *int
	Signal    *string
	Events    []map[string]any
	// Floor is the oldest sequence still retained; 0 until a test expires some.
	Floor int64
	// Signals counts client-origin durable records, against the contract's cap.
	Signals int
	// StdinOffset is the byte position the stdin stream has reached.
	StdinOffset int64
	// StdinMode is the mode the start request fixed; Stdin is every byte the
	// run has received by either path; StdinClosed says no more may arrive.
	// StdinReceipts are the offset-addressed receipts the contract replays,
	// and StdinWrites counts write requests that reached the stdin state
	// checks, receipts included.
	StdinMode     string
	Stdin         []byte
	StdinClosed   bool
	StdinReceipts map[int64]stdinReceipt
	StdinWrites   int
	changed       chan struct{}
}

type idemRecord struct {
	Fingerprint string
	Resource    string
	// Failure is a stored terminal failure replayed with a fresh request id.
	Failure *fault
}

type idemScope struct {
	TenantID string
	Subject  string
	Method   string
	Path     string
	Key      string
}

type fault struct {
	Status     int
	Code       string
	Message    string
	RetryAfter *int
	Details    map[string]any
}

// New starts a fake control plane with one approved profile and one owner. It
// is registered for cleanup with tb.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		tb:          tb,
		token:       DefaultToken,
		owner:       Principal{TenantID: "ben", Subject: "ben-daemon", ClientID: "ben-cli"},
		profiles:    map[string]profile{DefaultProfile: {Revision: revision("v1"), Status: "approved", Limits: DefaultLimits}},
		sandboxes:   map[string]*sandboxRec{},
		runs:        map[string]*runRec{},
		idem:        map[idemScope]*idemRecord{},
		drop:        map[string]int{},
		faults:      map[string][]fault{},
		stdinFaults: map[int64]fault{},
		maxWait:     defaultWait,
	}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(s.http.Close)
	return s
}

// URL is the base URL to configure a client with. It is http rather than https,
// which the production client refuses — tests reach the transport directly
// through Options.Transport for exactly that reason (see Transport).
func (s *Server) URL() string { return s.http.URL }

// Transport routes an https base URL at this in-process server.
//
// The client refuses a plain-http endpoint, deliberately and permanently: a
// bearer token and a run's whole output are not things to send in the clear. So
// a test configures `https://airlock.test` and substitutes this round tripper,
// which keeps the refusal under test rather than making the fake the reason to
// weaken it.
func (s *Server) Transport() http.RoundTripper { return &rewriteTransport{target: s.http.URL} }

type rewriteTransport struct{ target string }

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	parsed := clone.URL
	target := strings.TrimPrefix(t.target, "http://")
	parsed.Scheme, parsed.Host = "http", target
	clone.Host = target
	return http.DefaultTransport.RoundTrip(clone)
}

// Token replaces the accepted bearer token.
func (s *Server) Token(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

// Owner replaces the authenticated principal for later requests. Existing
// sandboxes keep the owner recorded at creation, while keyed requests use the
// new tenant/subject scope; together those reproduce credential rotation and
// cross-principal collisions.
func (s *Server) Owner(p Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner = p
}

// SetProfile publishes a profile at a status. Removing one is spelled by status
// "withdrawn"; an unknown profile id is a 404.
func (s *Server) SetProfile(id, status, rev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limits := DefaultLimits
	if existing, ok := s.profiles[id]; ok {
		limits = existing.Limits
	}
	s.profiles[id] = profile{Revision: rev, Status: status, Limits: limits}
}

// SetProfileLimits replaces the envelope the profile publishes at its current
// revision, for sandboxes created from now on. Existing sandboxes keep the
// envelope they pinned at creation, exactly as the contract has it. The
// revision is deliberately left alone: a test modelling a rollout moves it
// with SetProfile and then states the new revision's limits here.
func (s *Server) SetProfileLimits(id string, limits Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok {
		s.tb.Fatalf("airlocktest: no profile %s", id)
	}
	if limits.RequestBody <= 0 {
		limits.RequestBody = DefaultLimits.RequestBody
	}
	p.Limits = limits
	s.profiles[id] = p
}

// Stdin returns every byte a run has received on stdin, by either path.
func (s *Server) Stdin(runID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.mustRun(runID).Stdin...)
}

// StdinMode reports the stdin mode a run's start request fixed.
func (s *Server) StdinMode(runID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mustRun(runID).StdinMode
}

// StdinClosed reports whether a run's stdin can receive no more.
func (s *Server) StdinClosed(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mustRun(runID).StdinClosed
}

// StdinWrites counts the stdin write requests a run has answered past its
// receipt lookup — fresh writes and exact resends alike.
func (s *Server) StdinWrites(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mustRun(runID).StdinWrites
}

// Partition makes every request fail at the transport. An unreachable backend,
// which must never be readable as termination.
func (s *Server) Partition(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partitioned = on
}

// DropNextResponse abandons the next response for a route *after* its side
// effect has committed — the lost-response case a client must resolve by
// replaying its key rather than minting a new one.
func (s *Server) DropNextResponse(method, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	if route, ok := resolve(method, path); ok {
		key = route.route
	}
	s.drop[key]++
}

// FailNext queues one stored refusal for a route.
func (s *Server) FailNext(method, path string, status int, code, message string, details map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	f := fault{Status: status, Code: code, Message: message, Details: details}
	if retryableCode(code) {
		zero := 0
		f.RetryAfter = &zero
	}
	s.faults[key] = append(s.faults[key], f)
}

// FailStdinAt refuses the next fresh stdin write that begins at offset, once,
// with the given refusal. Receipted resends at that offset are unaffected, so
// this interrupts a delivery exactly where a crash would and lets the resume
// walk the delivered prefix as the contract promises.
func (s *Server) FailStdinAt(offset int64, status int, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stdinFaults[offset] = fault{Status: status, Code: code, Message: message}
}

// ForgetIdempotency removes one completed keyed result in the current
// tenant/subject scope, modelling expiry after the contract's 24-hour retention
// window without making the fake depend on a wall clock.
func (s *Server) ForgetIdempotency(method, path, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.idem, idemScope{
		TenantID: s.owner.TenantID, Subject: s.owner.Subject, Method: method, Path: path, Key: key,
	})
}

// Requests returns every recorded call.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// SandboxIDs lists live sandbox identifiers, oldest first. A test proving that
// a crash did not create a second sandbox counts these.
func (s *Server) SandboxIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

// RunIDs lists run identifiers, sorted. Counting these is how a duplicate
// dispatch is proven not to have happened.
func (s *Server) RunIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

// SandboxState reports one sandbox's current state, or "" when it is gone.
func (s *Server) SandboxState(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb, ok := s.sandboxes[id]; ok {
		return sb.State
	}
	return ""
}

// PendingDeletion makes the next DELETE answer `202 deleting` with the three
// evidence fields left as given, instead of the immediate confirmation the fake
// otherwise models.
//
// It exists because "deletion is not complete when the call returns" is the
// contract's own sentence, and a fake that always confirmed on the 202 could
// not produce the state a client is supposed to wait through. Each argument is
// an evidence value — "confirmed" or anything else, which the client must read
// as not-yet — so a test can script a *partial* confirmation, which is the one
// shape that reads as done to code checking two of the three fields.
//
// The active slot is still released and the sandbox still leaves `ready`: a
// deletion in progress has already taken the domain, and modelling otherwise
// would let a run appear to survive its own sandbox.
func (s *Server) PendingDeletion(compute, volume, tombstone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingDeletion = &deletion{Compute: compute, Volume: volume, Tombstone: tombstone}
}

// ConfirmDeletion completes a deletion already in progress, as the control
// plane does once the volume is actually gone. It also clears the scripted
// PendingDeletion, so a later DELETE confirms normally.
func (s *Server) ConfirmDeletion(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingDeletion = nil
	sb, ok := s.sandboxes[id]
	if !ok || sb.Deletion == nil {
		return
	}
	sb.Deletion.Compute, sb.Deletion.Volume, sb.Deletion.Tombstone = "confirmed", "confirmed", "confirmed"
	sb.State = "deleted"
}

// ActiveRun reports the run holding a sandbox's single active slot.
func (s *Server) ActiveRun(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb, ok := s.sandboxes[id]; ok {
		return sb.ActiveRun
	}
	return ""
}

// RunState reports one run's current state.
func (s *Server) RunState(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r.State
	}
	return ""
}

// SignalCount reports how many client-origin signal records a run holds. The
// number a per-call idempotency key would grow without bound.
func (s *Server) SignalCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r.Signals
	}
	return 0
}

// Emit appends one output chunk to a run's event log. Chunk boundaries are
// wherever the caller puts them: the contract is explicit that a chunk may
// split a line or a JSON object, and BEN's framing has to survive that.
func (s *Server) Emit(runID, stream string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRun(runID)
	if r.State == "queued" || r.State == "accepted" {
		s.startRunLocked(r)
	}
	s.appendLocked(r, map[string]any{
		"kind":          "output",
		"stream":        stream,
		"data_b64":      base64.StdEncoding.EncodeToString(data),
		"bytes":         len(data),
		"stream_offset": 0,
	})
}

// Truncate appends the contract's durable output.truncated event. Consumers
// that form a verdict from process output must preserve this fact rather than
// treating the retained prefix as the complete stream.
func (s *Server) Truncate(runID string, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRun(runID)
	if r.State == "queued" || r.State == "accepted" {
		s.startRunLocked(r)
	}
	s.appendLocked(r, map[string]any{
		"kind": "output.truncated", "dropped_bytes": dropped,
	})
}

// Terminal describes how a run ended: its state, its wait status, and each of
// the three evidence fields independently.
type Terminal struct {
	// State is exited, failed or lost.
	State string
	// Reason is the contract's TerminationReason.
	Reason string
	// Sealed, Reaped and Quiet are the three evidence fields. Empty means
	// `unknown`, which is what a run nobody could inspect reports.
	Sealed, Reaped, Quiet string
	ExitCode              *int
	Signal                string
}

// Exited is the ordinary success shape: the process was reaped with an exit
// status, its streams sealed, and its execution domain observed quiet.
func Exited(code int) Terminal {
	return Terminal{
		State: "exited", Reason: "process_exit",
		Sealed: "confirmed", Reaped: "confirmed", Quiet: "confirmed", ExitCode: &code,
	}
}

// Lost is a run Airlock can no longer obtain evidence about. Every field stays
// unknown, which is the case that must never read as termination.
func Lost() Terminal { return Terminal{State: "lost", Reason: "runner_lost"} }

// TimedOut is the hard-timeout shape: the ladder ran, the process was reaped,
// and the domain was observed quiet.
func TimedOut() Terminal {
	code := 137
	return Terminal{
		State: "exited", Reason: "hard_timeout",
		Sealed: "confirmed", Reaped: "confirmed", Quiet: "confirmed",
		ExitCode: &code, Signal: "KILL",
	}
}

// Terminate ends a run, appending its single run.terminal event.
//
// The sandbox transition is the interesting half and is deliberately not a
// choice a test makes: a terminal run whose domain quiet is not `confirmed`
// moves the sandbox `ready -> failed` in the same step that clears the active
// slot, exactly as the contract requires, so a replacement run can never race
// into a domain whose quietness is unknown.
func (s *Server) Terminate(runID string, t Terminal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRun(runID)
	if stateTerminal(r.State) {
		return
	}
	r.State, r.Reason = t.State, t.Reason
	r.Sealed, r.Reaped, r.Quiet = evidence(t.Sealed), evidence(t.Reaped), evidence(t.Quiet)
	r.ExitCode = t.ExitCode
	if t.Signal != "" {
		sig := t.Signal
		r.Signal = &sig
	}
	s.appendLocked(r, map[string]any{
		"kind":                 "run.terminal",
		"state":                r.State,
		"exit_code":            r.ExitCode,
		"signal":               r.Signal,
		"termination":          s.terminationLocked(r),
		"total_output_bytes":   0,
		"dropped_output_bytes": 0,
	})
	if sb, ok := s.sandboxes[r.SandboxID]; ok {
		sb.ActiveRun = ""
		if sb.State == "ready" && r.Quiet != "confirmed" {
			sb.State = "failed"
		}
	}
}

// ExpireEvents raises a run's retention floor, so a cursor at or below it is
// `cursor_too_old`.
func (s *Server) ExpireEvents(runID string, floor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustRun(runID).Floor = floor
}

// Rewrite replaces the payload of an already-sequenced output event. The
// backend's log ceasing to be the log BEN read — the one thing no dedupe rule
// can repair.
func (s *Server) Rewrite(runID string, seq int64, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRun(runID)
	for _, ev := range r.Events {
		if toInt64(ev["seq"]) != seq {
			continue
		}
		ev["data_b64"] = base64.StdEncoding.EncodeToString(data)
		ev["bytes"] = len(data)
		return
	}
	s.tb.Fatalf("airlocktest: run %s has no event at sequence %d", runID, seq)
}

func (s *Server) mustRun(id string) *runRec {
	r, ok := s.runs[id]
	if !ok {
		s.tb.Fatalf("airlocktest: no run %s", id)
	}
	return r
}

func (s *Server) startRunLocked(r *runRec) {
	r.State = "running"
	s.appendLocked(r, map[string]any{"kind": "run.started", "pid": 4242})
}

// appendLocked assigns the next contiguous sequence and wakes every long poll.
func (s *Server) appendLocked(r *runRec, ev map[string]any) {
	ev["seq"] = int64(len(r.Events) + 1)
	ev["run_id"] = r.ID
	ev["emitted_at"] = "2026-01-01T00:00:00.000Z"
	r.Events = append(r.Events, ev)
	close(r.changed)
	r.changed = make(chan struct{})
}

func (s *Server) terminationLocked(r *runRec) map[string]any {
	return map[string]any{
		"reason":         orDefault(r.Reason, "unknown"),
		"stream_sealed":  evidence(r.Sealed),
		"process_reaped": evidence(r.Reaped),
		"domain_quiet":   evidence(r.Quiet),
		"observed_at":    nil,
		"detail":         nil,
	}
}

func evidence(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func stateTerminal(s string) bool { return s == "exited" || s == "failed" || s == "lost" }

func retryableCode(code string) bool {
	switch code {
	case "sandbox_not_ready", "sandbox_suspended", "run_conflict", "run_not_ready_for_stdin",
		"idempotency_key_in_flight", "rate_limited", "quota_exceeded", "internal", "dependency_unavailable":
		return true
	}
	return false
}

func (s *Server) nextID(prefix string) string {
	s.seq++
	sum := sha256.Sum256([]byte(prefix + strconv.Itoa(s.seq)))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func revision(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Revision is the profile revision a seed produces, so a test can name the
// value it expects to be pinned.
func Revision(seed string) string { return revision(seed) }

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func requestID(n int) string {
	sum := sha256.Sum256([]byte("req" + strconv.Itoa(n)))
	return "req_" + hex.EncodeToString(sum[:16])
}

func (s *Server) errorf(w http.ResponseWriter, f fault) {
	s.seq++
	body := map[string]any{"error": map[string]any{
		"code":                f.Code,
		"message":             f.Message,
		"retryable":           retryableCode(f.Code),
		"retry_after_seconds": f.RetryAfter,
		"request_id":          requestID(s.seq),
		"details":             f.Details,
	}}
	if retryableCode(f.Code) && f.RetryAfter == nil {
		zero := 0
		body["error"].(map[string]any)["retry_after_seconds"] = &zero
	}
	writeJSON(w, f.Status, body)
}

func notFound(msg string) fault {
	return fault{Status: 404, Code: "not_found", Message: msg}
}

func conflict(code, msg string, details map[string]any) fault {
	return fault{Status: 409, Code: code, Message: msg, Details: details}
}

// tooLarge is the contract's 413: always `payload_too_large`, always naming the
// exceeded limit, and never carrying the bytes that exceeded it.
func tooLarge(msg string, limit int64) fault {
	return fault{Status: 413, Code: "payload_too_large", Message: msg, Details: map[string]any{"limit_bytes": limit}}
}

func badRequest(code, msg string, details map[string]any) fault {
	return fault{Status: 400, Code: code, Message: msg, Details: details}
}

func fmtf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
