package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// The §10.2 credential split, enforced at load.
//
// SPEC §10.2 names exactly two credentials and §6.7 gives publishing to the
// agent, so the tracker credential — which authorizes issue writes, assignment
// and labels — has no business inside an agent process. An agent holding it can
// rewrite the queue that dispatched it: strip `ben:*`, take the assignment, close
// the issue, claim more work. §10.2 named the two; nothing enforced the split.
//
// It refuses at load rather than at dispatch because it is a property of the
// file: authored once, it hits every run (the same reasoning as §7.6's `BEN_`
// reservation, harness.CheckProviderEnv).

// ErrCredentialShared is the §10.2 refusal, for callers asserting on the
// sentinel rather than on the detail (AGENTS.md conventions).
var ErrCredentialShared = errors.New("the tracker credential also reaches the agent")

// CredentialSharedError reports a workflow that would hand the tracker
// credential to an agent process (SPEC §10.2, §6.7).
//
// It names the variable, which is not a secret — the whole point of §5.5's
// indirection is that the file says a name and the environment holds the value.
// It never names the value, and never had one to name: the comparison is over
// references.
type CredentialSharedError struct {
	// Var is the environment variable both sides resolve to. It is the
	// operator's locator as well as the finding: §5.5 makes the name the thing
	// the file says, so grepping it finds both sites.
	Var string
	// TrackerKind and AgentKind name the adapters, so an operator knows whose
	// keys to look at when a side named the variable rather than a field.
	TrackerKind, AgentKind string
	// TrackerField and AgentField are the dotted paths that reference it, or
	// "" where a side reads or forwards the variable *by name* rather than
	// through a field value — the tracker's documented fallback, or an
	// `env_passthrough` entry. AgentField may also be allowlistSite.
	TrackerField, AgentField string
}

func (e *CredentialSharedError) Error() string {
	return fmt.Sprintf(
		"$%s is both the tracker credential (%s) and reaches the agent (%s): "+
			"SPEC §10.2 keeps these separate, because the tracker credential can rewrite the queue "+
			"that dispatched the run — give the agent its own push/PR-scoped credential",
		e.Var,
		credentialSite(e.TrackerField, e.TrackerKind),
		credentialSite(e.AgentField, e.AgentKind))
}

func (e *CredentialSharedError) Unwrap() error { return ErrCredentialShared }

func credentialSite(field, kind string) string {
	if field == "" {
		return "named by variable, not by a field — see the " + kind + " adapter's credential and environment keys"
	}
	return field
}

// allowlistSite is the AgentField for a variable that reaches the child because
// §7.6 copies it, rather than because the file mentioned it.
const allowlistSite = "core.EnvAllowlist (§7.6 daemon-environment passthrough)"

// checkCredentialSplit refuses a workflow whose tracker credential can reach an
// agent process.
//
// Asked after validate, so both kinds are known to exist. Structural is not run
// here and cannot be — Load never runs it, the assembly does (SPEC §5.7) — so
// neither side may assume a well-formed block.
func checkCredentialSplit(cfg *Config, prov Provenance) error {
	trackerKind, ok := registry.Tracker(cfg.Tracker.Kind)
	if !ok {
		return nil // validate already refused this
	}
	agentKind, ok := registry.Runner(cfg.Agent.Kind)
	if !ok {
		return nil
	}

	trackerVars := trackerCredentialVars(trackerKind.CredentialRefs(cfg.Tracker.Provider), cfg.Credentials.Tracker, prov)
	agentVars := agentChildVars(agentKind, cfg.Agent.Provider, cfg.Publish, cfg.Credentials.Publish, prov)

	// Sorted so a file with more than one collision always refuses on the same
	// one: a refusal that varies with map iteration order is a refusal an
	// operator cannot reproduce from the message.
	for _, name := range slices.Sorted(maps.Keys(trackerVars)) {
		if agentField, shared := agentVars[name]; shared {
			return &CredentialSharedError{
				Var:          name,
				TrackerKind:  cfg.Tracker.Kind,
				AgentKind:    cfg.Agent.Kind,
				TrackerField: trackerVars[name],
				AgentField:   agentField,
			}
		}
	}
	return nil
}

