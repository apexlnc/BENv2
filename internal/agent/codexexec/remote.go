package codexexec

import (
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// This adapter's v2 substrate surface (#205, #46).
//
// The interesting half is what is *kept*, because it is the opposite call from
// the one claude-code's remote invocation makes. `--sandbox workspace-write` and
// the `-c sandbox_workspace_write.*` overrides are not a launcher around the
// command: the harness enforces them inside its own process, and an omitted `-c`
// is not a neutral default but a decision handed to whatever `config.toml` lives
// under CODEX_HOME (sandboxOverrides). So the pinned posture travels unchanged,
// *inside* the profile-owned outer envelope, and the operator's stated sandbox
// is the sandbox — which is the whole posture of this adapter (ErrSandboxMode).
//
// What is dropped is the one host fact in the environment: CODEX_HOME names a
// directory on the daemon's disk, and pointing a sandbox at it would name a path
// that does not exist there. The profile resolves the harness's home itself.

var _ core.RemoteRunnerKind = Kind{}

// RemoteStructural validates the provider rules that apply only when BEN will
// serialize an invocation to another machine. Assembly calls this before it
// constructs or probes the selected substrate; RemoteInvocation repeats the
// same path so a caller cannot bypass the boundary.
func (Kind) RemoteStructural(cfg core.AgentConfig) error {
	_, err := remoteProvider(cfg)
	return err
}

func remoteProvider(cfg core.AgentConfig) (Provider, error) {
	if err := harness.CheckRemoteProviderSources(cfg.ProviderEnvSources, ErrRemoteGitHubCredential); err != nil {
		return Provider{}, err
	}
	p, err := ParseProvider(cfg)
	if err != nil {
		return Provider{}, err
	}
	if err := harness.CheckRemoteProviderEnvironment(p.Env, ErrRemoteGitHubCredential); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// RemoteInvocation composes the substrate request for one attempt. See
// claudecode.Kind.RemoteInvocation for why this is a kind method rather than a
// runner one.
func (Kind) RemoteInvocation(cfg core.AgentConfig, spec core.RunSpec) (core.RemoteInvocation, error) {
	p, err := remoteProvider(cfg)
	if err != nil {
		return core.RemoteInvocation{}, err
	}
	if err := harness.CheckRemoteSpec(spec, specErrors); err != nil {
		return core.RemoteInvocation{}, err
	}
	if spec.Continuation != "" && !capabilities().Resume {
		return core.RemoteInvocation{}, fmt.Errorf("%s: resume not supported but continuation token supplied", KindName)
	}
	// Before the environment is composed: the resume token is refused wherever the
	// argv is built (command), and a substrate request is an argv like any other.
	argv, err := p.command(spec)
	if err != nil {
		return core.RemoteInvocation{}, err
	}
	// CODEX_HOME is deliberately absent from the injected surface here; see the
	// file comment. The API key is the operator's configuration for this agent
	// rather than a fact about this host, so it travels exactly as the `env`
	// block does (harness.RemoteEnviron).
	env, err := harness.RemoteEnviron(
		p.Env, map[string]string{"CODEX_API_KEY": p.APIKey}, spec, ErrRemoteGitHubCredential,
	)
	if err != nil {
		return core.RemoteInvocation{}, err
	}
	return core.RemoteInvocation{
		Argv:  argv,
		Env:   env,
		Stdin: []byte(spec.Prompt),
	}, nil
}

// RemoteTranslate is the exported raw-line boundary, so a substrate-hosted run
// is read by the same parser a local one is (see Translate).
func (Kind) RemoteTranslate(line []byte) []core.Event { return Translate(line) }

// RemoteCapabilities is what this harness supports wherever it runs.
func (Kind) RemoteCapabilities() core.Capabilities { return capabilities() }
