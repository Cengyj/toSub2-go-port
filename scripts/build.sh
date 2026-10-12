#!/bin/sh
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$BUILD_ROOT"
./scripts/build-frontend.sh
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/tosub2 ./cmd/tosub2
cp docs/pure-go-runtime/THIRD_PARTY_NOTICES.txt bin/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt
