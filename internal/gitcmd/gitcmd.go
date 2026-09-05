// Package gitcmd is the one place BEN composes the invocation shape every git
// it starts must carry: the no-background-maintenance overrides (#154), the
// neutralization of the repository state surfaces a run can write to steer a
// daemon-side git (#228), and the child environment those invocations run under.
//
// It exists as a package rather than as a helper inside internal/workspace
// because internal/mirror is a second daemon component that drives git, and
// #222's developer-only ticketprep kernel consumes the same boundary. AGENTS.md
// states the rule as *one place*. Two copies of a safety override are two places
// for one of them to be edited, and the one that gets edited is whichever has
// the weaker comment attached.
//
// Deliberately argv and environment only. Running the command, obtaining a
// credential for it, bounding its output and classifying its exit status are
// each properties of the repository being driven, and the three consumers
// answer them differently: internal/workspace serializes against a base
// repository shared with live worktrees, internal/mirror against a bare store
// no agent can reach, and internal/ticketprep performs bounded read-only object
// queries in a developer-selected repository.
//
// Argv carries no declaration-driven test of its own: a test that read it back
// and compared it to the same literal would agree with any declaration,
// including one an edit emptied. That rule is anchored where it can fail — in
// each consumer's process-boundary test, over the argv its real lifecycle hands
// to the operating system. Env is instead held to Git's independently declared
// set of repository-local variables, and the mirror has the end-to-end
// regression for the object-store redirection that motivated the boundary
// (AGENTS.md, Conventions).
package gitcmd

import (
	"os"
	"strings"
)

// noAutoMaintenance turns off git's background maintenance for every git BEN
// starts (#154).
//
// `fetch`, `commit` and `merge` all end by running auto-maintenance, and git
// *detaches* what that starts, so it outlives the command BEN waited for — a
// pack can land in objects/pack after the fetch returned. What BEN then has is a
// process it did not start and cannot account for, running as the daemon's user,
// taking gc.pid and pack locks in a repository BEN believes it owns, while an
// attempt may be running against it. Not corruption: a process outside BEN's
// account of the workspaces it owns, which is what SPEC §9.10 is careful about
// everywhere else.
//
// Both keys, because they stop different things — maintenance.auto=false stops
// the fork itself, and gc.auto=0 stops the work, which is the leg that answers
// for a git old enough to have run `git gc --auto` directly, before `git
// maintenance` existed to be routed through. As `-c` rather than repository
// config, because `-c` outranks every config file: the guarantee then holds over
// a repository BEN did not create, cannot be edited out of one, and reaches the
// git processes git itself starts, through the GIT_CONFIG_PARAMETERS it exports
// for them. Maintaining the repository, if it is ever wanted, is then a decision
// BEN makes rather than git's default. docs/WORKTREES.md carries what each of
// those was measured from.
//
// Bounded to the git BEN starts, which is what BEN can account for: a hook's
// shell (SPEC §6.5) and an agent's run (§7.6) are given composed environments of
// their own, and a git either of them starts is theirs.
var noAutoMaintenance = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}

