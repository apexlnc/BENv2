#!/bin/sh
set -eu

# `go run module@version` queries the module proxy for version metadata even
# when every source archive is cached. The benchmark runtime has no network, so
# route the repository's one pinned tool invocation to the binary the trusted
# checker image installed from that exact version. Everything else is ordinary
# Go 1.26.1.
if [ "$#" -ge 2 ] &&
  [ "$1" = "run" ] &&
  [ "$2" = "honnef.co/go/tools/cmd/staticcheck@2026.1" ]; then
  shift 2
  exec /usr/local/bin/staticcheck "$@"
fi

exec /usr/local/go/bin/go "$@"
