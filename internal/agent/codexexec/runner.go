// Package codexexec is the codex-exec agent runner adapter (SPEC §7.7):
// `codex exec --json`, one process per attempt, resume via the CLI's own
// `resume <thread_id>` subcommand. It translates the harness stream into the
// closed event enum at the boundary so the orchestrator never sees a raw agent
// event (SPEC §3.6).
//
// It is the second adapter, and it exists to keep the first one honest: the
// process lifecycle, liveness windows, execution-domain teardown, environment composition,
// and transcript retention are shared with claude-code in internal/agent/harness,
// and both adapters run the same conformance suite (internal/agent/agenttest)
// unmodified. What is left here is genuinely codex-shaped — an argv, an
// environment variable, a line format, two probes.
//
// # What the sandbox permits
//
// Under `workspace-write` the agent can commit: measured against 0.147.0 on
// macOS, `git add` and `git commit` inside a *linked worktree* succeed, and both
// the new object and the updated branch ref land in the shared git directory of
// the base repository — outside the worktree. That is what makes the mode usable
// for BEN at all, since every workspace is a linked worktree whose object store
// lives in `base.git` (SPEC §6.2), and it is worth stating because it is not
// obvious: the writable root is the workspace, and the writes that matter are
// not. `git push` additionally needs `network_access` (SPEC §6.7).
//
// It also happens to justify one of the shared runtime's choices out loud: the
// `codex` on PATH is a launcher that spawns the platform binary as a child, so
// the process this adapter starts is never the only process doing the work.
// Stopping that direct child alone would leave the real harness running in the
// workspace; §7.5's execution domain accounts for the complete PID namespace,
// including children that create another process group or session.
//
// # The agent.provider block
//
// SPEC §7.7 requires each adapter to document its own keys. The set is closed —
// an unknown key is refused at load, because a typo in a sandbox mode is not
// something to discover at run time.
//
//	sandbox_mode    REQUIRED. workspace-write | danger-full-access. No default:
//	                the operator states the posture. `read-only` is refused —
//	                it cannot write, so it can never publish.
//	network_access  Grants the workspace-write sandbox network egress. The agent
//	                publishes its own PR, so a workflow whose agent runs `gh`
//	                needs it (SPEC §6.7). Always passed under workspace-write,
//	                false included, so the harness's config file cannot decide it
//	                (see sandboxOverrides). Inert under danger-full-access, which
//	                sandboxes nothing.
//	binary          Harness executable; a bare name resolves on PATH ("codex").
//	model           Alias or full name → --model.
//	api_key         → CODEX_API_KEY in the child env.
//	codex_home      → CODEX_HOME in the child env: the directory holding
//	                config.toml and a stored login. An *absolute path*, which is
//	                also the escape hatch for any harness setting this block does
//	                not name — settings belong in a file, not in argv. Relative is
//	                refused: readiness and an attempt run from different
//	                directories.
//	add_dirs        Writable roots beyond the workspace, absolute paths only.
//	                Empty is the norm; the workspace is the boundary. Passed as
//	                the sandbox's writable_roots, empty included, so ambient
//	                config cannot add roots. Inert under danger-full-access.
//	env             Extra child environment entries.
//	env_passthrough Daemon environment variable names to forward. Names only,
//	                so a secret does not become a load-time requirement.
//
// Neither environment surface may define a `BEN_`-prefixed key: that namespace
// is the orchestrator's, exclusively, and this half of the reservation is a
// property of the file, so it refuses at load (SPEC §7.6). Nor may either one
// spell the variable `publish.env` names, for the same reason pointed at a
// different owner.
//
// Publish-credential surface (SPEC §5.2.8, §6.7, §7.7): the agent runs `gh`
// itself, and what `gh` authenticates from arrives through the top-level
// `publish` block rather than through this one — the identity BEN publishes as
// is the operator's choice, not an adapter parameter. This adapter resolves that
// reference once per attempt and injects it under the variable the block names,
// never into argv; the sandbox must also allow egress (see network_access).
//
// # Capabilities
//
// Resume and usage, both declared honestly (SPEC §7.1). Resume is the thread id
// the CLI accepts back; usage is token counts only — `codex exec` reports no
// cost, so core.Usage.CostUSD stays 0 and a workflow's cost cap has no
// harness-side backstop here (SPEC §9.9, see command.go).
//
// Verified against codex-cli 0.147.0; testdata holds recorded streams from it.
package codexexec

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// readinessTimeout bounds Ready's subprocess probes together. Startup refuses
// on it (SPEC §11), so it must not hang a daemon boot.
const readinessTimeout = 10 * time.Second

