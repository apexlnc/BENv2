package airlock

import "encoding/json"

// The Airlock v2 wire vocabulary, mirroring the frozen contract in that
// repository's docs/v2/openapi.yaml.
//
// Everything here stops at this package. Nothing in internal/core, the
// orchestrator, or a provider adapter may name one of these types — the whole
// point of the #192 boundary is that a backend's schema is a detail behind
// remote.Status and remote.Envelope (SPEC §3 invariant 6).
//
// Only what BEN reads is modelled. The contract's request bodies carry more
// than this client sends and its responses more than it needs; a partial mirror
// is deliberate, because a full one would have to be re-audited on every
// contract revision for fields nothing consumes.
//
// Decoding is tolerant of unknown fields even though the contract says an
// unknown field is a violation. That rule binds the *server*: it is what makes
// `additionalProperties: false` enforceable on requests. A client that refused
// an unrecognised response field would turn the contract's own additive-change
// path — a new optional field plus a revision — into an outage for every daemon
// that had not been redeployed yet.

// SandboxState is the sandbox lifecycle enum. `deleted` is the only terminal
// state; `failed` is not terminal, because deletion is still permitted from it.
type SandboxState string

const (
	SandboxPending    SandboxState = "pending"
	SandboxStarting   SandboxState = "starting"
	SandboxReady      SandboxState = "ready"
	SandboxSuspending SandboxState = "suspending"
	SandboxSuspended  SandboxState = "suspended"
	SandboxResuming   SandboxState = "resuming"
	SandboxDeleting   SandboxState = "deleting"
	SandboxDeleted    SandboxState = "deleted"
	SandboxFailed     SandboxState = "failed"
)

// Settling reports a state that is on its way somewhere and worth waiting on.
// Deliberately not "not ready": `failed`, `deleting` and `deleted` are answers,
// and polling them is how a client waits forever for a verdict it already has.
func (s SandboxState) Settling() bool {
	switch s {
	case SandboxPending, SandboxStarting, SandboxSuspending, SandboxResuming:
		return true
	}
	return false
}

// RunState is the run lifecycle enum. `exited`, `failed` and `lost` are
// terminal, and none of the three is evidence of anything by itself — `lost`
// most of all, which is precisely a run Airlock can no longer observe.
type RunState string

const (
	RunQueued      RunState = "queued"
	RunAccepted    RunState = "accepted"
	RunRunning     RunState = "running"
	RunTerminating RunState = "terminating"
	RunExited      RunState = "exited"
	RunFailed      RunState = "failed"
	RunLost        RunState = "lost"
)

func (s RunState) Terminal() bool {
	switch s {
	case RunExited, RunFailed, RunLost:
		return true
	}
	return false
}

// Evidence is the contract's tri-state observation. The zero value of the Go
// string is "", which is neither `confirmed` nor a claim of anything — the same
// fail-closed default `unknown` has, reached by a response that omitted the
// field entirely.
type Evidence string

const (
	EvidenceUnknown      Evidence = "unknown"
	EvidenceConfirmed    Evidence = "confirmed"
	EvidenceNotConfirmed Evidence = "not_confirmed"
)

// Confirmed is the only positive reading, and it is a method so that no call
// site can spell the comparison as "not not_confirmed" — which is the same
// fail-open mistake remote.MayReuse exists to prevent.
func (e Evidence) Confirmed() bool { return e == EvidenceConfirmed }

// Signal is the closed set a client may request.
type Signal string

const (
	SignalINT  Signal = "INT"
	SignalTERM Signal = "TERM"
	SignalKILL Signal = "KILL"
)

// StdinMode selects how a run receives standard input.
type StdinMode string

const (
	StdinClosed    StdinMode = "closed"
	StdinInline    StdinMode = "inline"
	StdinStreaming StdinMode = "streaming"
)

// EventKind is the sequenced run-event vocabulary. A heartbeat is a transport
// frame and deliberately not a kind.
type EventKind string

