package reviewrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// RecordVersion is the current Record.Version. An older reader refuses a newer
// record loudly rather than acting on half of it, for remote.Record's reason:
// this file is an address, and a misread address resolves the wrong run.
const RecordVersion = 2

// MaxOutput bounds what one record may retain.
//
// The verdict is a small JSON object and the findings prose is already capped
// at publication (review.ReviewBody), so this is generous by two orders of
// magnitude for a well-behaved reviewer and is not a tuning knob. What it is
// for is the badly-behaved one: a model looping on a large diff can emit output
// without limit, and a daemon that grew its state directory to match would
// eventually be the outage. Exceeding it is ErrOutputOverflow — a refusal
// rather than a truncation, because bytes that were dropped cannot be shown to
// contain exactly one verdict envelope.
const MaxOutput = 4 << 20

// Record is one review run's durable execution state.
//
// One record rather than several files, and replaced whole by rename, for
// remotews.Cycle's reason: the cursor and the bytes it admits are read together
// at every decision, and a reader that saw one half updated would resume from a
// position it cannot reconstruct a verdict from. A torn record is worse than
// none, because none is a verdict of its own — nothing was dispatched.
type Record struct {
	Version int    `json:"version"`
	Run     string `json:"run"`
	// Cycle is the workspace-cycle address (Subject.CycleAddress). Records are
	// grouped by it so the quiet gate can ask whether any *other* run in this
	// sandbox is still going.
	Cycle string `json:"cycle"`
	Role  string `json:"role"`

	Repository      string `json:"repository"`
	Issue           string `json:"issue"`
	Approval        int64  `json:"approval"`
	Occurrence      int64  `json:"occurrence"`
	Claim           int64  `json:"claim"`
	PR              int    `json:"pr"`
	Base            string `json:"base"`
	Head            string `json:"head"`
	ReviewerProfile string `json:"reviewer_profile,omitempty"`

	// Branch, BaseSHA and TargetBranch are the publication branch, trusted base,
	// and claim-scoped pull-request target. Empty under the local executor,
	// which acquires nothing.
	Branch       string `json:"branch,omitempty"`
	BaseSHA      string `json:"base_sha,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	// Sandbox and Profile pin where this run executes. Empty under the local
	// executor, which has neither.
	Sandbox string `json:"sandbox,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Digest is the canonical request digest; Subject is the whole-subject
	// digest including the diff bytes. Both are compared on every resume.
	Digest  string `json:"digest"`
	Subject string `json:"subject"`

	// Dispatched is written *before* a start is attempted. A process that dies
	// inside the launch window leaves a record that says a dispatch was
	// attempted, which is the only reading that cannot produce a second run.
	Dispatched bool `json:"dispatched"`
	// BackendRunID is the durable resource handle a successful start returned.
	BackendRunID string `json:"backend_run_id,omitempty"`

	// Cursor is the highest admitted sequence; Output is every stdout byte up to
	// it, in order; Admitted is the per-sequence stream/payload digest replay is
	// checked against. Stderr and control events advance Cursor without entering
	// Output. The three move in one replacement.
	Cursor   int64            `json:"cursor"`
	Output   []byte           `json:"output,omitempty"`
	Admitted map[int64]string `json:"admitted,omitempty"`

	// Sealed records that the executor reported the output stream complete;
	// Verdict is the validated closed verdict extracted from it. Once Verdict is
	// set the run is terminal and no further execution is attempted for this
	// identity — which is what makes a crash between the verdict and its
	// publication resume rather than re-review.
	Sealed  bool   `json:"sealed"`
	Verdict string `json:"verdict,omitempty"`
	// Findings is the model's prose, retained so a resumed publication does not
	// need the run to still exist. Untrusted: it is sanitized at publication
	// (review.SanitizeFindings), never here.
	Findings string `json:"findings,omitempty"`
	// Quiet records a positive domain-quiet observation. It is the only thing
	// that lets another run start in this sandbox (SPEC §9.8).
	Quiet bool `json:"quiet"`

	// Refusal is the executor's definite statement that this dispatch was never
	// admitted and would not be as composed (#284). It is the durable answer a
	// later sweep reads instead of replaying an "unresolved" start, and the one
	// thing about the record an operator has to act on. A refused record holds
	// no run: it is Quiet, has no BackendRunID, and is superseded rather than
	// refused when the subject is recomposed.
	Refusal *Refusal `json:"refusal,omitempty"`
}

