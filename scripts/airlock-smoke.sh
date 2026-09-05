#!/usr/bin/env bash
# The optional, credential-gated Airlock kind smoke (#194).
#
# One fixture command, in one real sandbox, against a real Airlock control
# plane. It is deliberately NOT part of `make check`: it needs credentials and
# network, and `make check` green is the definition of green (AGENTS.md). CI
# stays offline and deterministic because the Go test behind this wrapper skips
# itself when the variables below are unset — and this wrapper refuses to run at
# all without them, so a skip reached from here is a defect rather than a mode.
#
# What it proves that the contract fake cannot: that BEN's client still matches
# the world. internal/airlock/airlocktest is faithful to the frozen v2 contract
# as of the last time somebody checked it, and a contract revision — a renamed
# field, a new required header, a changed status — is invisible to every other
# test in this repository until this one runs.
#
# See docs/AIRLOCK.md.

set -euo pipefail

# The test this wrapper exists to run, and the variables that decide whether it
# runs itself. Both are pinned to internal/airlock/smoke_test.go by
# internal/arch/airlock_smoke_test.go: a rename on either side is otherwise a
# silent success here, because a `go test -run` pattern that matches nothing and
# a test that skips itself both exit 0 (#244).
smoke_test=TestAirlockSmoke
smoke_vars=(BEN_AIRLOCK_URL BEN_AIRLOCK_TOKEN BEN_AIRLOCK_PROFILE)

usage() {
	cat >&2 <<'EOF'
Usage: scripts/airlock-smoke.sh

Runs one fixture command in one real Airlock sandbox. Configured entirely by
environment:

  BEN_AIRLOCK_URL       required   https base URL of the control plane
  BEN_AIRLOCK_TOKEN     required   bearer access token
  BEN_AIRLOCK_PROFILE   required   an approved profile this tenant may provision

The token MUST NOT be a credential this deployment also uses for the tracker or
for publishing: a token that can create and destroy execution environments has
no business also being one scoped to the forge (docs/AIRLOCK.md).

The sandbox it creates is deleted on the way out, including on failure.

  --check-verdict FILE  read FILE as a `go test -v` log and apply this script's
                        own success rule to it, without running anything. The
                        self-check internal/arch drives; needs no credentials.
EOF
}

die() {
	printf 'airlock-smoke: %s\n' "$1" >&2
	exit 1
}

# verdict decides whether a `go test -v` log is evidence the smoke actually ran
# and passed. It is a function, and reachable without credentials through
# --check-verdict, because it is the whole safety property of this script and an
# unrunnable rule is an unasserted one.
#
# Exit 0 only on an explicit PASS line for the named test. Every other shape is
# a refusal with the reason on stdout, and the two that matter are the ones that
# used to be reported as success:
#
#   - No result line at all. `-run` matched nothing (a renamed test, a typo, an
#     unanchored pattern that stopped matching), the package failed to build, or
#     the toolchain is absent. `go test` prints "no tests to run" and exits 0.
#   - SKIP. The test skips itself when a variable is empty — which cannot have
#     happened here, because the preflight below refuses an empty one. So a skip
#     means the variables this script checked are not the variables the test
#     reads, and the smoke that was supposed to gate a contract revision never
#     opened a connection.
verdict() {
	local log=$1 line
	if [[ ! -r $log ]]; then
		printf 'the go test log (%s) cannot be read, so nothing about the run is known\n' "$log"
		return 1
	fi
	# Leading whitespace tolerated: `go test -v` indents a result line under a
	# parent test, and this rule must not depend on that layout.
	line=$(grep -E "^[[:space:]]*--- (PASS|FAIL|SKIP): $smoke_test( |\$)" "$log" | head -1 || true)
	case "$line" in
	*"--- PASS: $smoke_test"*)
		return 0
		;;
	*"--- SKIP: $smoke_test"*)
		printf '%s skipped itself although every variable this script requires was set. It reads a different set than %s — see internal/airlock/smoke_test.go\n' \
			"$smoke_test" "${smoke_vars[*]}"
		return 1
		;;
	*"--- FAIL: $smoke_test"*)
		printf '%s failed against the real control plane; the output above is the drift\n' "$smoke_test"
		return 1
		;;
	*)
		printf 'no result for %s in the go test output: it never ran. A -run pattern that matches nothing exits 0, so this is a refusal rather than a pass\n' \
			"$smoke_test"
		return 1
		;;
	esac
}

if [[ ${1-} == "-h" || ${1-} == "--help" ]]; then
	usage
	exit 0
fi

if [[ ${1-} == "--check-verdict" ]]; then
	if [[ -z ${2-} ]]; then
		usage
		die "--check-verdict needs a go test log to read"
	fi
	reason=$(verdict "$2") || die "$reason"
	exit 0
fi

for var in "${smoke_vars[@]}"; do
	if [[ -z ${!var-} ]]; then
		usage
		die "$var is required"
	fi
done

case "$BEN_AIRLOCK_URL" in
https://*) ;;
*) die "BEN_AIRLOCK_URL must be https: the bearer token and the run's whole output cross this connection" ;;
esac

command -v go >/dev/null 2>&1 || die "go is not on PATH; this wrapper runs the smoke as a Go test"

cd "$(dirname "$0")/.."

log=$(mktemp)
trap 'rm -f "$log"' EXIT

# The token never reaches argv or a log line here; it is passed through the
# environment the Go test reads, and the client sends it only in a header.
#
# The -run pattern is anchored. Unanchored, it is a substring match: it would
# keep matching a renamed TestAirlockSmokeAgainstStaging, and match nothing at
# all once the test is renamed past that — and matching nothing is the exit-0
# this script must never report as success.
if go test ./internal/airlock/ -run "^${smoke_test}\$" -count=1 -v -timeout 15m 2>&1 | tee "$log"; then
	status=0
else
	status=$? # pipefail is set, so this is go test's, not tee's
fi

# The verdict first: it names what happened, where the exit status only says
# whether the toolchain was happy.
reason=$(verdict "$log") || die "$reason"
[[ $status -eq 0 ]] || die "$smoke_test passed but go test exited $status; something else in ./internal/airlock/ failed"