const (
	EventRunStarted      EventKind = "run.started"
	EventOutput          EventKind = "output"
	EventStdinClosed     EventKind = "stdin.closed"
	EventSignalDelivered EventKind = "signal.delivered"
	EventOutputTruncated EventKind = "output.truncated"
	EventRunTerminal     EventKind = "run.terminal"
)

// OutputStream names which of the process's streams a chunk came from.
type OutputStream string

const (
	StreamStdout OutputStream = "stdout"
	StreamStderr OutputStream = "stderr"
)

// TerminationReason says what ended a run. Orthogonal to RunState, which says
// what Airlock knows about the ending.
type TerminationReason string

const (
	ReasonProcessExit  TerminationReason = "process_exit"
	ReasonClientSignal TerminationReason = "client_signal"
	ReasonHardTimeout  TerminationReason = "hard_timeout"
	ReasonStallTimeout TerminationReason = "stall_timeout"
	ReasonOutputLimit  TerminationReason = "output_limit"
	ReasonSandboxDel   TerminationReason = "sandbox_deleted"
	ReasonRunnerLost   TerminationReason = "runner_lost"
	ReasonUnknown      TerminationReason = "unknown"
)

// TimedOut reports the two reasons a hook or a run was stopped at a deadline
// rather than exiting. Kept apart from a nonzero exit for the reason the local
// workspace path keeps them apart: only one of them means "your script is
// slower than the bound" (remote.HookResult).
func (r TerminationReason) TimedOut() bool {
	return r == ReasonHardTimeout || r == ReasonStallTimeout
}

// ErrorCode is the stable failure identifier. The contract is explicit that the
// code, not the HTTP status, is what a client routes on.
type ErrorCode string

const (
	CodeInvalidRequest         ErrorCode = "invalid_request"
	CodeIdempotencyKeyRequired ErrorCode = "idempotency_key_required"
	CodeCursorAhead            ErrorCode = "cursor_ahead"
	CodeUnauthenticated        ErrorCode = "unauthenticated"
	CodeForbidden              ErrorCode = "forbidden"
	CodeNotFound               ErrorCode = "not_found"
	CodeSandboxNotReady        ErrorCode = "sandbox_not_ready"
	CodeSandboxSuspended       ErrorCode = "sandbox_suspended"
	CodeRunConflict            ErrorCode = "run_conflict"
	CodeSignalLimitExceeded    ErrorCode = "signal_limit_exceeded"
	CodeRunNotReadyForStdin    ErrorCode = "run_not_ready_for_stdin"
	CodeRunNotAcceptingStdin   ErrorCode = "run_not_accepting_stdin"
	CodeStdinOffsetMismatch    ErrorCode = "stdin_offset_mismatch"
	CodeStdinOutcomeUnknown    ErrorCode = "stdin_delivery_outcome_unknown"
	CodeIdempotencyKeyConflict ErrorCode = "idempotency_key_conflict"
	CodeIdempotencyKeyInFlight ErrorCode = "idempotency_key_in_flight"
	CodeInvalidStateTransition ErrorCode = "invalid_state_transition"
	CodeCursorTooOld           ErrorCode = "cursor_too_old"
	CodeProfileRevUnavailable  ErrorCode = "profile_revision_unavailable"
	CodePayloadTooLarge        ErrorCode = "payload_too_large"
	CodeEnvRejected            ErrorCode = "env_rejected"
	CodeRateLimited            ErrorCode = "rate_limited"
	CodeQuotaExceeded          ErrorCode = "quota_exceeded"
	CodeInternal               ErrorCode = "internal"
	CodeDependencyUnavailable  ErrorCode = "dependency_unavailable"
)

// wireError is the contract's Error object. `retryable` is fixed from `code` by
// the schema rather than chosen independently, so this client reads the code and
// treats the flag as corroboration.
type wireError struct {
	Code              ErrorCode      `json:"code"`
	Message           string         `json:"message"`
	Retryable         bool           `json:"retryable"`
	RetryAfterSeconds *int           `json:"retry_after_seconds"`
	RequestID         string         `json:"request_id"`
	Details           map[string]any `json:"details"`
}

type errorEnvelope struct {
	Error wireError `json:"error"`
}