// KindName is the agent.kind this package answers to.
const KindName = "codex-exec"

// Kind is the package-level agent.kind registration (SPEC §5.7, §7.1; BUILD
// assembly decision 13): the entry points that exist before any runner does.
// domain is an unexported contract-test seam; registry construction leaves it
// nil and therefore selects the process-lifetime production domain.
type Kind struct{ domain harness.ExecutionDomain }

var _ core.RunnerKind = Kind{}

// Structural reports whether an agent.provider block is well-formed. It is pure
// — no filesystem, no subprocess, no network — so `ben config effective` works
// on a machine that has never installed the harness (SPEC §5.8, §7.1).
// Everything that can fail because the world is not set up belongs to Ready.
func (Kind) Structural(cfg core.AgentConfig) error {
	_, err := ParseProvider(cfg)
	return err
}

// LocalWrites reports the complete write scope beyond the workspace and
// inherited TMPDIR. An unsandboxed run is explicitly unbounded: add_dirs and
// TMPDIR are then strict subsets, not the write boundary. Parsing through
// ParseProvider is load-bearing: the grant and its declaration cannot disagree
// about a key.
func (Kind) LocalWrites(cfg core.AgentConfig, _ core.LocalRuntimePaths) (core.LocalWriteScope, error) {
	p, err := ParseProvider(cfg)
	if err != nil {
		return core.LocalWriteScope{}, err
	}
	if p.SandboxMode == sandboxFullAccess {
		return core.LocalWriteScope{Unbounded: true}, nil
	}
	roots := slices.Clone(p.AddDirs)
	if tmp, ok := p.Env["TMPDIR"]; ok {
		roots = append(roots, tmp)
	}
	return core.LocalWriteScope{Roots: roots}, nil
}

// ForwardedEnvVars names the variables this adapter copies into a child by
// name (SPEC §10.2). Everything the block carries in as a resolved value is the
// loader's to see from its own provenance, which is what keeps a route from
// being forgotten here.
func (Kind) ForwardedEnvVars(provider map[string]any) []string {
	return harness.ForwardedEnvVars(provider)
}

// SensitiveFields names the provider values that are secrets, so every
// `config effective` rendering hides them whatever their provenance
// (SPEC §5.8). Delegated, because the two adapters place secrets in the same
// two shapes and a private copy each is a second place to forget one.
func (Kind) SensitiveFields(provider map[string]any) [][]string {
	return harness.SensitiveFields(provider, credentialKeys)
}

// Model names the model this block selects, for the attempt-outcome record
// (#60). Delegated for the same reason the two above are: both adapters read
// the same key, and the key this package turns into `--model` (command.go) is
// the one the record has to name.
func (Kind) Model(provider map[string]any) (string, []string) {
	return harness.Model(provider)
}

