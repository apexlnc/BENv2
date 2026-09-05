#!/usr/bin/env bash
# SPEC §12.4 — the real-integration smoke profile.
#
# One scripted issue, end to end on a canary repository, through the two things
# CI can never exercise: a real coding-agent harness and a real GitHub. It is
# RECOMMENDED and deliberately not CI-required (SPEC §12.4) — it needs
# credentials, spends tokens, and writes to a repository — so it is a command a
# human or a nightly job runs, and `make check` stays the definition of green.
#
# What it proves that the invariant suite cannot: that the two adapters still
# match the world. The §12.3 invariant suite models the outside world through
# internal/fake, and a fake is faithful to the adapter it stands in for only as
# of the last time somebody checked. Harness drift — a renamed stream field, a
# changed exit convention, a new required flag — is invisible to every test in
# this repository until this one runs.
#
# See docs/SMOKE.md for what to set up and how to read the result.

set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage: make smoke   [or: scripts/smoke.sh]

Runs one scripted issue end to end on a canary repository, with a real agent
harness. Configured entirely by environment:

  BEN_SMOKE_REPO      required   owner/name of the canary repository
  GITHUB_TOKEN        required   tracker credential — issues rw + contents read
  GH_TOKEN            required   agent publish credential — contents + PRs, NOT issues
  ANTHROPIC_API_KEY    required   Claude credential for the bounded srt posture
  BEN_SMOKE_TIMEOUT   optional   seconds to wait for the run (default 900)
  BEN_SMOKE_READY_TIMEOUT
                      optional   seconds to wait for local readiness (default 120)
  BEN_SMOKE_KEEP      optional   set to 1 to keep the issue, branch and PR

The two tokens MUST differ (SPEC §10.2): a run holding the tracker credential
can rewrite the queue that dispatched it.

  --check-repo-guard REPO FILE
                      apply the own-repository refusal to REPO, reading FILE as
                      a `ben config effective` rendering, and run nothing else.
                      The self-check internal/arch drives; needs no credentials.
  --check-runtime-layout DIR
                      print the three runtime roots derived from DIR and run
                      nothing else. The self-check internal/arch drives.
  --check-workflow-label SOURCE DEST LABEL
                      render LABEL into SOURCE at the one smoke-label marker.
                      The self-check internal/arch drives.
EOF
}

die() {
	printf 'smoke: %s\n' "$1" >&2
	exit 1
}

note() { printf '\n=== %s\n' "$1"; }

# One temporary tree, three siblings. The daemon receives agent_tmp as TMPDIR,
# so its state_home (and the scratch repositories beneath it) is not reachable
# through the inherited temporary-directory grant.
configure_runtime_layout() {
	smoke_workspace=$1/workspaces
	smoke_state_home=$1/state
	smoke_agent_tmp=$1/agent-tmp
}

# required_labels is deliberately literal config, not an environment-expanding
# field. Render the one generated value into a temporary workflow and refuse if
# the committed marker stops being exactly one list item. The generated label's
# alphabet makes this scalar valid without YAML quoting or escaping.
smoke_label_marker=__BEN_SMOKE_QUEUE_LABEL__
render_smoke_workflow() {
	local source=$1 destination=$2 label=$3 count
	case "$label" in
	'' | *[!a-zA-Z0-9._-]*)
		printf 'invalid smoke queue label %q\n' "$label" >&2
		return 1
		;;
	esac
	count=$(grep -Fxc "    - $smoke_label_marker" "$source" || true)
	if [ "$count" -ne 1 ]; then
		printf '%s contains %s exact smoke queue-label markers, want 1\n' "$source" "$count" >&2
		return 1
	fi
	awk -v target="    - $smoke_label_marker" -v replacement="    - $label" '
		$0 == target { print replacement; next }
		{ print }
	' "$source" >"$destination"
}

