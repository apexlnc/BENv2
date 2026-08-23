package harness

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

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

// SignalFunc delivers sig to a process group. Injectable so a test can simulate
// a process the kernel will not kill for us.
type SignalFunc func(pgid int, sig syscall.Signal) error

// SignalGroup is the default sender, and the negation is the whole of it: SPEC
// §7.5's ladder addresses the process *group*, never the leader.
//
// Named and exported rather than inlined as the default, so a test that needs to
// *order* the ladder rather than withhold it can delay this exact call instead of
// respelling `-pgid` itself (agenttest's Options.Signal, #138). A stub that
// spelled the negation would be a fake carrying a guarantee the real sender does
// not make, and it would pass while signalling one process instead of a group.
func SignalGroup(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

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
	// Signal defaults to killing the real process group.
	Signal SignalFunc
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
	if l.Signal == nil {
		l.Signal = SignalGroup
	}

	cmd := exec.Command(l.Argv[0], l.Argv[1:]...)
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	cmd.Stdin = strings.NewReader(l.Prompt)
	// Own process group: Stop signals the whole group, so a harness that
	// spawned tools of its own — or, for a harness shipped behind a launcher
	// script, the launcher's own child — cannot be left behind mutating the
	// workspace (SPEC §7.5).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// The output pipes are this package's, not cmd's. cmd.StdoutPipe would hand
	// ownership to Wait, which closes the read end the moment the process exits
	// — discarding anything still buffered, including the result line that is
	// the run's ground truth (SPEC §7.4). Owning them keeps the drain strictly
	// ahead of the decision (SPEC §7.5) and makes the post-exit bound ours to
	// set.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: stdout pipe: %w", l.Name, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdoutR, stdoutW)
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: stderr pipe: %w", l.Name, err)
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW

	if err := cmd.Start(); err != nil {
		closeAll(stdoutR, stdoutW, stderrR, stderrW)
		l.Transcript.Close()
		return nil, fmt.Errorf("%s: starting %s: %w", l.Name, l.Argv[0], err)
	}
	// The child holds its own descriptors now. Dropping the parent's copies is
	// what makes EOF mean "no descendant is still writing" — an *os.File passed
	// as Stdout is not tracked by cmd, so this close is ours to make.
	closeAll(stdoutW, stderrW)

	h := newHandle(l, cmd, stdoutR, stderrR)
	h.run(ctx, l.Transcript)

	// Past this point there is a process, so there is a handle: no path here may
	// return an error. Every `return nil, err` above precedes the process
	// existing, which is what makes "error returned" and "nothing is running"
	// equivalent for the caller (SPEC §7.4) — and the marker upgrade is the first
	// step that could fail *after* that equivalence is established. Returning an
	// error from it would leave a live group with no handle, nobody to stop it,
	// and the marker still un-upgraded: a workspace §9.10 must then park.
	//
	// So a sink failure is delivered as an outcome of a live run. expire is the
	// ordinary runner-enforced verdict — the same ladder a stall or a timeout
	// walks — which takes the group down and removes the marker by the one path
	// that is allowed to (confirmed absence, §9.8).
	//
	// This runs *after* h.run and *before* the pump publishes: the readers and
	// Wait must already be going, or the ladder would poll a zombie that answers
	// signal 0 until its grace expired, while publication must not have started,
	// or a fast child's `succeeded` would already be the run's outcome and no
	// later verdict could replace it (see handle.recorded).
	if l.OnRun != nil {
		if err := l.OnRun(localEvidence(h.pgid)); err != nil {
			h.expire(core.FailureLaunchError)
		}
	}
	close(h.recorded)
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