// noRepositorySteering takes away the three config surfaces a repository can use
// to steer the git BEN starts against it (#228).
//
// These are attacker-controlled input rather than a hypothetical: base.git must
// stay writable by the run — an agent's `git commit` in a linked worktree writes
// objects and refs there — so its hooks directory, its config and its refs are
// authored by the thing being judged (SPEC §3.5), and read afterwards by a
// daemon-side git that decides whether the run may be published. The legacy
// info/grafts file provides the same ancestry substitution as refs/replace/ but
// has no config switch; Env neutralizes that fourth surface below.
//
// Each key closes a different route out of that, and none of them covers
// another:
//
//   - core.hooksPath, so that no ref update, checkout or fetch BEN performs runs
//     a script the run left behind. `git fetch` runs reference-transaction and
//     `git worktree add` runs post-checkout, both out of base.git/hooks, both as
//     the daemon's user and inside the process BEN believes it is waiting for.
//   - core.fsmonitor, which is the second hook and not covered by the first: git
//     starts the configured program on index refresh, under its own key.
//   - core.useReplaceRefs, the config spelling Git 2.19 and newer understand for
//     disabling refs/replace/. A ref there substitutes one commit for another
//     in every object read git performs — including the `merge-base
//     --is-ancestor` behind SPEC §9.7 leg 1. Measured on git 2.39: a single ref
//     write turns "does not descend from the claim-time base" into "does",
//     while `rev-parse` keeps reporting the original commit, so the
//     substitution is invisible to the evidence around it. Env carries the
//     older, version-independent spelling as the primary guard below.
//
// Empty is what turns the two hook keys off: an empty core.fsmonitor reads as no
// monitor at all, and core.hooksPath is joined with the hook name, so an empty
// value resolves a lookup to /<hook-name> — outside every repository, and in
// particular outside the one the run can write.
//
// As `-c` rather than repository config for the reasons noAutoMaintenance
// already gives, plus one of its own: `-c` reaches the git processes git itself
// starts. For replacement refs it is defense in depth, not the compatibility
// boundary: Git has honored GIT_NO_REPLACE_OBJECTS since 1.6.6, while it silently
// ignored the unknown core.useReplaceRefs key before 2.19. Env scrubs any
// inherited value and then restores BEN's required value below, so the safety
// property does not imply an undeclared minimum Git version.
//
// Bounded to the git BEN starts, exactly as above: hooks are disabled for BEN's
// own invocations only. A SPEC §6.5 hook script is BEN's own configured command,
// run by the workspace provider rather than found by git, and a git the agent
// starts inside its worktree (§7.6) is the agent's.
var noRepositorySteering = []string{
	"-c", "core.hooksPath=",
	"-c", "core.fsmonitor=",
	"-c", "core.useReplaceRefs=false",
}

// Argv composes the full argv of a BEN-invoked git: the no-maintenance overrides
// and the repository-steering neutralization, which must precede the subcommand,
// then what the caller asked for. Every exec of git in this module goes through
// it; error messages keep naming the caller's own args, because the overrides are
// not part of the request.
func Argv(args []string) []string {
	argv := make([]string, 0, len(noAutoMaintenance)+len(noRepositorySteering)+len(args))
	argv = append(argv, noAutoMaintenance...)
	argv = append(argv, noRepositorySteering...)
	return append(argv, args...)
}

// repositoryLocalEnv is Git's `rev-parse --local-env-vars` contract: values a
// caller may inherit from another repository and that can redirect objects,
// refs, config, grafts, replacements or shallow state away from the repository
// BEN named explicitly.
//
// GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n are the indexed payload governed by
// GIT_CONFIG_COUNT. They are removed too: once the count is gone Git ignores
// them, but keeping credential-shaped config in a child environment would make
// that safety depend on an implementation detail of Git's parser.
var repositoryLocalEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_INTERNAL_SUPER_PREFIX":        true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,
}

// environmentAllowlist is the complete baseline inherited by a BEN-started
// Git. It is exact rather than prefix-based: subtraction leaves the next
// credential or Git control variable nobody thought to remove (#229).
//
// All three consumers — internal/workspace, internal/mirror and
// internal/ticketprep — need PATH for Git's own child programs. Local commands
// retain HOME, XDG_CONFIG_HOME and the explicit global/system selectors for
// operator configuration; RemoteEnv replaces both config layers below because
// a same-uid run may write them and thereby steer a credentialed command.
// GIT_CONFIG itself remains in repositoryLocalEnv and is always stripped.
// TMPDIR is Git's temporary-file location, TZ controls rendered times, and the enumerated
// locale categories preserve diagnostics and text handling. LANGUAGE is
// Git/gettext's message-language override; LC_* is deliberately not a prefix
// rule, so a coincidentally named secret cannot join the child environment.
//
// Transport values are deliberately absent here. A local workspace checkout
// can launch repository-authored filters, and the local mirror and ticketprep
// paths have no transport need, so even a normally useful SSH agent or
// credentialed proxy is authority this shared baseline must not carry.
var environmentAllowlist = map[string]bool{
	"PATH":                true,
	"HOME":                true,
	"XDG_CONFIG_HOME":     true,
	"GIT_CONFIG_GLOBAL":   true,
	"GIT_CONFIG_NOSYSTEM": true,
	"GIT_CONFIG_SYSTEM":   true,
	"TMPDIR":              true,
	"TZ":                  true,
	"LANG":                true,
	"LANGUAGE":            true,

	"LC_ALL":      true,
	"LC_COLLATE":  true,
	"LC_CTYPE":    true,
	"LC_MESSAGES": true,
	"LC_MONETARY": true,
	"LC_NUMERIC":  true,
	"LC_TIME":     true,
}

