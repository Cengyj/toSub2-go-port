#!/bin/sh
set -eu

if [ "$(uname -s)" != Darwin ]; then
  printf '%s\n' 'This launcher requires a logged-in macOS desktop session.' >&2
  exit 1
fi

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
binary=${TOSUB2_BINARY:-"$project_dir/bin/tosub2"}
data_root=${TOSUB2_VISIBLE_DATA_ROOT:-"$HOME/Library/Application Support/toSub2/native-visible/data"}
port=${TOSUB2_VISIBLE_PORT:-4400}

if [ ! -x "$binary" ]; then
  printf '%s\n' 'Build the Go binary with ./scripts/build.sh, or set TOSUB2_BINARY to an installed executable.' >&2
  exit 1
fi

umask 077
export TOSUB2_BROWSER_HEADLESS=0
export TOSUB2_BROWSER_NO_SANDBOX=0
exec "$binary" --host=127.0.0.1 --port="$port" --output-root="$data_root" "$@"
