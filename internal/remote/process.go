package remote

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// RunID is BEN's name for one foreign process. It is unique only within the
// sandbox named by ProcessRef; a backend must never treat it as a global key.
type RunID string

// Cursor is the last backend event accepted by BEN's durable consumer.
type Cursor int64

// Stream identifies the byte stream carried by an output envelope. The zero
// value is control data: it is retained by the consumer but never handed to a
// provider line translator.
type Stream uint8

const (
	StreamControl Stream = iota
	StreamStdout
	StreamStderr
)

func (s Stream) String() string {
	switch s {
	case StreamControl:
		return "control"
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "Stream(" + strconv.Itoa(int(s)) + ")"
	}
}

// Envelope is one durable backend event. Payload is an arbitrary byte chunk,
// not a provider record: a chunk may split a JSON line or contain several.
type Envelope struct {
	Seq     int64
	Stream  Stream
	Payload []byte
	// Truncated marks the backend's durable statement that output bytes were
	// dropped. It is orthogonal to Stream: the event itself is control data, but
	// any consumer deriving a verdict from process output must refuse it.
	Truncated bool
}

// Phase is a coarse backend phase kept for diagnostics. It is deliberately not
// lifecycle evidence: stream closure, process reaping and domain quiet are
// independent facts in Status.
type Phase uint8

const (
	PhaseUnknown Phase = iota
	PhaseStarting
	PhaseRunning
	PhaseSignaled
	PhaseQuiet
)

func (p Phase) String() string {
	switch p {
	case PhaseUnknown:
		return "unknown"
	case PhaseStarting:
		return "starting"
	case PhaseRunning:
		return "running"
	case PhaseSignaled:
		return "signaled"
	case PhaseQuiet:
		return "quiet"
	default:
		return "Phase(" + strconv.Itoa(int(p)) + ")"
	}
}

// StreamState, ProcessState and DomainState are separate because none implies
// either of the others. Their zero values authorize nothing.
type StreamState uint8

const (
	StreamStateUnknown StreamState = iota
	StreamStateOpen
	StreamStateSealed
)

func (s StreamState) String() string {
	switch s {
	case StreamStateUnknown:
		return "unknown"
	case StreamStateOpen:
		return "open"
	case StreamStateSealed:
		return "sealed"
	default:
		return "StreamState(" + strconv.Itoa(int(s)) + ")"
	}
}

type ProcessState uint8

const (
	ProcessStateUnknown ProcessState = iota
	ProcessStateRunning
	ProcessStateReaped
)

func (s ProcessState) String() string {
	switch s {
	case ProcessStateUnknown:
		return "unknown"
	case ProcessStateRunning:
		return "running"
	case ProcessStateReaped:
		return "reaped"
	default:
		return "ProcessState(" + strconv.Itoa(int(s)) + ")"
	}
}

type DomainState uint8

const (
	DomainStateUnknown DomainState = iota
	DomainStateActive
	DomainStateQuiet
)

func (s DomainState) String() string {
	switch s {
	case DomainStateUnknown:
		return "unknown"
	case DomainStateActive:
		return "active"
	case DomainStateQuiet:
		return "quiet"
	default:
		return "DomainState(" + strconv.Itoa(int(s)) + ")"
	}
}

// Status is one fresh observation of a backend run.
type Status struct {
	Phase        Phase
	Stream       StreamState
	Process      ProcessState
	Domain       DomainState
	BackendRunID string
	Reachable    bool
}

// Termination confirms workspace reuse only from an explicit domain-quiet fact.
func (s Status) Termination() core.Termination {
	if s.Reachable && s.Domain == DomainStateQuiet {
		return core.TerminationConfirmed
	}
	return core.TerminationUnconfirmed
}

// Reaped reports the narrower fact core.RunHandle.Done represents.
func (s Status) Reaped() bool {
	return s.Reachable && s.Process == ProcessStateReaped
}

// ProcessRef is the durable, sandbox-scoped address of one exact dispatch.
// RequestDigest is over ProcessSpec, so replaying an id with different argv,
// input, limits, or workspace is a conflict rather than an accidental attach.
type ProcessRef struct {
	Identity      Identity `json:"identity"`
	RunID         RunID    `json:"run_id"`
	RequestDigest string   `json:"request_digest"`
}

func (r ProcessRef) Complete() bool {
	return r.Identity.Complete() && r.RunID != "" && r.RequestDigest != ""
}

func (r ProcessRef) String() string {
	return r.Identity.SandboxID + "/" + string(r.RunID) + "@" + r.RequestDigest
}

// ProcessSpec is the immutable request a backend executes inside Identity.
// Limits cross this boundary because the backend must enforce both deadlines
// while BEN is disconnected.
type ProcessSpec struct {
	Identity Identity
	Argv     []string
	Env      map[string]string
	Stdin    []byte
	Limits   core.RunLimits
	Git      GitScope
}

// GitPhase is why a process may use the Airlock-owned Git capability. The
// zero value grants nothing; backends that do not implement the capability may
// ignore an empty scope, but must never infer one from argv or environment.
type GitPhase string