// remoteEnvironmentAllowlist is the exact transport overlay used only by
// RemoteEnv. Workspace and mirror contact the configured remote; ticketprep
// never does because GIT_NO_LAZY_FETCH makes its object reads offline. The
// fixed proxy spellings support Git/curl deployments and may themselves carry
// credentials. The CA file/directory spellings preserve custom trust roots
// without admitting client keys or TLS-disable switches. SSH_AUTH_SOCK and
// Git's three SSH selector variables are the non-interactive SSH surface; no
// broad SSH_* prefix admits askpass programs or unrelated session state.
//
// Git tracing, askpass programs, client TLS keys and GIT_SSL_NO_VERIFY are not
// compatibility settings here. Authenticated remoteGit paths append their
// BEN_REMOTE_PROTOCOL, BEN_REMOTE_HOST, BEN_REMOTE_USERNAME and
// BEN_REMOTE_PASSWORD values for one invocation only; those names do not
// belong to either parent-environment allowlist.
var remoteEnvironmentAllowlist = map[string]bool{
	"http_proxy":  true,
	"https_proxy": true,
	"all_proxy":   true,
	"no_proxy":    true,
	"HTTP_PROXY":  true,
	"HTTPS_PROXY": true,
	"ALL_PROXY":   true,
	"NO_PROXY":    true,

	"GIT_SSL_CAINFO":       true,
	"GIT_SSL_CAPATH":       true,
	"GIT_PROXY_SSL_CAINFO": true,
	"SSL_CERT_FILE":        true,
	"SSL_CERT_DIR":         true,

	"SSH_AUTH_SOCK":   true,
	"GIT_SSH":         true,
	"GIT_SSH_COMMAND": true,
	"GIT_SSH_VARIANT": true,
}

// remoteConfigEnvironment makes the configured URL and command-line credential
// policy the complete Git configuration input to a network process. Retaining
// HOME and XDG_CONFIG_HOME remains useful to transport children such as ssh,
// but those paths must not let Git load ~/.gitconfig or
// $XDG_CONFIG_HOME/git/config: both may be inside a provider-selected writable
// root even when daemon state is elsewhere. The system layer is suppressed for
// the same reason the fresh repository's config is empty — the remote command
// is defined at its call site, not by ambient policy.
var remoteConfigEnvironment = []string{
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=" + os.DevNull,
}

func isRemoteConfigVariable(key string) bool {
	return key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM" || key == "GIT_CONFIG_SYSTEM"
}

// Env composes a local Git child from environmentAllowlist, with
// GIT_GRAFT_FILE bound to empty and GIT_NO_REPLACE_OBJECTS bound to 1, plus a
// guard against interactive credential prompts hanging a BEN-started process.
//
// These are the two values that must remain present after the scrub:
//
//   - absence of GIT_GRAFT_FILE makes Git fall back to
//     $GIT_COMMON_DIR/info/grafts, which is inside the shared git dir a run must
//     be able to write. An empty value names no graft file, so the default
//     attacker-authored file cannot add a fake parent edge to the merge-base
//     query behind SPEC §9.7 leg 1.
//   - absence of GIT_NO_REPLACE_OBJECTS lets Git honor refs/replace/. Rebinding
//     it after removing the caller's repository-local value is the guard Git
//     versions back through 1.6.6 understand; core.useReplaceRefs=false remains
//     the second spelling newer Git understands.
func Env() []string {
	return childEnv(os.Environ(), false)
}

// RemoteEnv adds only the transport settings needed by workspace and mirror
// remoteGit calls and replaces ambient global/system Git configuration.
// Keeping this a separate entry point makes a local call site unable to gain
// remote authority merely by sharing Git's baseline setup.
func RemoteEnv() []string {
	return childEnv(os.Environ(), true)
}

func childEnv(environ []string, remote bool) []string {
	env := make([]string, 0, len(environmentAllowlist)+len(remoteEnvironmentAllowlist)+len(remoteConfigEnvironment)+3)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		allowed := environmentAllowlist[key] || remote && remoteEnvironmentAllowlist[key]
		if !allowed ||
			remote && isRemoteConfigVariable(key) ||
			repositoryLocalEnv[key] ||
			strings.HasPrefix(key, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(key, "GIT_CONFIG_VALUE_") ||
			key == "GIT_TERMINAL_PROMPT" {
			continue
		}
		env = append(env, kv)
	}
	if remote {
		env = append(env, remoteConfigEnvironment...)
	}
	return append(env, "GIT_GRAFT_FILE=", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
}
