package claudecode

import (
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// This adapter's v2 substrate surface (#205, #46): the same provider command,
// composed for a machine that is not this one.
//
// Everything about the invocation is deliberately *narrower* than the local
// Start, and each omission is one of the local path's host facts:
//
//   - **No `srt`.** SandboxMode is this host's launcher: `sandboxCommand` puts
//     the srt binary in front of argv[0] and `writeSandbox` generates a settings
//     file, a git config and an allowlist naming daemon-side paths (sandbox.go).
//     A sandbox has none of those paths, its own enforcement is the worker
//     profile's, and Airlock owns the one mandatory outer whole-process envelope
//     — so a wrapper here would be a second envelope inside it, built from a
//     policy describing another machine.
//   - **No `CLAUDE_CONFIG_DIR` or `CLAUDE_CODE_TMPDIR`.** Both name children of
//     the workspace's private dir (harnessDirs, #114), which exists on the
//     daemon's disk. A remote workspace reports no paths at all, so there is
//     nothing to point them at and the profile resolves the harness's config
//     and scratch itself.
//   - **No resolved binary path.** Start substitutes the absolute path Ready
//     found on this host; the configured name is what a sandbox can resolve.
//   - **No publish credential.** It stays on the daemon
//     (harness.RemoteEnviron). After this coding run is terminal and quiet, BEN
//     invokes Airlock's credential-free Git client in a separate publish run.
//
// What is unchanged is the provider argv itself — the flags, the model, the
// resume token, the settings file and the cost backstop — because that is the
// command, not a wrapper around it.

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

// RemoteInvocation composes the substrate request for one attempt.
//
// It parses the provider block on every call rather than binding it, because
// this is a *kind* method: the whole point of the seam is that assembly can ask
// for the command without constructing a runner that would probe this host for a
// harness no run here will launch (core.RemoteRunnerKind).
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
	env, err := harness.RemoteEnviron(p.Env, p.injected(), spec, ErrRemoteGitHubCredential)
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

// RemoteCapabilities is what this harness supports wherever it runs. Resume and
// usage are properties of the CLI's own stream, not of the host.
func (Kind) RemoteCapabilities() core.Capabilities { return capabilities() }