// Profile is the approved-profile catalogue entry, reduced to what readiness
// needs: that the named profile exists, is provisionable, and which revision it
// currently pins.
type Profile struct {
	ProfileID       string        `json:"profile_id"`
	ProfileRevision string        `json:"profile_revision"`
	DisplayName     string        `json:"display_name"`
	Status          string        `json:"status"`
	Limits          ProfileLimits `json:"limits"`
}

// ProfileLimits is the part of the profile's run envelope this client shapes
// its own requests by: how a prompt may be delivered on stdin. The rest of the
// envelope — output, argv, environment, timeouts — is enforced by the server
// and echoed in the run's resolved spec, so nothing here mirrors it.
//
// Every value is pinned into the profile revision a sandbox is created against
// (#284): an operator cannot loosen or tighten one without the pin moving, so
// what Ready reads is what every run in a sandbox pinned to that revision will
// be judged by.
type ProfileLimits struct {
	// MaxStdinInlineBytes caps `stdin.inline_b64` at startRun, decoded. A zero
	// forbids inline stdin entirely; the schema permits it.
	MaxStdinInlineBytes int64 `json:"max_stdin_inline_bytes"`
	// MaxStdinChunkBytes caps one streaming write, decoded.
	MaxStdinChunkBytes int64 `json:"max_stdin_chunk_bytes"`
	// MaxStdinTotalBytes caps the whole of a run's stdin across every write. The
	// server reads zero as unbounded, and so does this client.
	MaxStdinTotalBytes int64 `json:"max_stdin_total_bytes"`
	// MaxRequestBodyBytes caps a whole request body, enforced before the server
	// parses it. It binds stdin twice over: an inline prompt travels base64
	// encoded inside JSON, so a body can exceed this while the decoded prompt
	// is under the inline bound, and each streaming write is a body of its own.
	MaxRequestBodyBytes int64 `json:"max_request_body_bytes"`
}

// Provisionable reports the two statuses createSandbox accepts. `withdrawn` is
// the third and is refused, which is what makes an operator's withdrawal of a
// profile visible at daemon readiness rather than at the first claim.
func (p Profile) Provisionable() bool {
	return p.Status == "approved" || p.Status == "deprecated"
}

// Principal is the sandbox owner Airlock derived from validated token claims.
// Recorded at creation and immutable; BEN compares it and never sends it.
type Principal struct {
	TenantID string `json:"tenant_id"`
	Subject  string `json:"subject"`
	ClientID string `json:"client_id"`
}

// SandboxDeletion is the independent evidence that a deletion actually
// happened. A sandbox reaches `deleted` if and only if all three are confirmed,
// which is why BEN reads them rather than the state alone.
type SandboxDeletion struct {
	ComputeReleased  Evidence `json:"compute_released"`
	VolumeDestroyed  Evidence `json:"volume_destroyed"`
	RecordTombstoned Evidence `json:"record_tombstoned"`
}

// Confirmed is the conjunction, stated once so no caller spells it as two of
// the three.
func (d SandboxDeletion) Confirmed() bool {
	return d.ComputeReleased.Confirmed() && d.VolumeDestroyed.Confirmed() && d.RecordTombstoned.Confirmed()
}

// Sandbox is the durable sandbox record.
type Sandbox struct {
	SandboxID       string            `json:"sandbox_id"`
	Owner           Principal         `json:"owner"`
	State           SandboxState      `json:"state"`
	ProfileID       string            `json:"profile_id"`
	ProfileRevision string            `json:"profile_revision"`
	WorkspaceID     string            `json:"workspace_id"`
	ActiveRunID     *string           `json:"active_run_id"`
	Labels          map[string]string `json:"labels"`
	Deletion        *SandboxDeletion  `json:"deletion"`
}

// RunTermination is the contract's three independent facts, which map one to
// one onto remote's StreamState, ProcessState and DomainState.
type RunTermination struct {
	Reason        TerminationReason `json:"reason"`
	StreamSealed  Evidence          `json:"stream_sealed"`
	ProcessReaped Evidence          `json:"process_reaped"`
	DomainQuiet   Evidence          `json:"domain_quiet"`
	Detail        *string           `json:"detail"`
}

