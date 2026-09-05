package remote

import (
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Record is the durable BEN state one remote attempt needs in order to be picked
// up by a *different process* than the one that wrote it.
//
// It is deliberately not a database and deliberately not authority. The tracker
// and git remain the source of truth (SPEC §9.10), and everything here is an
// address, a durable event position, or the non-secret input needed to recover
// that address. A daemon that finds this file missing has lost the ability to
// *attach*, not the ability to be correct: it reconciles from the tracker as it
// always did, and a run it cannot name parks rather than being dispatched twice.
//
// The encoding is JSON with explicit tags because it is read by a later binary
// that may be older or newer than the writer — the same rolling-upgrade argument
// state.ReadRuns makes for not setting DisallowUnknownFields.
type Record struct {
	// Version is the record format. Present so an older reader refuses a newer
	// record loudly instead of attaching to a run it half understands: an
	// address it misreads is a duplicate dispatch, which is the one outcome
	// this whole file exists to prevent.
	Version int `json:"version"`

	// Identity is the workspace this attempt runs in, sandbox id and immutable
	// profile revision included. Written before any start (Journal.Reserve).
	Identity Identity `json:"identity"`

	// RunID is BEN's externally persisted run identity — the field whose
	// existence *before* a dispatch is the whole recovery story (see RunID).
	RunID RunID `json:"run_id"`
	// RequestDigest pins the exact ProcessSpec accepted under RunID. Together
	// with Identity it forms ProcessRef, the address every backend call uses.
	RequestDigest string `json:"request_digest"`
	// Dispatched records that a start has been attempted for RunID. A restart
	// that finds it set attaches; one that finds it clear may dispatch. It is
	// set before the call and never after it, because "the start returned" is
	// not knowable from a process that died during it.
	Dispatched bool `json:"dispatched"`
	// BackendRunID is the backend's durable resource identifier, learned from a
	// successful Start response and required for later attachment. ProcessRef
	// still pins the BEN request identity and guards every operation against a
	// mismatched sandbox or request.
	BackendRunID string `json:"backend_run_id,omitempty"`

	// Replay is the non-secret, orchestrator-owned RunSpec needed to rebuild an
	// unanswered Start after a daemon restart. It deliberately stops before the
	// provider invocation: API keys and provider environment are recomposed from
	// the current binding, and the recorded RequestDigest refuses the replay if
	// any of those bytes changed. Journal drops it as soon as BackendRunID lands.
	Replay *ReplaySpec `json:"replay,omitempty"`

	// Cursor is the latest *committed* backend event sequence: everything at or
	// below it has been durably consumed and translated. A restart resumes the
	// event stream after it, replaying whatever was in flight (Sequencer).
	Cursor int64 `json:"cursor"`
	// DecoderTail is the unterminated provider stdout line at Cursor. It is
	// replaced in the same write as Cursor so arbitrary backend chunks decode
	// identically after a restart.
	DecoderTail []byte `json:"decoder_tail,omitempty"`
	// Terminal records that the durable consumer accepted a normalized outcome.
	// Recovery may keep draining raw bytes, but it must never synthesize or
	// publish a second outcome.
	Terminal bool `json:"terminal,omitempty"`

	// TemplateRevision identifies the prompt template this attempt rendered
	// from, and PromptDigest the bytes it actually sent.
	//
	// Both, because they answer different questions and only one of them is
	// stable. The revision says which template a reload had installed when the
	// claim was taken (SPEC §5.4: a reload never changes a live run's ground);
	// the digest says what the agent was told, which §9.5 requires be answerable
	// after the fact. Replay temporarily carries those bytes for recovery; this
	// digest remains the stable, compact identity compared by diagnostics.
	TemplateRevision string `json:"template_revision,omitempty"`
	PromptDigest     string `json:"prompt_digest,omitempty"`

	// Provider and Model are core.AgentDescriptor's two terms, carried for the
	// same reason it carries them: an attempt-outcome record has to say which
	// agent produced it (#60, #62). An empty Model is an answer — the block
	// named none and the harness's own default applied — not a gap.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// Transcript is where the raw stream for this run was retained
	// (SPEC §10.3). A reference rather than the bytes: the transcript is
	// megabytes and this file is rewritten on every cursor commit.
	Transcript string `json:"transcript,omitempty"`
}

// ReplaySpec is the JSON-stable projection of core.RunSpec retained only while
// a dispatched run has no permanent backend id. Its prompt is the same
// canonical prompt §9.5 already requires the state directory to retain; it
// carries no provider configuration or credential value.
type ReplaySpec struct {
	Workspace    ReplayWorkspace   `json:"workspace"`
	Prompt       string            `json:"prompt"`
	Continuation string            `json:"continuation,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Limits       ReplayLimits      `json:"limits"`
}

type ReplayWorkspace struct {
	Path         string `json:"path"`
	SharedGitDir string `json:"shared_git_dir,omitempty"`
	PrivateDir   string `json:"private_dir,omitempty"`
}

type ReplayLimits struct {
	StallTimeout   time.Duration `json:"stall_timeout_ns"`
	AttemptTimeout time.Duration `json:"attempt_timeout_ns"`
	MaxTurns       int           `json:"max_turns"`
	MaxCostUSD     float64       `json:"max_cost_usd"`
}

func replaySpecOf(spec core.RunSpec) *ReplaySpec {
	return &ReplaySpec{
		Workspace: ReplayWorkspace{
			Path: spec.Workspace.Path, SharedGitDir: spec.Workspace.SharedGitDir,
			PrivateDir: spec.Workspace.PrivateDir,
		},
		Prompt: spec.Prompt, Continuation: spec.Continuation, Env: cloneReplayEnv(spec.Env),
		Limits: ReplayLimits{
			StallTimeout: spec.Limits.StallTimeout, AttemptTimeout: spec.Limits.AttemptTimeout,
			MaxTurns: spec.Limits.MaxTurns, MaxCostUSD: spec.Limits.MaxCostUSD,
		},
	}
}

func (r ReplaySpec) runSpec() core.RunSpec {
	return core.RunSpec{
		Workspace: core.WorkspacePaths{
			Path: r.Workspace.Path, SharedGitDir: r.Workspace.SharedGitDir,
			PrivateDir: r.Workspace.PrivateDir,
		},
		Prompt: r.Prompt, Continuation: r.Continuation, Env: cloneReplayEnv(r.Env),
		Limits: core.RunLimits{
			StallTimeout: r.Limits.StallTimeout, AttemptTimeout: r.Limits.AttemptTimeout,
			MaxTurns: r.Limits.MaxTurns, MaxCostUSD: r.Limits.MaxCostUSD,
		},
	}
}

func cloneReplay(r *ReplaySpec) *ReplaySpec {
	if r == nil {
		return nil
	}
	clone := *r
	clone.Env = cloneReplayEnv(r.Env)
	return &clone
}

func cloneReplayEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	return maps.Clone(env)
}

// RecordVersion is the current Record.Version.
const RecordVersion = 3

// ProcessRef reconstructs the exact backend address persisted before dispatch.
func (r Record) ProcessRef() ProcessRef {
	return ProcessRef{Identity: r.Identity, RunID: r.RunID, RequestDigest: r.RequestDigest}
}

// EncodeRecord renders a record for durable storage.
func EncodeRecord(r Record) ([]byte, error) {
	if r.Version == 0 {
		r.Version = RecordVersion
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("remote: encoding record for %s: %w", r.Identity.Claim, err)
	}
	return append(body, '\n'), nil
}

// DecodeRecord reads a record back.
//
// A record from a future version is refused rather than best-effort decoded, for
// the reason Version gives: this file is an address, and attaching to a
// half-understood one dispatches twice. That is the opposite call from
// state.ReadRuns, which renders forensics for a human and should degrade; here a
// wrong answer is an action.
func DecodeRecord(body []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("remote: decoding record: %w", err)
	}
	if r.Version > RecordVersion {
		return Record{}, fmt.Errorf("remote: record is version %d and this binary understands %d: "+
			"refusing to attach to a run it may address wrongly", r.Version, RecordVersion)
	}
	return r, nil
}
