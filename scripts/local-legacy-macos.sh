#!/bin/sh
# Dedicated Go HTTP local instance with the embedded Go script host.
set -eu
[ ! -L "$0" ] || { printf '%s\n' 'toSub2 local: Refusing a symlinked launcher.' >&2; exit 1; }
launcher_dir=$(CDPATH= cd -P -- "$(dirname -- "$0")" && pwd -P)
TOSUB2_LOCAL_INSTANCE=test
TOSUB2_LOCAL_PORT=${TOSUB2_LOCAL_PORT:-4401}
export TOSUB2_LOCAL_INSTANCE TOSUB2_LOCAL_PORT
exec "$launcher_dir/local-macos.sh" "$@"