// RunEventWindow is what of the event log is still readable.
type RunEventWindow struct {
	LatestSeq          int64 `json:"latest_seq"`
	OldestAvailableSeq int64 `json:"oldest_available_seq"`
}

// Run is the durable run record, reduced to what remote.Status needs plus the
// exit facts a hook result reads.
type Run struct {
	RunID       string            `json:"run_id"`
	SandboxID   string            `json:"sandbox_id"`
	State       RunState          `json:"state"`
	Termination RunTermination    `json:"termination"`
	Events      RunEventWindow    `json:"events"`
	ExitCode    *int              `json:"exit_code"`
	Signal      *string           `json:"signal"`
	Labels      map[string]string `json:"labels"`
}

// runEvent is one sequenced event, decoded far enough to route it. Payload
// bytes stay base64 until framing, because a chunk is opaque: it may split a
// line, a JSON object, or a UTF-8 rune, and only the provider adapter's own
// translator is entitled to read one (remote.Translator).
type runEvent struct {
	Seq     int64        `json:"seq"`
	Kind    EventKind    `json:"kind"`
	RunID   string       `json:"run_id"`
	Stream  OutputStream `json:"stream"`
	DataB64 string       `json:"data_b64"`
}

// runEventPage is one JSON page of events.
type runEventPage struct {
	Events             []json.RawMessage `json:"events"`
	Cursor             int64             `json:"cursor"`
	LatestSeq          int64             `json:"latest_seq"`
	OldestAvailableSeq int64             `json:"oldest_available_seq"`
	HasMore            bool              `json:"has_more"`
}

// createSandboxRequest is the createSandbox body.
//
// It carries no profile_revision, deliberately. The body is the idempotency
// fingerprint, so a reattach that pinned the revision it had learned would
// fingerprint differently from the create that learned it and collide with its
// own key. BEN pins by *comparison* instead: the revision comes back in the
// response and Workspaces refuses one that moved (ErrProfileRevision), which is
// the same guarantee without making the key depend on a fact discovered after
// the key was chosen.
type createSandboxRequest struct {
	ProfileID               string            `json:"profile_id"`
	Labels                  map[string]string `json:"labels,omitempty"`
	IdleSuspendAfterSeconds *int              `json:"idle_suspend_after_seconds,omitempty"`
	DeleteAfterIdleSeconds  *int              `json:"delete_after_idle_seconds,omitempty"`
}

type runTimeoutsRequest struct {
	HardSeconds        *int `json:"hard_seconds,omitempty"`
	OutputStallSeconds *int `json:"output_stall_seconds,omitempty"`
}

type runStdinRequest struct {
	Mode      StdinMode `json:"mode"`
	InlineB64 string    `json:"inline_b64,omitempty"`
}

type startRunRequest struct {
	Argv     []string            `json:"argv"`
	Env      map[string]string   `json:"env,omitempty"`
	Stdin    *runStdinRequest    `json:"stdin,omitempty"`
	Timeouts *runTimeoutsRequest `json:"timeouts,omitempty"`
	Labels   map[string]string   `json:"labels,omitempty"`
}

type signalRequest struct {
	Signal       Signal `json:"signal"`
	GraceSeconds *int   `json:"grace_seconds,omitempty"`
}

type signalResponse struct {
	SignalID    *string        `json:"signal_id"`
	RunID       string         `json:"run_id"`
	State       RunState       `json:"state"`
	Termination RunTermination `json:"termination"`
}

type waitForRunRequest struct {
	WaitSeconds int `json:"wait_seconds"`
}

type writeStdinRequest struct {
	Offset  int64  `json:"offset"`
	DataB64 string `json:"data_b64"`
	Close   bool   `json:"close,omitempty"`
}

type writeStdinResponse struct {
	AcceptedBytes int64 `json:"accepted_bytes"`
	NextOffset    int64 `json:"next_offset"`
	Closed        bool  `json:"closed"`
}