const (
	GitPhasePrepare GitPhase = "prepare"
	GitPhaseCoding  GitPhase = "coding"
	GitPhasePublish GitPhase = "publish"
	GitPhaseReview  GitPhase = "review"
)

// GitScope is trusted orchestration metadata attached to a durable process.
// It is deliberately absent from provider invocations: model-authored code can
// neither choose nor widen these fields. The Airlock backend translates the
// scope into control-plane-owned active-run labels.
type GitScope struct {
	Phase          GitPhase
	Repository     string
	Branch         string
	BaseCommit     string
	BaseBranch     string
	CheckoutCommit string
	Operation      string
}

func (s GitScope) Empty() bool { return s == (GitScope{}) }

// Validate checks the closed phase-dependent shape before a request reaches a
// backend. Airlock applies the authoritative repository/ref validation again;
// these checks keep an incomplete BEN binding from becoming a remote run.
func (s GitScope) Validate() error {
	if s.Empty() {
		return nil
	}
	if s.Repository == "" || s.Branch == "" || s.BaseBranch == "" || !fullSHA(s.BaseCommit) {
		return fmt.Errorf("remote: incomplete Git scope")
	}
	switch s.Phase {
	case GitPhasePrepare:
		if !fullSHA(s.CheckoutCommit) || s.Operation != "" {
			return fmt.Errorf("remote: invalid prepare Git scope")
		}
	case GitPhasePublish:
		if s.CheckoutCommit != "" || s.Operation == "" {
			return fmt.Errorf("remote: invalid publish Git scope")
		}
	case GitPhaseCoding, GitPhaseReview:
		if s.CheckoutCommit != "" || s.Operation != "" {
			return fmt.Errorf("remote: invalid %s Git scope", s.Phase)
		}
	default:
		return fmt.Errorf("remote: invalid Git phase %q", s.Phase)
	}
	return nil
}

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// ProcessRequestDigest returns the canonical identity of a dispatch request.
// encoding/json sorts string map keys, so Env insertion order cannot change it.
func ProcessRequestDigest(spec ProcessSpec) (string, error) {
	digest, err := marshalRequestDigest(processRequestDigestPayload(spec))
	if err != nil {
		return "", fmt.Errorf("remote: encoding process request: %w", err)
	}
	return digest, nil
}

// StopRequest preserves the orchestrator's two stop meanings at the backend.
// Interrupt is the patient TERM/grace/KILL ladder; discard may kill immediately.
type StopRequest struct {
	Mode  core.StopMode
	Grace time.Duration
}

// ProcessBackend is the durable foreign-process boundary. Every operation is
// scoped by the same ProcessRef and must reject a mismatched request digest.
//
// Its three absence outcomes are deliberately not interchangeable:
//
//   - ErrNoProcess: Start never crossed the backend's durable acceptance fence;
//   - ErrProcessUnresolved: Start crossed that fence, but its result is unknown;
//   - ErrProcessUnavailable: an accepted run's permanent id is no longer usable.
//
// Only the first means no process was started. Neither of the latter two is
// lifecycle evidence, and neither may authorize reuse or disposal.
type ProcessBackend interface {
	// Start is idempotent for the exact ref and request. Same address/different
	// request returns ErrProcessMismatch. An error may be ambiguous; callers
	// resolve an unknown result by replaying this exact call. A backend-generated
	// run id may exist only in the missing response; a backend may also recover
	// it from an authoritative, request-bound resource observation.
	Start(ctx context.Context, ref ProcessRef, spec ProcessSpec) (Status, error)
	// Attach addresses a run by the backend id returned by a successful Start.
	// The id is required deliberately: ProcessRef is an idempotency address, not
	// a permanent backend resource name, and cannot resolve an unknown Start.
	Attach(ctx context.Context, ref ProcessRef, backendRunID string) (Status, error)
	Status(ctx context.Context, ref ProcessRef) (Status, error)
	// Events returns durable envelopes after the cursor. An empty successful
	// result means only that the event stream is sealed; it says nothing about
	// process reaping or domain quiet.
	Events(ctx context.Context, ref ProcessRef, after Cursor) ([]Envelope, error)
	Stdin(ctx context.Context, ref ProcessRef, data []byte) error
	// Stop performs the mode-specific backend ladder and returns a fresh status.
	Stop(ctx context.Context, ref ProcessRef, req StopRequest) (Status, error)
	// Wait blocks until the direct process is reaped or ctx ends. It must not
	// wait for descendants/domain quiet, which is a separate workspace fact.
	Wait(ctx context.Context, ref ProcessRef) (Status, error)
}

// StartResolver reconstructs and replays one exact unanswered Start from
// daemon-side durable state. Backends whose own resource model can discover a
// permanent run id use this only when that narrower recovery cannot answer.
type StartResolver func(context.Context, ProcessRef) (Status, error)

// Translator parses one complete provider stdout line into normalized events.
// Framing arbitrary backend chunks into lines belongs to Attempt, before this
// provider-owned boundary.
type Translator func(line []byte) []core.Event