// New binds the configuration to a runner instance.
//
// The nil is returned explicitly rather than as a typed *Runner: a refusal must
// leave the caller with a nil interface, or `runner, err := kind.New(...)`
// hands back something non-nil to call methods on after it has already failed.
func (k Kind) New(opts core.RunnerOptions) (core.AgentRunner, error) {
	r, err := New(Options{
		Provider:       opts.Provider,
		Publish:        opts.Publish,
		AttemptTimeout: opts.AttemptTimeout,
		TranscriptDir:  opts.TranscriptDir,
		OnRun:          opts.OnRun,
		Timings:        harness.Timings{StopGrace: opts.StopGrace},
		Domain:         k.domain,
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Runner is a constructed codex-exec runner: its provider configuration is
// bound and will not change under it (SPEC §7.1).
type Runner struct {
	provider Provider
	// publish is the publish credential *reference* (SPEC §5.2.8), bound here
	// with the provider block so Ready and Start cannot disagree about which
	// credential a run publishes with (SPEC §7.1). The value behind it is read
	// per attempt, never held: a workflow file must load on a host that has none,
	// so this is the one piece of configuration whose resolution is deferred.
	publish core.PublishBinding
	// attemptTimeout is the bound the publish credential's deadline is checked
	// against, bound here with the credential for the same reason: Ready and
	// Start must not disagree about the arithmetic (SPEC §7.7).
	attemptTimeout time.Duration

	// binary is the harness path, resolved on the first successful Ready or
	// Start and reused from then on. Resolution belongs to Ready rather than New
	// (BUILD B06): a binary installed between construction and readiness must be
	// found. Only success is remembered — memoizing a failure would make a
	// runner permanently wrong about a machine that has since been set up, and
	// the binding exists to prevent divergence, not to cache absence. Resolving
	// again after that would undo it in the other direction: a PATH change
	// between Ready and Start would run a binary nothing verified (SPEC §7.1).
	binaryMu sync.Mutex
	binary   string

	transcripts harness.TranscriptStore
	onRun       core.RunEvidenceSink
	domain      harness.ExecutionDomain
	// redact are the bound block's credential values, kept out of retained
	// transcripts (SPEC §10.3). Read off credentialKeys through
	// harness.CredentialValues, so this adapter states each credential once.
	redact  []string
	timings harness.Timings
}

// Options configures a Runner.
type Options struct {
	// Provider is the agent.provider block. It is parsed once, here.
	Provider map[string]any
	// Publish is the publish credential's binding (SPEC §5.2.8): the child
	// variable and the source that mints it. The zero value configures none.
	Publish core.PublishBinding
	// AttemptTimeout is `limits.attempt_timeout_ms`, the other operand of the
	// TTL gate Ready and Start apply to a bounded credential (SPEC §7.7).
	AttemptTimeout time.Duration
	// TranscriptDir retains raw per-run harness streams (SPEC §10.3); empty
	// disables retention. Transcripts overrides it when set.
	TranscriptDir string
	// Transcripts is an explicit sink, for a caller that owns run identity.
	Transcripts harness.TranscriptStore
	// Timings overrides the harness's lifecycle windows (harness.Timings); each
	// unset field keeps its harness.DefaultTimings value.
	Timings harness.Timings
	// OnRun records each run's evidence against the workspace it belongs to
	// (SPEC §9.10). Bound per attempt in Start, never hoisted: one runner serves
	// every issue, so a sink that could not name the workspace would upgrade the
	// wrong marker.
	OnRun core.RunEvidenceSink
	// Domain overrides the process-lifetime Linux execution domain in contract
	// tests. Production leaves it nil.
	Domain harness.ExecutionDomain
}

// New binds the provider configuration to a runner. Binding at construction is
// what makes Ready meaningful: the configuration Ready checks is the one Start
// will use, and a reload constructs a fresh runner rather than mutating this
// one underneath an in-flight attempt (SPEC §7.1).
func New(opts Options) (*Runner, error) {
	// The same call Structural makes, with the same argument, so a runner cannot
	// be constructed from a configuration Structural would have refused.
	p, err := ParseProvider(core.AgentConfig{Provider: opts.Provider, Publish: harness.PublishReference(opts.Publish)})
	if err != nil {
		return nil, err
	}
	domain := opts.Domain
	if domain == nil {
		domain = harness.LocalDomain()
	}
	r := &Runner{
		provider:       p,
		publish:        opts.Publish,
		attemptTimeout: opts.AttemptTimeout,
		redact:         harness.CredentialValues(opts.Provider, credentialKeys),
		transcripts:    opts.Transcripts,
		onRun:          opts.OnRun,
		domain:         domain,
		timings:        opts.Timings,
	}
	if r.transcripts == nil {
		if opts.TranscriptDir != "" {
			r.transcripts = harness.DirTranscripts{Dir: opts.TranscriptDir}
		} else {
			r.transcripts = harness.NopTranscripts{}
		}
	}
	return r, nil
}

// Capabilities reports what this harness supports (SPEC §7.1). Resume is the
// thread id carried in RunSpec.Continuation; usage is the token counts on the
// turn.completed line — without a cost, which core.Usage allows for.
func (r *Runner) Capabilities() core.Capabilities { return capabilities() }

// capabilities is the one declaration behind both substrates — see
// claudecode.capabilities for why it is a function and why there is only one.
func capabilities() core.Capabilities { return core.Capabilities{Resume: true, Usage: true} }

// Ready checks the bound configuration against the world: the binary exists,
// identifies itself as the Codex CLI, and has a plausible credential
// (SPEC §7.1). Catching any of it here is the difference between one loud
// refusal at startup and every dispatch burning a workspace to rediscover it.
func (r *Runner) Ready(ctx context.Context) error {
	if err := r.domain.Ready(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrExecutionDomain, err)
	}
	path, err := r.resolveBinary()
	if err != nil {
		return err
	}
	// The publish credential is read here as well as at Start, and this is the
	// reading that matters: §5.5 defers it so the file loads without the secret,
	// which leaves startup as the one place a host missing it can be told once
	// rather than one dispatched issue at a time (SPEC §5.2.8).
	publish, err := harness.MintPublish(ctx, r.publish, r.attemptTimeout, ErrPublishCredential)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	return r.provider.checkRuntime(ctx, path, publish, r.timings)
}

// resolveBinary finds the harness once and keeps the answer.
func (r *Runner) resolveBinary() (string, error) {
	r.binaryMu.Lock()
	defer r.binaryMu.Unlock()
	if r.binary != "" {
		return r.binary, nil
	}
	path, err := harness.ResolveBinary(r.provider.Binary, ErrBinary)
	if err != nil {
		return "", err
	}
	r.binary = path
	return r.binary, nil
}

// Start launches one attempt. An error here means no process and no handle:
// the orchestrator never has to reason about a half-started run. Once Start
// returns a handle, every outcome arrives as a terminal event instead
// (SPEC §7.4).
//
// Cancelling ctx after a successful Start discards the run — the daemon is
// shutting down — equivalent to Stop(StopDiscard).
func (r *Runner) Start(ctx context.Context, spec core.RunSpec) (core.RunHandle, error) {
	// The provider config is the one bound at New. Nothing about this attempt
	// can change it (SPEC §7.1).
	p := r.provider
	if err := harness.CheckSpec(spec, specErrors); err != nil {
		return nil, err
	}
	if spec.Continuation != "" && !r.Capabilities().Resume {
		// Unreachable today; the contract says an adapter without resume MUST
		// fail loudly rather than silently start a fresh session (SPEC §7.1).
		return nil, fmt.Errorf("codex-exec: resume not supported but continuation token supplied")
	}

	// The path Ready resolved and verified, not the configured name. A caller
	// that skipped Ready resolves here instead — once, and identically.
	binary, err := r.resolveBinary()
	if err != nil {
		return nil, err
	}
	argv, err := p.command(spec)
	if err != nil {
		return nil, err
	}
	argv[0] = binary

	// Per attempt, before the environment is composed. Refusing here costs one
	// attempt; launching without it costs the whole ticket's work, discovered at
	// `git push`, which is what an absent env_passthrough name silently bought
	// before the credential was a named thing (SPEC §5.2.8).
	publish, err := harness.MintPublish(ctx, r.publish, r.attemptTimeout, ErrPublishCredential)
	if err != nil {
		return nil, err
	}

	// One snapshot behind both: the environment the child gets and the values
	// kept out of its transcript cannot disagree (SPEC §10.3).
	env, undeclared := p.environ(publish, spec)
	if err := harness.CheckRedactableEnv(undeclared, ErrProviderValue); err != nil {
		return nil, err
	}
	redact := append(slices.Clone(r.redact), slices.Sorted(maps.Values(undeclared))...)

	transcript, promptSink, err := r.transcripts.Open(spec)
	if err != nil {
		return nil, fmt.Errorf("codex-exec: %w", err)
	}
	handle, err := harness.Start(ctx, harness.Launch{
		Name:       KindName,
		Argv:       argv,
		Env:        env,
		Dir:        spec.Workspace.Path,
		Prompt:     spec.Prompt,
		Limits:     spec.Limits,
		Translate:  translate,
		Transcript: transcript,
		PromptSink: promptSink,
		Redact:     redact,
		OnRun:      harness.BindEvidence(r.onRun, spec),
		Timings:    r.timings,
		Domain:     r.domain,
	})
	if errors.Is(err, harness.ErrExecutionDomain) {
		return nil, fmt.Errorf("%w: %w", ErrExecutionDomain, err)
	}
	return handle, err
}

// specErrors names this adapter's refusals for the shared RunSpec checks
// (SPEC §7.6): each adapter keeps its own sentinels, only the reasoning is
// shared.
var specErrors = harness.SpecErrors{
	EnvNamespace:  ErrEnvNamespace,
	PromptEmpty:   ErrPromptEmpty,
	WorkspacePath: ErrWorkspacePath,
}
