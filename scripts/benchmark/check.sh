#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <checker-image> <absolute-checkout> <command>" >&2
  exit 2
fi

image=$1
checkout=$2
command=$3
if ! uid=$(id -u); then
  echo "benchmark check: cannot resolve the controller UID" >&2
  exit 2
fi
if ! gid=$(id -g); then
  echo "benchmark check: cannot resolve the controller GID" >&2
  exit 2
fi

if [ "$uid" -eq 0 ]; then
  echo "benchmark check: run the controller as an unprivileged account" >&2
  exit 2
fi

case "$checkout" in
  /*) ;;
  *) echo "benchmark check: checkout must be an absolute path" >&2; exit 2 ;;
esac
if [ ! -d "$checkout/.git" ]; then
  echo "benchmark check: $checkout is not a standalone git checkout" >&2
  exit 2
fi

# Status is a protocol consumed by docs/BENCH.md: 0 is a passing check, 1 is a
# completed check whose command failed, and every other status is checker
# infrastructure failure. Keep the stopped container long enough to inspect
# runtime state; in particular, an OOM must abort the session rather than become
# evidence against an adapter.
if ! scratch=$(mktemp -d "${TMPDIR:-/tmp}/ben-bench-check.XXXXXX"); then
  echo "benchmark check: cannot create private controller state" >&2
  exit 2
fi
cidfile="$scratch/cid"
cid=
# shellcheck disable=SC2329 # Invoked by the signal and EXIT traps below.
cleanup() {
  if [ -z "$cid" ] && [ -s "$cidfile" ]; then
    cid=$(sed -n '1p' "$cidfile" 2>/dev/null || true)
  fi
  if [ -n "$cid" ]; then
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  rm -f "$cidfile"
  rmdir "$scratch" 2>/dev/null || true
}
# Only the final, fully inspected command verdict may return 1. Normalize an
# unexpected controller command failure before that point to infrastructure.
# shellcheck disable=SC2329 # Invoked by the EXIT trap below.
on_exit() {
  status=$?
  [ "$status" -ne 1 ] || status=2
  trap - EXIT HUP INT TERM
  cleanup
  exit "$status"
}
trap on_exit EXIT
trap 'trap - EXIT HUP INT TERM; cleanup; exit 129' HUP
trap 'trap - EXIT HUP INT TERM; cleanup; exit 130' INT
trap 'trap - EXIT HUP INT TERM; cleanup; exit 143' TERM

# The Docker client is trusted controller code. The command after the image is
# the security boundary: env -i prevents image or host credentials from reaching
# the shell, and the container gets only a read-only checkout plus fresh tmpfs
# storage. It has no network, host home, Docker socket, capabilities, or writable
# image layer. The host account's numeric ID grants read access to the bind mount
# but no host identity or privilege inside the container.
set +e
docker run \
  --cidfile="$cidfile" \
  --network=none \
  --ipc=none \
  --read-only \
  --cap-drop=ALL \
  --security-opt=no-new-privileges \
  --pids-limit=1024 \
  --memory=6g \
  --cpus=4 \
  --user="$uid:$gid" \
  --mount "type=bind,source=$checkout,target=/work,readonly" \
  --tmpfs "/tmp:rw,exec,nosuid,nodev,size=2g,uid=$uid,gid=$gid,mode=1777" \
  --tmpfs "/home/check:rw,nosuid,nodev,size=16m,uid=$uid,gid=$gid,mode=0700" \
  --tmpfs "/cache:rw,exec,nosuid,nodev,size=4g,uid=$uid,gid=$gid,mode=0700" \
  --workdir=/work \
  --entrypoint=/usr/bin/env \
  "$image" \
  -i \
  HOME=/home/check \
  PATH=/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin \
  GOCACHE=/cache/go-build \
  GOMODCACHE=/go/pkg/mod \
  GOPATH=/go \
  GOTOOLCHAIN=local \
  XDG_CACHE_HOME=/cache/xdg \
  TMPDIR=/tmp \
  GIT_CONFIG_COUNT=1 \
  GIT_CONFIG_KEY_0=safe.directory \
  GIT_CONFIG_VALUE_0=/work \
  /bin/bash --noprofile --norc -c \
  'if /bin/bash --noprofile --norc -c "$1"; then exit 0; fi; exit 1' \
  benchmark-check "$command"
run_status=$?
set -e

if [ ! -s "$cidfile" ]; then
  echo "benchmark check: Docker did not create a container (status $run_status)" >&2
  exit 2
fi
cid=$(sed -n '1p' "$cidfile")

if ! oom=$(docker inspect --format '{{.State.OOMKilled}}' "$cid"); then
  echo "benchmark check: cannot inspect container $cid for OOM state" >&2
  exit 2
fi
if ! container_status=$(docker inspect --format '{{.State.ExitCode}}' "$cid"); then
  echo "benchmark check: cannot inspect container $cid exit status" >&2
  exit 2
fi
if ! state_error=$(docker inspect --format '{{.State.Error}}' "$cid"); then
  echo "benchmark check: cannot inspect container $cid runtime error" >&2
  exit 2
fi

if [ "$oom" != false ]; then
  echo "benchmark check: container $cid was OOM-killed" >&2
  exit 2
fi
if [ -n "$state_error" ]; then
  echo "benchmark check: container $cid runtime error: $state_error" >&2
  exit 2
fi
if [ "$container_status" != "$run_status" ]; then
  echo "benchmark check: Docker reported status $run_status, container reported $container_status" >&2
  exit 2
fi
if ! docker rm "$cid" >/dev/null; then
  echo "benchmark check: cannot remove stopped container $cid" >&2
  exit 2
fi
cid=
if ! rm -f "$cidfile" || ! rmdir "$scratch"; then
  echo "benchmark check: cannot remove private controller state" >&2
  exit 2
fi
trap - EXIT HUP INT TERM

case "$run_status" in
  0) exit 0 ;;
  1) exit 1 ;;
  *)
    echo "benchmark check: container did not return a check verdict (status $run_status)" >&2
    exit 2
    ;;
esac
