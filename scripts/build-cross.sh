#!/bin/sh
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$BUILD_ROOT"
./scripts/build-frontend.sh
mkdir -p bin
for target in darwin/arm64 darwin/amd64 linux/amd64 windows/amd64; do
  target_os=${target%/*}
  target_arch=${target#*/}
  artifact="bin/tosub2-$target_os-$target_arch"
  if [ "$target_os" = windows ]; then artifact="$artifact.exe"; fi
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags='-s -w' -o "$artifact" ./cmd/tosub2
  echo "Built $artifact"
done
cp docs/pure-go-runtime/THIRD_PARTY_NOTICES.txt bin/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt
