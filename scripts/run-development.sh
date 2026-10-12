#!/bin/sh
# Use the committed static frontend assets embedded in the Go executable.
set -eu
DEV_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$DEV_ROOT"
exec go run ./cmd/tosub2 "$@"
