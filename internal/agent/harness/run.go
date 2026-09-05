package harness

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Translator maps one raw harness line to zero or more normalized events
// (SPEC §7.2). A nil or empty result means "no normalized meaning": the caller
// still counts the line as activity and synthesizes a heartbeat, so an
// unrecognized or future line kind cannot make a healthy run look dead.
//
// It returns no error by design. A malformed line is the harness's business,
// and refusing to parse one must not end a run that is otherwise progressing.
type Translator func(line []byte) []core.Event

// Launch is one attempt's process, fully described. Every field is the
// adapter's decision; what happens to the process afterwards is this package's.
type Launch struct {
	// Argv[0] is the resolved absolute binary (see ResolveBinary). Secrets are
	// never part of argv, which is world-readable through ps (SPEC §7.6).
	Argv []string
	// Env is the complete child environment (see Environ). Nothing is inherited
	// beyond what it names.
	Env []string
	// Dir is the workspace the process runs in — the only thing keeping the
	// agent inside its worktree.
	Dir string
	// Prompt is delivered on stdin, never argv (SPEC §7.6).
	Prompt string
	// Limits are the runner-enforced liveness windows (SPEC §7.4).
	Limits core.RunLimits
	// Translate turns the harness's stream into the closed event enum.
	Translate Translator
	// Transcript retains the raw stream verbatim (SPEC §7.2, §10.3). Start
	// takes ownership: it is closed when the run ends, and on a failed launch.
	Transcript io.WriteCloser
	// PromptSink retains the canonical rendered prompt for this attempt
	// (SPEC §9.5). Start writes Prompt to it and closes it before launching;
	// nil discards the retention.
	//
	// A sink rather than a path, and written here rather than by the store that
	// opened it, because the bytes are subject to Redact and Redact lives here.
	// A store that wrote the prompt itself would be a second place holding the
	// credential values — and the first version of this shipped exactly that, so
	// a credential the transcript scrubs survived in the file beside it.
	PromptSink io.WriteCloser
	// Redact are credential values that must not reach the transcript. Both
	// adapters build it from two provenances: CredentialValues over the provider
	// block they were constructed with, so a reload that rotates a credential
	// builds a new runner (SPEC §7.1) and an in-flight run keeps the set it
	// launched with; plus the env_passthrough values Environ resolved for *this*
	// attempt, which no declaration holds and which the daemon's environment can
	// change between attempts.
	//
	// The raw stream is deliberately not redacted on its way to translation:
	// readStdout hands the pump the same line it writes to the transcript, so
	// translation, liveness and terminal classification see exactly the bytes they
	// saw before. Redaction cannot change how a run is judged.
	//
	// core.Event.Text is the one exception, redacted in handle.emit because the
	// orchestrator retains a bounded tail of it for the next attempt's prompt
	// (SPEC §9.6, #61). It is a *retained* field rather than a judged one — no
	// verdict reads it — so covering it leaves that invariant intact.
	Redact []string
	// Timings are the lifecycle windows this package enforces; each unset field
	// takes its DefaultTimings value.
	Timings Timings
	// Domain owns launch and termination. Production adapters use LocalDomain;
	// tests inject a contract fake that still drives real stream processes.
	Domain ExecutionDomain
	// Name prefixes launch errors, e.g. "codex-exec".
	Name string
	// OnRun records this run's evidence the moment the run exists, so a later
	// daemon can ask whether it is still going (SPEC §9.10). Nil skips the
	// upgrade — right for a probe or a test, wrong for the daemon, which would
	// leave every marker in the "launch outcome unknown" state that parks.
	//
	// It takes no run identity because a Launch *is* one launch: the adapter
	// binds its RunSpec into this closure when it builds the Launch, which is
	// what keeps core.RunEvidenceSink's per-workspace addressing intact without
	// this package needing to know what a workspace is.
	OnRun func(core.RunEvidence) error
}

// Start launches one attempt. An error here means no process and no handle: the
// orchestrator never has to reason about a half-started run. Once Start returns
// a handle, every outcome arrives as a terminal event instead (SPEC §7.4).
//
// Cancelling ctx after a successful Start discards the run — the daemon is
// shutting down — equivalent to Stop(StopDiscard).
func Start(ctx context.Context, l Launch) (core.RunHandle, error) {
	if l.Transcript == nil {
		l.Transcript = nopWriteCloser{}
	}
	// Before anything can write to it, and after the default: every sink is
	// covered, including the run-id-keyed store §10.3 will inject later.
	l.Transcript = redactTranscript(l.Transcript, l.Redact)
	// The retained prompt is the same kind of artifact and gets the same
	// treatment (SPEC §9.5, §10.3): 0600 on disk with no expiry, holding whatever
	// the render put in it. Written here rather than by the store, so there is
	// one place that knows the credential values and no sink can be added that
	// forgets to use them.
	if err := retainPrompt(l.PromptSink, l.Prompt, l.Redact); err != nil {
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: %w", l.Name, err)
	}
	l.Timings = l.Timings.withDefaults()
	if l.Domain == nil {
		l.Domain = LocalDomain()
	}

	// The output pipes are this package's, not cmd's. cmd.StdoutPipe would hand
	// ownership to Wait, which closes the read end the moment the process exits
	// — discarding anything still buffered, including the result line that is
	// the run's ground truth (SPEC §7.4). Owning them keeps the drain strictly
	// ahead of the decision (SPEC §7.5) and makes the post-exit bound ours to
	// set.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: stdin pipe: %w", l.Name, err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: stdout pipe: %w", l.Name, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: stderr pipe: %w", l.Name, err)
	}
	// A pipe rather than a temporary file: the prompt may contain credentials
	// and must never acquire another retained copy. Writing concurrently avoids
	// blocking Start on prompts larger than the pipe buffer while the provider is
	// still gated behind its durable-domain handshake.
	go func() {
		_, _ = io.WriteString(stdinW, l.Prompt)
		_ = stdinW.Close()
	}()

	run, err := l.Domain.Start(ctx, DomainLaunch{
		Argv: l.Argv, Env: l.Env, Dir: l.Dir,
		Stdin: stdinR, Stdout: stdoutW, Stderr: stderrW,
		OnDomain: l.OnRun, Timings: l.Timings,
	})
	// The execution domain owns duplicated child descriptors after Start. These
	// parent copies must close even when setup refused, releasing the prompt
	// writer and making provider/descendant ownership the only source of EOF.
	closeAll(stdinR, stdoutW, stderrW)
	if err != nil {
		closeAll(stdoutR, stderrR)
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: %w: %w", l.Name, ErrExecutionDomain, err)
	}

	h := newHandle(l, run, stdoutR, stderrR)
	h.run(ctx, l.Transcript)
	return h, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// IsTerminal reports whether an event ends a run (SPEC §7.2). Exactly one
// terminal event is delivered per attempt, and it is the last.
func IsTerminal(t core.EventType) bool {
	return t == core.EventSucceeded || t == core.EventFailed
}
