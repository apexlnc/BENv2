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
  BEN_SMOKE_TIMEOUT   optional   seconds to wait for the run (default 900)
  BEN_SMOKE_KEEP      optional   set to 1 to keep the issue, branch and PR

The two tokens MUST differ (SPEC §10.2): a run holding the tracker credential
can rewrite the queue that dispatched it.
EOF
}

die() {
	printf 'smoke: %s\n' "$1" >&2
	exit 1
}

note() { printf '\n=== %s\n' "$1"; }

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

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	usage
	exit 0
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
timeout_s=${BEN_SMOKE_TIMEOUT:-900}

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

# Refuse BEN's own repository. The dogfood workflow already dispatches against
# it, and a smoke run there would file throwaway issues into the real queue.
if [ -f "$repo_root/WORKFLOW.md" ]; then
	own_repo=$(sed -n 's/^ *repo: *\([^ ]*\) *$/\1/p' "$repo_root/WORKFLOW.md" | head -1)
	if [ -n "$own_repo" ] && [ "$own_repo" = "$BEN_SMOKE_REPO" ]; then
		die "BEN_SMOKE_REPO is BEN's own repository ($own_repo). Use a throwaway canary — see docs/SMOKE.md."
	fi
fi

[ -n "${GITHUB_TOKEN:-}" ] || die "GITHUB_TOKEN is unset; it is the tracker credential (SPEC §10.2)"
[ -n "${GH_TOKEN:-}" ] || die "GH_TOKEN is unset; it is the agent's publish credential (SPEC §10.2)"
[ "$GITHUB_TOKEN" != "$GH_TOKEN" ] || die "GITHUB_TOKEN and GH_TOKEN are the same value. SPEC §10.2 keeps them apart: the tracker credential can rewrite the queue that dispatched the run, so an agent holding it can strip its own labels, take the assignment, and close the issue."

command -v gh >/dev/null 2>&1 || die "gh is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"
command -v claude >/dev/null 2>&1 || die "claude is not on PATH; this profile exists to run the real harness"

# The AGENTS.md audit, run for real rather than described. A persisted `go env`
# setting is invisible to every interactive shell that exports over it and
# visible to everything else — which is exactly what a daemon and its hooks are.
# BEN's first dogfood run died on a `GO111MODULE=off` written two years earlier.
persisted_modules=$(env -i PATH="$PATH" HOME="$HOME" go env GO111MODULE)
if [ "$persisted_modules" = "off" ]; then
	die "GO111MODULE is 'off' for a process that reads no dotfiles (this is what BEN's hooks and agent see). Fix it with: go env -u GO111MODULE — see AGENTS.md, 'Go on a dev machine'."
fi

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
ben="$work/ben"
(cd "$repo_root" && go build -o "$ben" ./cmd/ben)

issue=""
daemon_pid=""

cleanup() {
	local status=$?

	if [ -n "$daemon_pid" ] && kill -0 "$daemon_pid" 2>/dev/null; then
		# SIGTERM, not SIGKILL: the drain is part of what this profile
		# exercises. §11 waits for a confirmed termination of every run's
		# process group and bounds it nowhere — the supervisor's
		# TimeoutStopSec is the bound — so this waits, and says so if it is
		# still waiting.
		note "draining the daemon (SIGTERM)"
		kill -TERM "$daemon_pid" 2>/dev/null || true
		for _ in $(seq 1 60); do
			kill -0 "$daemon_pid" 2>/dev/null || break
			sleep 1
		done
		if kill -0 "$daemon_pid" 2>/dev/null; then
			printf 'smoke: the daemon is still draining after 60s; it is waiting on a run whose process group is unconfirmed (SPEC §9.8). Leaving it: killing it here is the abandonment the drain exists to prevent.\n' >&2
		fi
	fi

	if [ -n "$issue" ] && [ "${BEN_SMOKE_KEEP:-}" != "1" ]; then
		note "cleaning up issue #$issue"
		# The pull request and its branch are the agent's to close: the tracker
		# credential has neither pull-requests nor contents write.
		gh_agent pr close --repo "$BEN_SMOKE_REPO" --delete-branch "ben/$issue" >/dev/null 2>&1 || true
		gh_tracker issue close --repo "$BEN_SMOKE_REPO" "$issue" >/dev/null 2>&1 || true
	elif [ -n "$issue" ]; then
		note "keeping issue #$issue, branch ben/$issue and its pull request (BEN_SMOKE_KEEP=1)"
	fi

	if [ -n "${daemon_log:-}" ] && [ "$status" -ne 0 ] && [ -f "$daemon_log" ]; then
		note "last 40 daemon log lines"
		tail -40 "$daemon_log" >&2
	fi
	printf '\nsmoke: workspace, state and logs left in %s\n' "$work" >&2
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# The workflow. Committed rather than generated so `make workflow-check`
# validates the exact example this profile runs. The two run-specific values
# remain environment references in that file; publish.value is deliberately a
# reference too and is resolved per attempt (SPEC §5.2.8).
# ---------------------------------------------------------------------------

export BEN_SMOKE_WORKSPACE="$work/workspaces"
workflow="$repo_root/scripts/smoke-workflow.md"

note "validating the smoke workflow with BEN's own loader"
"$ben" config effective "$workflow" >"$work/effective.txt" || die "the smoke workflow does not load; see $work/effective.txt"

# ---------------------------------------------------------------------------
# The canary issue. The task is chosen to be completable in one turn with no
# knowledge of the repository, because what is under test is BEN and its two
# adapters — not the agent's ability to do something hard.
# ---------------------------------------------------------------------------

note "preparing the queue label"
gh_tracker label create ben-queue --repo "$BEN_SMOKE_REPO" --color 0e8a16 \
	--description "Dispatchable to BEN" >/dev/null 2>&1 || true

note "filing the canary issue"
issue_url=$(gh_tracker issue create --repo "$BEN_SMOKE_REPO" \
	--title "smoke: add a dated marker file" \
	--label ben-queue \
	--body 'Create a file named `SMOKE.md` at the repository root containing exactly one line:

    BEN smoke test

If the file already exists, append one further line with the same text.

That is the whole task. Do not change anything else.')
issue=${issue_url##*/}
printf 'filed %s\n' "$issue_url"

# ---------------------------------------------------------------------------
# The run.
# ---------------------------------------------------------------------------

note "starting the daemon"
daemon_log="$work/daemon.jsonl"
XDG_STATE_HOME="$work/state" "$ben" run "$workflow" >"$daemon_log" 2>&1 &
daemon_pid=$!
printf 'ben run: pid %s, log %s\n' "$daemon_pid" "$daemon_log"

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