# GitHub repository names are case-insensitive, so every comparison of one is
# folded. `tr`, not ${var,,}: this script runs on whatever bash a maintainer's
# machine has, and macOS still ships 3.2.
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# tracker_repo reads the repository BEN's own workflow dispatches against out of
# a `ben config effective` rendering, printing it on success and a reason on
# failure.
#
# From the loader's own output rather than from WORKFLOW.md directly, because
# the file is YAML and this is a shell script: the `sed` this replaced matched
# `^ *repo: *([^ ]*) *$` and therefore missed a quoted value and a value with a
# trailing comment, printing nothing — and printing nothing was how the guard
# below decided the run was safe (#244). `config effective` is credential-free
# and network-free, which is what `make workflow-check` proves.
#
# Exactly one value, or a refusal. Zero means the parse found nothing and is the
# original failure mode; more than one means the rendering grew a second
# tracker.provider.repo and this rule no longer knows which is BEN's.
tracker_repo() {
	rendered=$1
	if [ ! -r "$rendered" ]; then
		printf 'the effective-config rendering (%s) cannot be read\n' "$rendered"
		return 1
	fi
	# Scoped to tracker.provider, because `repo` is a provider key and another
	# block may legitimately grow one. A column-0 line ends the section it opens.
	values=$(awk '
		/^[^[:space:]]/ { tracker = ($1 == "tracker:"); provider = 0 }
		tracker && $1 == "provider:" { provider = 1; next }
		tracker && provider && $1 == "repo:" && NF >= 2 { print $2 }
	' "$rendered")
	count=$(printf '%s\n' "$values" | grep -c . || true)
	if [ "$count" -ne 1 ]; then
		printf 'found %s tracker.provider.repo values in %s, want exactly one\n' "$count" "$rendered"
		return 1
	fi
	printf '%s' "$values"
}

# refuse_own_repository refuses a canary that is BEN's own repository. The
# dogfood workflow already dispatches against it, and a smoke run there would
# file throwaway issues into the real queue.
#
# It fails closed in all three directions #244 found open: a rendering it cannot
# read, a value it cannot parse, and a value that is the same repository under a
# different spelling.
refuse_own_repository() {
	candidate=$1
	own_repo=$(tracker_repo "$2") || die "cannot read BEN's own repository, so the refusal that keeps a smoke run out of the dogfood queue cannot be made: $own_repo. This guard refuses rather than proceeds — see docs/SMOKE.md."
	case "$own_repo" in
	*/*) ;;
	*) die "BEN's own repository parsed as '$own_repo', which is not owner/name. A redacted or malformed value cannot be compared, so this refuses — see docs/SMOKE.md." ;;
	esac
	if [ "$(lower "$own_repo")" = "$(lower "$candidate")" ]; then
		die "BEN_SMOKE_REPO is BEN's own repository (given '$candidate', BEN's own is '$own_repo'). GitHub repository names are case-insensitive, so a different spelling is the same repository. Use a throwaway canary — see docs/SMOKE.md."
	fi
}

# `gh` prefers GH_TOKEN over GITHUB_TOKEN when both are set, and this script sets
# both — so every unqualified `gh` call here would authenticate as the *agent*,
# whose credential deliberately has no Issues permission (SPEC §10.2). Filing and
# reading the canary issue would fail under exactly the scopes docs/SMOKE.md
# tells you to grant.
#
# BEN's own WORKFLOW.md relies on that precedence in the other direction: its
# publish block injects GH_TOKEN so the agent's `gh pr create` authenticates as
# the publisher rather than as the daemon. Same rule, opposite side of the split
# — so the two roles are named here rather than left to whichever variable wins.
#
# Every call goes through one of these. A bare `gh` in this script is a bug.
gh_tracker() { GH_TOKEN="$GITHUB_TOKEN" gh "$@"; }
gh_agent() { GH_TOKEN="$GH_TOKEN" gh "$@"; }

# A scope remains active while any process in its delegated subtree exists,
# including BEN's supervisor and attempt leaves. An unreadable state is not
# quiet: a failed observation must not turn into permission to tear down the
# temporary state or claim that the daemon drained.
daemon_scope_quiet() {
	local state
	state=$(systemctl --user show "$daemon_unit" --property=ActiveState --value 2>/dev/null) || return 1
	case "$state" in
	inactive) return 0 ;;
	*) return 1 ;;
	esac
}

# BEN writes runs.json only after every adapter (including the local execution
# domain) has passed Ready. This observes that publication from the same binary
# and state root rather than treating systemd accepting the transient scope as
# readiness.
await_delegated_readiness() {
	local deadline status candidate
	deadline=$((SECONDS + ready_timeout_s))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if ! kill -0 "$daemon_launcher_pid" 2>/dev/null; then
			wait "$daemon_launcher_pid" 2>/dev/null || true
			die "the daemon exited before publishing ready state; no queue label or issue was created. See $daemon_log"
		fi
		status=$(XDG_STATE_HOME="$smoke_state_home" "$ben" status "$workflow" 2>/dev/null || true)
		candidate=$(printf '%s\n' "$status" | awk '
			$1 == "status" && $2 == "running" {
				for (i = 1; i <= NF; i++) {
					if ($i == "pid") {
						gsub(/,/, "", $(i + 1))
						print $(i + 1)
						exit
					}
				}
			}
		')
		case "$candidate" in
		'' | *[!0-9]*) ;;
		*)
			if kill -0 "$candidate" 2>/dev/null; then
				daemon_pid=$candidate
				printf 'ben run: pid %s, scope %s, log %s\n' "$daemon_pid" "$daemon_unit" "$daemon_log"
				return 0
			fi
			;;
		esac
		sleep 1
	done
	die "the daemon did not publish ready state within ${ready_timeout_s}s; no queue label or issue was created. See $daemon_log"
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	usage
	exit 0
fi

if [ "${1:-}" = "--check-repo-guard" ]; then
	if [ -z "${2:-}" ] || [ -z "${3:-}" ]; then
		usage
		die "--check-repo-guard needs a candidate repository and an effective-config file"
	fi
	refuse_own_repository "$2" "$3"
	exit 0
fi

if [ "${1:-}" = "--check-runtime-layout" ]; then
	[ -n "${2:-}" ] || {
		usage
		die "--check-runtime-layout needs a directory"
	}
	configure_runtime_layout "$2"
	printf 'workspace=%s\nstate_home=%s\nagent_tmp=%s\n' \
		"$smoke_workspace" "$smoke_state_home" "$smoke_agent_tmp"
	exit 0
fi

if [ "${1:-}" = "--check-workflow-label" ]; then
	if [ -z "${2:-}" ] || [ -z "${3:-}" ] || [ -z "${4:-}" ]; then
		usage
		die "--check-workflow-label needs a source, destination and label"
	fi
	render_smoke_workflow "$2" "$3" "$4" || die "could not render the smoke workflow"
	exit 0
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
timeout_s=${BEN_SMOKE_TIMEOUT:-900}
ready_timeout_s=${BEN_SMOKE_READY_TIMEOUT:-120}

# ---------------------------------------------------------------------------
# Preflight. Check everything knowable without running an adapter before the
# canary issue is created. Runtime-only failures can still leave an issue and a
# branch for cleanup, so the point is to reject static mistakes first and keep
# this profile cheap enough to run often.
# ---------------------------------------------------------------------------

[ -n "${BEN_SMOKE_REPO:-}" ] || {
	usage
	die "BEN_SMOKE_REPO is unset. There is deliberately no default: this creates an issue, pushes a branch and opens a pull request, so the target is always named explicitly."
}
case "$BEN_SMOKE_REPO" in
*/*) ;;
*) die "BEN_SMOKE_REPO must be owner/name, got '$BEN_SMOKE_REPO'" ;;
esac

