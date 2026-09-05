#!/bin/sh
set -eu

if [ "$(uname -s)" != Linux ]; then
	echo "test-systemd-localdomain: Linux is required" >&2
	exit 1
fi

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
proof_dir=$(mktemp -d "${TMPDIR:-/tmp}/ben-systemd-localdomain.XXXXXX")

cleanup() {
	rm -f -- "$proof_dir/localdomain.test"
	rmdir -- "$proof_dir"
}
trap cleanup EXIT HUP INT TERM

cd "$repo_root"
go test -c -o "$proof_dir/localdomain.test" ./internal/agent/localdomain
if [ "$(id -u)" -eq 0 ]; then
	env BEN_LOCALDOMAIN_SYSTEMD_PROOF=1 \
		"$proof_dir/localdomain.test" \
		-test.run='^TestShippedSystemdCrashRestart$' -test.v -test.count=1
else
	sudo env BEN_LOCALDOMAIN_SYSTEMD_PROOF=1 \
		"$proof_dir/localdomain.test" \
		-test.run='^TestShippedSystemdCrashRestart$' -test.v -test.count=1
fi