// trackerCredentialVars resolves the tracker kind's references to variable
// names, mapped to the field path that referenced each — "" for a variable the
// kind reads directly.
//
// Precise rather than "every env-resolved leaf in the block", which is what the
// agent side does: `repo: $ORG_NAME` is not a credential, and flagging it would
// refuse harmless configurations over a variable that carries no secret. The
// tracker block has exactly one credential and its kind knows which key.
// A named `credential_sources` entry contributes too, and has to: a `static`
// source over `$FOO` is the same secret as a tracker `token: $FOO`, and the
// authority rule alone would catch only the tracker↔publisher pair — not the
// agent routes this function exists to read.
func trackerCredentialVars(refs core.CredentialRefs, cred Credential, prov Provenance) map[string]string {
	out := map[string]string{}
	if cred.Name != "" && cred.variable != "" {
		out[cred.variable] = sourcePath(cred.Name) + ".value"
	}
	for _, segments := range refs.Fields {
		path := "tracker.provider"
		for _, seg := range segments {
			path = appendProvenanceMapKey(path, seg)
		}
		for _, name := range prov[path].envVars() {
			out[name] = path
		}
	}
	for _, name := range refs.Vars {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		// A field reference wins over a direct name for the same variable: it
		// points at a line in the file, which is what an operator can act on.
		if _, seen := out[name]; !seen {
			out[name] = ""
		}
	}
	return out
}

// agentChildVars is every variable whose secret can reach an agent process,
// mapped to what put it there.
//
// Four sources, and the first is what makes this whole-block rather than
// key-by-key:
//
//  1. **Every env-resolved leaf under `agent.provider`.** Turning that block into
//     a child process is the adapter's whole job, so every value in it lands in
//     argv, in the child environment, or in a file the child reads.
//     `model: $TRACKER_PAT` reaches the child as `--model <secret>`, and a rule
//     that asked an adapter to enumerate its "credential keys" would miss it — as
//     would any list, for the next key somebody adds. The loader already walked
//     every leaf and recorded its provenance, so it reads that instead of asking.
//  2. **§7.6's daemon-environment allowlist.** PATH, HOME, TERM and the rest are
//     copied into every child unconditionally (harness.Environ), so a tracker
//     credential sourced from one of them is in the child whatever the agent block
//     says. Not an adapter's to declare: it is core's list, and it applies to all
//     of them.
//  3. **The publish credential's variable** (SPEC §5.2.8). Injected into the
//     child by construction — naming it is the whole point of the block — and
//     invisible to the other three: it is not in the opaque block, the adapter
//     does not forward it, and the loader deliberately never resolved it, so
//     there is no provenance entry to read. The parsed name is read straight off
//     the config, which the loader owns because the section is not opaque.
//  4. **The variables the kind forwards by name** — its `env_passthrough`
//     surface, the one route the loader cannot see for itself, because the value
//     in the block is a variable *name* rather than a secret.
//  5. **A named `credential_sources` entry the publish block selects.** Same
//     reasoning as 3, one spelling further out: `publish.kind: source` names an
//     entry, and a `static` entry names a variable, so the secret reaches the
//     child exactly as `publish.value` would.
func agentChildVars(kind core.RunnerKind, provider map[string]any, publish PublishConfig, cred Credential, prov Provenance) map[string]string {
	out := map[string]string{}

	// Sorted so the cited path is stable when several leaves share a variable.
	for _, path := range slices.Sorted(maps.Keys(prov)) {
		if !underProviderBlock(path, "agent.provider") {
			continue
		}
		for _, name := range prov[path].envVars() {
			if _, seen := out[name]; !seen {
				out[name] = path
			}
		}
	}
	// Before the allowlist, so a variable that is both reports the field an
	// operator can edit rather than "core copies this into every child".
	if name := publish.ValueVar; name != "" {
		if _, seen := out[name]; !seen {
			out[name] = "publish.value"
		}
	}
	if cred.Name != "" && cred.variable != "" {
		if _, seen := out[cred.variable]; !seen {
			out[cred.variable] = sourcePath(cred.Name) + ".value"
		}
	}
	for _, name := range core.EnvAllowlist {
		if _, seen := out[name]; !seen {
			out[name] = allowlistSite
		}
	}
	for _, name := range kind.ForwardedEnvVars(provider) {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if _, seen := out[name]; !seen {
			out[name] = ""
		}
	}
	return out
}

// underProviderBlock reports whether a provenance path names a leaf inside the
// given block. Both segment spellings count: a plain key appends ".key", while a
// key needing quotes or a list index appends "[...]" (appendProvenanceMapKey,
// appendProvenanceIndex).
func underProviderBlock(path, prefix string) bool {
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || rest == "" {
		return false
	}
	return rest[0] == '.' || rest[0] == '['
}

// envVars is the variables behind an env-resolved value, and none for any other
// origin.
func (o FieldOrigin) envVars() []string {
	if o.Source != SourceEnv {
		return nil
	}
	return o.EnvVars
}