[ -n "${GITHUB_TOKEN:-}" ] || die "GITHUB_TOKEN is unset; it is the tracker credential (SPEC §10.2)"
[ -n "${GH_TOKEN:-}" ] || die "GH_TOKEN is unset; it is the agent's publish credential (SPEC §10.2)"
[ -n "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is unset; sandbox_mode srt denies the logged-in session under HOME, so the smoke harness needs an environment credential"
[ "$GITHUB_TOKEN" != "$GH_TOKEN" ] || die "GITHUB_TOKEN and GH_TOKEN are the same value. SPEC §10.2 keeps them apart: the tracker credential can rewrite the queue that dispatched the run, so an agent holding it can strip its own labels, take the assignment, and close the issue."

command -v gh >/dev/null 2>&1 || die "gh is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"
command -v go >/dev/null 2>&1 || die "go is not on PATH; this profile builds ben and reads its own workflow with it"
command -v claude >/dev/null 2>&1 || die "claude is not on PATH; this profile exists to run the real harness"
command -v srt >/dev/null 2>&1 || die "srt is not on PATH; the smoke workflow uses Claude's bounded sandbox posture so daemon-only Git scratch remains unreachable to the run"

# The AGENTS.md audit, run for real rather than described. A persisted `go env`
# setting is invisible to every interactive shell that exports over it and
# visible to everything else — which is exactly what a daemon and its hooks are.
# BEN's first dogfood run died on a `GO111MODULE=off` written two years earlier.
persisted_modules=$(env -i PATH="$PATH" HOME="$HOME" go env GO111MODULE)
if [ "$persisted_modules" = "off" ]; then
	die "GO111MODULE is 'off' for a process that reads no dotfiles (this is what BEN's hooks and agent see). Fix it with: go env -u GO111MODULE — see AGENTS.md, 'Go on a dev machine'."
fi

# Refuse BEN's own repository, before anything is created. Here rather than
# beside the BEN_SMOKE_REPO shape check above because this one loads a workflow
# with BEN's own binary, and the cheap refusals should have run first.
note "refusing BEN's own repository as the canary"
own_workflow="$repo_root/WORKFLOW.md"
[ -f "$own_workflow" ] || die "$own_workflow is missing, so this cannot tell whether $BEN_SMOKE_REPO is BEN's own repository. That refusal is not optional — restore the file or run this from a checkout that has it."
own_effective=$(mktemp)
trap 'rm -f "$own_effective"' EXIT
if ! (cd "$repo_root" && go run ./cmd/ben config effective "$own_workflow") >"$own_effective" 2>&1; then
	printf 'smoke: %s\n' "$(cat "$own_effective")" >&2
	die "$own_workflow does not load, so the refusal that keeps a smoke run out of the dogfood queue cannot be made"
fi
refuse_own_repository "$BEN_SMOKE_REPO" "$own_effective"
rm -f "$own_effective"
trap - EXIT

# Platform readiness comes after the own-repository refusal — that safety rule
# is unconditional and its live missing-workflow case must fail closed even on
# an unsupported developer host — but before the first GitHub request.
[ "$(uname -s)" = "Linux" ] || die "local smoke requires Linux: the local adapter has no execution-domain fallback on this platform. Use a Linux systemd host or the supervised remote canary in docs/SMOKE.md."
command -v systemd-run >/dev/null 2>&1 || die "systemd-run is not on PATH; local smoke launches BEN in a delegated transient scope"
command -v systemctl >/dev/null 2>&1 || die "systemctl is not on PATH; local smoke verifies and drains its delegated transient scope"
systemctl --user show-environment >/dev/null 2>&1 || die "the systemd user manager is not reachable from this shell. Run from a login session with a user manager; no queue label or issue was created."

# Each credential is asked for the minimum API read on its own side before
# anything is created. Two reads, no side effects — and they are what would
# have caught the bug that made this script's first version unrunnable: every
# `gh` call authenticated as the agent, because gh prefers GH_TOKEN, so filing
# the canary issue failed under the very scopes docs/SMOKE.md says to grant. A
# refusal here names the role, which is the thing a misconfigured run needs told.
note "checking each credential against its own half of the split"
gh_tracker api "repos/$BEN_SMOKE_REPO/issues?per_page=1" >/dev/null 2>&1 ||
	die "the tracker credential (GITHUB_TOKEN) cannot read issues on $BEN_SMOKE_REPO — it needs issues read/write; see docs/SMOKE.md"
gh_agent api "repos/$BEN_SMOKE_REPO/pulls?per_page=1" >/dev/null 2>&1 ||
	die "the agent credential (GH_TOKEN) cannot read pull requests on $BEN_SMOKE_REPO — it needs contents and pull-requests write; see docs/SMOKE.md"

note "building ben"
work=$(mktemp -d)
configure_runtime_layout "$work"
mkdir -p "$smoke_agent_tmp"
ben="$work/ben"
(cd "$repo_root" && go build -o "$ben" ./cmd/ben)

# A not-yet-existing per-run label keeps the now-ready daemon inert until this
# script performs its first forge write. Randomness prevents stale smoke runs
# from sharing a route; the read below independently refuses a collision.
smoke_nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]')
[ "${#smoke_nonce}" -eq 32 ] || die "could not generate the one-run queue-label nonce"
queue_label="ben-smoke-$smoke_nonce"

