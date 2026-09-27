#!/bin/sh
# Node and pnpm are build-time tools only. The production binary embeds dist.
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
FRONTEND_SOURCE="$BUILD_ROOT/web"
if [ ! -f "$FRONTEND_SOURCE/package.json" ] || [ ! -f "$FRONTEND_SOURCE/pnpm-lock.yaml" ]; then
  echo "React package.json and pnpm-lock.yaml are required" >&2
  exit 1
fi
NODE_COMMAND=$(command -v "${NODE_BIN:-node}" 2>/dev/null || true)
if [ -z "$NODE_COMMAND" ] || [ ! -x "$NODE_COMMAND" ]; then
  echo "Node.js is required only for this build; set NODE_BIN to its executable" >&2
  exit 1
fi
# pnpm and its subprocesses use /usr/bin/env node. Set PATH before installation.
NODE_DIRECTORY=$(CDPATH= cd -- "$(dirname "$NODE_COMMAND")" && pwd)
PATH="$NODE_DIRECTORY:$PATH"
export PATH
PNPM_COMMAND=$(command -v "${PNPM:-pnpm}" 2>/dev/null || true)
if [ -z "$PNPM_COMMAND" ] || [ ! -x "$PNPM_COMMAND" ]; then
  echo "pnpm is required only for this build; set PNPM to its executable" >&2
  exit 1
fi
(cd "$FRONTEND_SOURCE" && "$PNPM_COMMAND" install --frozen-lockfile --ignore-scripts && "$NODE_COMMAND" ./node_modules/vite/bin/vite.js build)
if [ ! -s "$FRONTEND_SOURCE/dist/index.html" ]; then
  echo "Vite did not produce dist/index.html" >&2
  exit 1
fi
EMBED_PARENT="$BUILD_ROOT/cmd/tosub2/web"
mkdir -p "$EMBED_PARENT"
EMBED_STAGING=$(mktemp -d "$EMBED_PARENT/.dist-build.XXXXXX")
trap 'rm -rf "$EMBED_STAGING"' EXIT HUP INT TERM
cp -R "$FRONTEND_SOURCE/dist/." "$EMBED_STAGING/"
rm -rf "$EMBED_PARENT/dist"
mv "$EMBED_STAGING" "$EMBED_PARENT/dist"
