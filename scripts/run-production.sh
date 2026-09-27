#!/bin/sh
set -eu
exec "$(dirname "$0")/../bin/tosub2" "$@"