// Refusal is one definite admission refusal, as retained on the record and
// surfaced on the issue.
type Refusal struct {
	// Reason is the executor's stable code — `payload_too_large`, say.
	Reason string `json:"reason"`
	// Detail is the executor's sanitized statement. Airlock's contract binds it
	// to carry no stdin bytes, environment values or argv beyond argv[0].
	Detail string `json:"detail,omitempty"`
}

// Ref is the durable address this record names.
func (r Record) Ref() Ref {
	return Ref{
		Run: r.Run, Repository: r.Repository, Issue: r.Issue, Cycle: r.Approval,
		Branch: r.Branch, BaseSHA: r.BaseSHA, TargetBranch: r.TargetBranch,
		Sandbox: r.Sandbox, Profile: r.Profile, Digest: r.Digest,
	}
}

// Terminal reports a run that has stated a verdict. Sealing alone is not
// terminal: a sealed stream with no envelope in it is a run that finished and
// said nothing, which authorizes no routing and is not a fact to cache.
func (r Record) Terminal() bool { return r.Verdict != "" }

// Refused reports a dispatch the executor definitively declined. Not terminal:
// a refusal states nothing about the subject, and the same identity is
// re-offered on a later sweep in case the executor can now admit it.
func (r Record) Refused() bool { return r.Refusal != nil }

// Report projects the accepted verdict back onto internal/review's type.
func (r Record) Report() (verdict, findings string) { return r.Verdict, r.Findings }

// validate refuses a record this package could not have written. Every clause
// is a shape that would otherwise be acted on.
func (r Record) validate() error {
	switch {
	case r.Version != RecordVersion:
		return fmt.Errorf("%w: unsupported version %d", ErrRecordState, r.Version)
	case r.Run == "" || r.Cycle == "" || r.Role == "" || r.Digest == "" || r.Subject == "":
		return fmt.Errorf("%w: record %q is not fully named", ErrRecordState, r.Run)
	case r.Repository == "" || r.Issue == "":
		return fmt.Errorf("%w: record %q names no repository or issue", ErrRecordState, r.Run)
	case r.Approval <= 0 || r.Occurrence <= 0 || r.Claim <= 0 || r.PR <= 0:
		return fmt.Errorf("%w: record %q carries a non-positive identity field", ErrRecordState, r.Run)
	case !isFullSHA(r.Base) || !isFullSHA(r.Head):
		return fmt.Errorf("%w: record %q does not name both diff endpoints as commit shas", ErrRecordState, r.Run)
	case r.ReviewerProfile != "" && !review.ValidReviewerProfile(r.ReviewerProfile):
		return fmt.Errorf("%w: record %q names invalid reviewer profile %q", ErrRecordState, r.Run, r.ReviewerProfile)
	case r.Sandbox != "" && r.TargetBranch == "":
		return fmt.Errorf("%w: remote record %q names no claim-scoped target branch", ErrRecordState, r.Run)
	case r.Cursor < 0:
		return fmt.Errorf("%w: record %q has a negative cursor", ErrRecordState, r.Run)
	case r.BackendRunID != "" && !r.Dispatched:
		return fmt.Errorf("%w: record %q names a backend run it never dispatched", ErrRecordState, r.Run)
	case r.Verdict != "" && !r.Sealed:
		return fmt.Errorf("%w: record %q states a verdict from an unsealed stream", ErrRecordState, r.Run)
	case r.Verdict != "" && !r.Quiet:
		return fmt.Errorf("%w: record %q states a verdict before execution-domain quiet", ErrRecordState, r.Run)
	case r.Refusal != nil && !r.Dispatched:
		return fmt.Errorf("%w: record %q records a refusal of a dispatch it never attempted", ErrRecordState, r.Run)
	case r.Refusal != nil && (r.BackendRunID != "" || r.Verdict != ""):
		return fmt.Errorf("%w: record %q records a refusal beside a run that exists", ErrRecordState, r.Run)
	case r.Refusal != nil && r.Refusal.Reason == "":
		return fmt.Errorf("%w: record %q records a refusal with no reason", ErrRecordState, r.Run)
	case len(r.Output) > MaxOutput:
		return fmt.Errorf("%w: record %q retains %d bytes", ErrOutputOverflow, r.Run, len(r.Output))
	}
	return nil
}

// chunkDigest is the per-sequence payload identity replay is compared against.
func chunkDigest(chunk Chunk) string {
	h := sha256.New()
	h.Write([]byte{byte(chunk.Stream)})
	if chunk.Truncated {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(chunk.Payload)
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum)
}
