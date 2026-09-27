#!/bin/sh
# Use the committed static assets; rebuild them after changing web/src.
set -eu
DEV_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$DEV_ROOT"
exec go run ./cmd/tosub2 "$@"
