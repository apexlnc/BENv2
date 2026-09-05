package harness

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The child environment for a run that happens on a v2 execution substrate
// (#205, #46) — the same composition question Environ answers locally, asked
// about a machine this daemon does not own.
//
// Three of Environ's five layers are deliberately absent, and the omissions are
// the whole content of this function:
//
//   - **core.EnvAllowlist is not composed.** It exists because a child launched
//     *here* would otherwise inherit the daemon's whole environment, and it names
//     the handful of variables a local process needs to find its tools, home and
//     locale (SPEC §7.6). None of those describes the sandbox: its PATH, HOME and
//     TMPDIR are the worker profile's, and sending this host's would point a run
//     at directories that do not exist. Composing it would not be protecting
//     anything — it would be exporting this host's environment to another machine.
//   - **`env_passthrough` is not composed either, and that is the same rule with
//     teeth.** Its values are read from the daemon's own process (Environ), which
//     is exactly the set §7.6 exists to keep out of a child. A repo-authored
//     workflow naming a variable is asking for *this host's* value, and the one
//     place it must never be delivered to is a machine BEN cannot audit.
//   - **The publish credential never leaves the daemon.** Nothing BEN holds — the
//     tracker credential, the base-fetch credential, or the publish credential —
//     is serialized into a substrate request (docs/AIRLOCK.md). A remote coding
//     run never publishes: BEN starts a separate trusted publish phase whose
//     credential-free client talks to Airlock's workload gateway.
//
// What remains is what the operator wrote *about this agent* in its provider
// block — the `env` map and the adapter's own documented auth surface — plus the
// orchestrator's BEN_ coordinates. Those are configuration rather than host
// state, and dropping them would silently give a remote run a different
// configuration from the one `ben config effective` prints. The exception is a
// reusable GitHub credential: standard GitHub tooling consumes the names below
// as ambient authority, and #194 explicitly forbids sending that authority to a
// remote execution environment. Remote publication belongs to Airlock's
// separately authorized publish phase.
func RemoteEnviron(
	providerEnv, injected map[string]string, spec core.RunSpec, credentialSentinel error,
) (map[string]string, error) {
	env := map[string]string{}
	maps.Copy(env, providerEnv)
	for k, v := range injected {
		if v != "" {
			env[k] = v
		}
	}
	for k, v := range spec.Env {
		if RemoteEnvOmitted(k) {
			continue
		}
		env[k] = v
	}
	if err := CheckRemoteProviderEnvironment(env, credentialSentinel); err != nil {
		return nil, err
	}
	return env, nil
}

// CheckRemoteProviderEnvironment applies #194's credential rule to environment
// destinations. RemoteStructural asks it about the parsed provider environment
// before a backend is constructed; RemoteEnviron asks it again about the whole
// per-attempt environment before a request is composed.
func CheckRemoteProviderEnvironment(env map[string]string, credentialSentinel error) error {
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if _, forbidden := remoteGitHubCredentialEnv[name]; forbidden {
			return fmt.Errorf("%w: remote invocation environment contains %s, a reusable GitHub "+
				"credential variable that an execution substrate may not receive",
				credentialSentinel, name)
		}
	}
	return nil
}

// remoteGitHubCredentialEnv is the ambient authentication surface recognized by
// GitHub tooling in this repository. Exact names, because environment variables
// are case-sensitive and a lookalike grants no authority to that tooling.
var remoteGitHubCredentialEnv = map[string]struct{}{
	"GITHUB_TOKEN":            {},
	"GH_TOKEN":                {},
	"GITHUB_API_TOKEN":        {},
	"GH_ENTERPRISE_TOKEN":     {},
	"GITHUB_ENTERPRISE_TOKEN": {},
}

// CheckRemoteProviderSources applies #194's credential rule to the source side
// of the entire provider block. Destination checks cannot see either a rename
// (`env.AGENT_FLAG: $GH_TOKEN`) or an argv-bound field (`model: $GH_TOKEN`), so
// both adapters ask this before parsing or composing any invocation.
func CheckRemoteProviderSources(sources []core.ProviderEnvSource, credentialSentinel error) error {
	fields := map[string]string{}
	for _, source := range sources {
		name := strings.TrimSpace(source.Variable)
		if _, forbidden := remoteGitHubCredentialEnv[name]; !forbidden {
			continue
		}
		field := source.Field
		if field == "" {
			field = "agent.provider"
		}
		if previous, seen := fields[name]; !seen || field < previous {
			fields[name] = field
		}
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		return fmt.Errorf("%w: %s resolves from $%s, a reusable GitHub credential source "+
			"that an execution substrate may not receive", credentialSentinel, fields[name], name)
	}
	return nil
}

// RemoteEnvOmitted names the orchestrator variables a remote invocation must not
// carry, and today there is exactly one.
//
// `BEN_WORKSPACE` is the agent's working directory, and on this substrate BEN
// does not know it. A remote workspace identity is opaque by construction — a
// sandbox id and a profile revision, with no path in it (remote.Identity) — and
// the run's working directory belongs to the worker profile. The value the
// orchestrator composes is BEN's *own* address for the workspace cycle, which is
// not a directory anywhere; passing it would state a filesystem fact BEN has not
// got, in the one variable an agent would act on.
//
// `BEN_BRANCH` is kept, and the difference is the point: the branch is BEN's
// fact about where the work publishes (remote.Identity.Branch), not a claim
// about the sandbox's disk.
func RemoteEnvOmitted(name string) bool {
	return strings.EqualFold(name, "BEN_WORKSPACE")
}

// CheckRemoteSpec is CheckSpec's substrate half: the two checks that are about
// the *request* rather than about this host.
//
// The reserved-namespace rule and the empty prompt are properties of the RunSpec
// and hold wherever the run happens. What is deliberately absent is the
// workspace-path check, and its absence is a statement rather than an omission:
// that check stats a directory on the daemon's disk, and a remote workspace
// identity carries no path at all (remote.Identity). Running it would refuse
// every remote attempt, and relaxing it to "non-empty" would assert a filesystem
// fact about a machine BEN cannot see.
func CheckRemoteSpec(spec core.RunSpec, errs SpecErrors) error {
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		if !strings.HasPrefix(name, core.EnvPrefix) {
			return fmt.Errorf("%w: RunSpec.Env may carry only %s-prefixed keys, got %q",
				errs.EnvNamespace, core.EnvPrefix, name)
		}
	}
	if spec.Prompt == "" {
		return errs.PromptEmpty
	}
	return nil
}