issue=""
queue_label_created=""
daemon_pid=""
daemon_launcher_pid=""
daemon_unit=""

cleanup() {
	local status=$? scope_confirmed=1 scope_quiet="" daemon_signalled=""

	if [ -n "$daemon_pid" ]; then
		if daemon_scope_quiet; then
			scope_quiet=1
		else
			if kill -0 "$daemon_pid" 2>/dev/null; then
				# SIGTERM, not SIGKILL: the drain is part of what this profile
				# exercises. §11 waits for every run's execution domain to be
				# positively quiet and bounds it nowhere — the supervisor's
				# TimeoutStopSec is the bound — so this waits, and says so if it is
				# still waiting.
				note "draining the daemon (SIGTERM)"
				kill -TERM "$daemon_pid" 2>/dev/null || true
				daemon_signalled=1
			else
				note "the daemon exited; waiting for its execution domain to become quiet"
			fi

			for _ in $(seq 1 60); do
				if daemon_scope_quiet; then
					scope_quiet=1
					break
				fi
				sleep 1
			done
		fi

		if [ -n "$scope_quiet" ]; then
			[ -z "$daemon_launcher_pid" ] || wait "$daemon_launcher_pid" 2>/dev/null || true
		else
			scope_confirmed=""
			if [ -n "$daemon_signalled" ]; then
				printf 'smoke: the daemon is still draining after 60s; a run has not reached confirmed execution-domain quiet (SPEC §9.8). Leaving the scope and forge artifacts intact: killing or deleting them here is the abandonment the drain exists to prevent.\n' >&2
			else
				printf 'smoke: the daemon exited after readiness but its delegated scope is not confirmed inactive after 60s; an execution-domain supervisor or descendant remains unconfirmed (SPEC §9.8). Leaving the scope and forge artifacts intact.\n' >&2
			fi
		fi
	elif [ -z "$daemon_pid" ] && [ -n "$daemon_unit" ]; then
		# Before readiness there cannot be an attempt: the one-run label does
		# not exist yet. Let systemd clean up a partially started scope so a
		# readiness refusal leaves no local process behind.
		systemctl --user stop "$daemon_unit" >/dev/null 2>&1 || true
		[ -z "$daemon_launcher_pid" ] || wait "$daemon_launcher_pid" 2>/dev/null || true
	fi

	if [ -n "$issue" ]; then
		if [ "${BEN_SMOKE_KEEP:-}" = "1" ]; then
			note "keeping issue #$issue, branch ben/$issue, its pull request and label $queue_label (BEN_SMOKE_KEEP=1)"
		elif [ -z "$scope_confirmed" ]; then
			note "keeping issue #$issue, branch ben/$issue, its pull request and label $queue_label because execution-domain quiet is unconfirmed"
		else
			note "cleaning up issue #$issue"
			# The pull request and its branch are the agent's to close: the tracker
			# credential has neither pull-requests nor contents write.
			gh_agent pr close --repo "$BEN_SMOKE_REPO" --delete-branch "ben/$issue" >/dev/null 2>&1 || true
			gh_tracker issue close --repo "$BEN_SMOKE_REPO" "$issue" >/dev/null 2>&1 || true
		fi
	fi

	if [ -n "$queue_label_created" ] && [ "${BEN_SMOKE_KEEP:-}" != "1" ] && [ -n "$scope_confirmed" ]; then
		gh_tracker label delete "$queue_label" --repo "$BEN_SMOKE_REPO" --yes >/dev/null 2>&1 || true
	elif [ -n "$queue_label_created" ] && [ -z "$issue" ] && [ -z "$scope_confirmed" ]; then
		note "keeping queue label $queue_label because execution-domain quiet is unconfirmed"
	fi

	if [ -n "${daemon_log:-}" ] && [ "$status" -ne 0 ] && [ -f "$daemon_log" ]; then
		note "last 40 daemon log lines"
		tail -40 "$daemon_log" >&2
	fi
	printf '\nsmoke: workspace, state and logs left in %s\n' "$work" >&2
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# The workflow source is committed so `make workflow-check` validates its
# structure. Repository and workspace remain environment references, while the
# generated queue label is literal configuration: render its one marker into a
# temporary copy, then validate and run that exact copy. publish.value is also
# deliberately a reference and is resolved per attempt (SPEC §5.2.8).
# ---------------------------------------------------------------------------

export BEN_SMOKE_WORKSPACE="$smoke_workspace"
workflow_source="$repo_root/scripts/smoke-workflow.md"
workflow="$work/smoke-workflow.md"
render_smoke_workflow "$workflow_source" "$workflow" "$queue_label" ||
	die "could not render the one-run queue label into the smoke workflow"

note "validating the smoke workflow with BEN's own loader"
"$ben" config effective "$workflow" >"$work/effective.txt" || die "the smoke workflow does not load; see $work/effective.txt"

# Prove the random route does not already exist before a daemon begins polling
# it. The paginated read fails closed: an API failure is not evidence of
# absence. No GitHub mutation has occurred at this point.
note "checking the one-run queue route"
existing_labels=$(gh_tracker api --paginate "repos/$BEN_SMOKE_REPO/labels?per_page=100" --jq '.[].name') ||
	die "could not prove that the one-run queue label is absent; no queue label or issue was created"
if printf '%s\n' "$existing_labels" | grep -Fqx -- "$queue_label"; then
	die "the generated one-run queue label already exists; refusing to risk dispatching an older canary issue"
fi

# One launch serves as both readiness proof and the actual smoke daemon. A
# scope, unlike a transient service, inherits this shell's environment without
# serializing its credentials into unit properties. Delegate=yes gives BEN the
# cgroup subtree its local-domain Ready canary must prove before runs.json is
# published. The queue route is still absent, so the first tick is inert.
note "starting the daemon in a delegated systemd scope"
daemon_log="$work/daemon.jsonl"
daemon_unit="ben-smoke-$smoke_nonce.scope"
TMPDIR="$smoke_agent_tmp" XDG_STATE_HOME="$smoke_state_home" \
	systemd-run --user --scope --quiet --collect --unit="$daemon_unit" \
	--property=Delegate=yes -- "$ben" run "$workflow" >"$daemon_log" 2>&1 &
daemon_launcher_pid=$!
await_delegated_readiness

# ---------------------------------------------------------------------------
# The canary issue. The task is chosen to be completable in one turn with no
# knowledge of the repository, because what is under test is BEN and its two
# adapters — not the agent's ability to do something hard.
# ---------------------------------------------------------------------------

note "preparing the one-run queue label"
gh_tracker label create "$queue_label" --repo "$BEN_SMOKE_REPO" --color 0e8a16 \
	--description "Dispatchable to one BEN smoke run" >/dev/null 2>&1 ||
	die "could not create the one-run queue label $queue_label"
queue_label_created=1

note "filing the canary issue"
issue_url=$(gh_tracker issue create --repo "$BEN_SMOKE_REPO" \
	--title "smoke: add a dated marker file" \
	--label "$queue_label" \
	--body 'Create a file named `SMOKE.md` at the repository root containing exactly one line:

    BEN smoke test

If the file already exists, append one further line with the same text.

That is the whole task. Do not change anything else.')
issue=${issue_url##*/}
printf 'filed %s\n' "$issue_url"

note "waiting for the published pull request (timeout ${timeout_s}s)"
pr_url=""
deadline=$((SECONDS + timeout_s))
while [ "$SECONDS" -lt "$deadline" ]; do
	if ! kill -0 "$daemon_pid" 2>/dev/null; then
		wait "$daemon_pid" || true
		die "the daemon exited before the run completed; see $daemon_log"
	fi
	pr_url=$(gh_agent pr list --repo "$BEN_SMOKE_REPO" --head "ben/$issue" --state open \
		--json url --jq '.[0].url // empty' 2>/dev/null || true)
	[ -n "$pr_url" ] && break
	sleep 5
done
[ -n "$pr_url" ] || die "no open pull request on branch ben/$issue after ${timeout_s}s. The daemon log and the issue's ben:* label say where it stopped."

# The pull request existing is the agent's work. What BEN owes on top of it is
# the §9.7 verdict: the publish milestone comment, and the state projection
# cleared. Waiting for those is what distinguishes "the agent published" from
# "BEN verified that it did" (SPEC §3.5 — evidence over claims).
note "waiting for BEN's published verdict"
while [ "$SECONDS" -lt "$deadline" ]; do
	comments=$(gh_tracker issue view --repo "$BEN_SMOKE_REPO" "$issue" --json comments --jq '[.comments[].body] | join("\n")')
	labels=$(gh_tracker issue view --repo "$BEN_SMOKE_REPO" "$issue" --json labels --jq '[.labels[].name] | join(",")')
	case "$comments" in
	*"$pr_url"*)
		case "$labels" in
		*ben:*)
			# The comment landed and a state label is still standing — a
			# projection still in flight, or a verdict that is not `done`.
			;;
		*)
			note "PASS"
			printf 'issue:        %s\n' "$issue_url"
			printf 'pull request: %s\n' "$pr_url"
			printf 'labels:       %s\n' "${labels:-<none>}"
			printf '\nBEN claimed the issue, prepared a worktree, ran %s, verified the\n' "$(claude --version 2>/dev/null || echo claude)"
			printf 'three §9.7 legs, posted the publish milestone and cleared its state\nprojection. The claim is retained until the pull request closes (§9.2).\n'
			exit 0
			;;
		esac
		;;
	esac
	sleep 5
done

die "a pull request was published ($pr_url) but BEN did not post its publish milestone within ${timeout_s}s — the §9.7 verification is where this stopped, not the agent. See $daemon_log."
