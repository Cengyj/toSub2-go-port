#!/bin/sh
# Validate an existing production image with static Go test binaries and local
# fixtures only. This does not build/pull an image or run race/live-account tests.
set -eu
umask 077

usage() {
    printf '%s\n' 'Usage: scripts/test-container.sh [--compile-only] [IMAGE]' \
        'Default image: tosub2-go:local'
}

compile_only=0
if [ "$#" -gt 0 ]; then
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --compile-only) compile_only=1; shift ;;
    esac
fi
image=tosub2-go:local
if [ "$#" -gt 0 ]; then image=$1; shift; fi
if [ "$#" -ne 0 ]; then usage >&2; exit 2; fi
case "$image" in ''|-*) usage >&2; exit 2 ;; esac

SCRIPT_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$SCRIPT_ROOT"
command -v docker >/dev/null
command -v go >/dev/null

daemon_info=$(docker info --format '{{.OSType}} {{.Architecture}}')
daemon_os=$(printf '%s\n' "$daemon_info" | awk '{print $1}')
daemon_arch=$(printf '%s\n' "$daemon_info" | awk '{print $2}')
if [ "$daemon_os" != linux ]; then
    printf 'Linux Docker daemon required; got %s.\n' "$daemon_info" >&2
    exit 2
fi
go_arm=
case "$daemon_arch" in
    aarch64|arm64) go_arch=arm64 ;;
    x86_64|amd64) go_arch=amd64 ;;
    i386|i686|386) go_arch=386 ;;
    armv7l|armv7) go_arch=arm; go_arm=7 ;;
    armv6l|armv6) go_arch=arm; go_arm=6 ;;
    ppc64le|s390x|riscv64) go_arch=$daemon_arch ;;
    *) printf 'Unsupported Docker architecture: %s\n' "$daemon_arch" >&2; exit 2 ;;
esac

# --compile-only prepares the same binaries without requiring a ready image.
if [ "$compile_only" -eq 0 ]; then
    image_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
    if [ "$image_platform" != "linux/$go_arch" ]; then
        printf 'Image platform %s differs from daemon target linux/%s.\n' "$image_platform" "$go_arch" >&2
        exit 2
    fi
    image_id=$(docker image inspect --format '{{.Id}}' "$image")
    printf 'Existing image: %s (%s), platform %s\n' "$image" "$image_id" "$image_platform"
fi

tmp_dir=
active_container=
cleanup() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
    if [ -n "$active_container" ]; then
        docker rm -f "$active_container" >/dev/null 2>&1 || true
    fi
    if [ -n "$tmp_dir" ]; then
        if [ -d "$tmp_dir/bin" ]; then chmod u+w "$tmp_dir/bin" || true; fi
        cleanup_failed=0
        for cleanup_suite in jshost legacyruntime cmd; do
            for cleanup_file in "$tmp_dir/bin/$cleanup_suite.test" "$tmp_dir/logs/$cleanup_suite.log"; do
                if [ -f "$cleanup_file" ]; then rm -- "$cleanup_file" || cleanup_failed=1; fi
            done
        done
        for cleanup_dir in "$tmp_dir/bin" "$tmp_dir/logs" "$tmp_dir"; do
            if [ -d "$cleanup_dir" ]; then rmdir -- "$cleanup_dir" || cleanup_failed=1; fi
        done
        if [ "$cleanup_failed" -ne 0 ]; then
            printf 'Failed to remove temporary validation directory: %s\n' "$tmp_dir" >&2
            if [ "$cleanup_status" -eq 0 ]; then cleanup_status=1; fi
        fi
    fi
    exit "$cleanup_status"
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

temp_parent=$(printenv TMPDIR 2>/dev/null || printf /tmp)
if [ -z "$temp_parent" ]; then temp_parent=/tmp; fi
tmp_dir=$(mktemp -d "$temp_parent/tosub2-container.XXXXXXXX")
mkdir "$tmp_dir/bin" "$tmp_dir/logs"
run_tag=$(basename "$tmp_dir" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-')
printf 'Compile static test binaries for linux/%s (CGO disabled; no race instrumentation).\n' "$go_arch"
for suite in jshost legacyruntime cmd; do
    case "$suite" in
        cmd) package=./cmd/tosub2 ;;
        *) package=./internal/$suite ;;
    esac
    CGO_ENABLED=0 GOOS=linux GOARCH="$go_arch" GOARM="$go_arm" \
        go test -c -trimpath -o "$tmp_dir/bin/$suite.test" "$package"
    chmod 0555 "$tmp_dir/bin/$suite.test"
done
# Only compiled, synthetic-fixture binaries are exposed to UID 10001. Logs
# remain in their private host directory; no source, data or credentials mount.
chmod 0755 "$tmp_dir"
chmod 0755 "$tmp_dir/bin"
if [ "$compile_only" -eq 1 ]; then
    printf '%s\n' 'All three static test binaries compiled; temporary files will be removed.'
    exit 0
fi

run_suite() {
    suite=$1
    suite_tests=$2
    test_regex="^($(printf '%s' "$suite_tests" | tr ' ' '|'))$"
    log_file="$tmp_dir/logs/$suite.log"
    active_container="tosub2-check-$run_tag-$suite"
    printf 'Run %s fixtures: network=none, uid=10001.\n' "$suite"
    # Never forward the host environment: these explicit flags contain no
    # credentials. --pull=never preserves the chosen, already-built image.
    if docker run --rm --pull=never --name "$active_container" --init \
        --network none --read-only --user 10001:10001 --cap-drop ALL \
        --security-opt no-new-privileges --shm-size 512m --pids-limit 512 \
        --tmpfs /tmp:rw,nosuid,nodev,mode=1777,size=512m \
        --tmpfs /data:rw,uid=10001,gid=10001,mode=700 \
        --mount "type=bind,source=$tmp_dir/bin,target=/checks,readonly" \
        --no-healthcheck --workdir /data \
        --env TOSUB2_LEGACY_RUNTIME_TEST=1 \
        --env TOSUB2_KEYCHAIN_TEST=0 --env TOSUB2_LONG_MONITOR_TEST=0 \
        --env TOSUB2_LONG_TRANSPORT_TEST=0 \
        --entrypoint "/checks/$suite.test" "$image" \
        -test.v -test.count=1 -test.timeout=4m -test.run "$test_regex" \
        >"$log_file" 2>&1; then
        suite_status=0
    else
        suite_status=$?
    fi
    cat "$log_file"
    if [ "$suite_status" -ne 0 ]; then
        printf '%s fixture container failed (exit %s).\n' "$suite" "$suite_status" >&2
        return "$suite_status"
    fi
    active_container=
    if grep -E '(^|[[:space:]])--- SKIP:' "$log_file" >/dev/null; then
        printf '%s fixtures skipped a selected test; acceptance failed.\n' "$suite" >&2
        return 1
    fi
    for test_name in $suite_tests; do
        if ! grep -Fx -- "=== RUN   $test_name" "$log_file" >/dev/null || \
           ! grep -F -- "--- PASS: $test_name (" "$log_file" >/dev/null; then
            printf 'Required test did not run and pass: %s\n' "$test_name" >&2
            return 1
        fi
    done
}

# Anchor exact names: helper tests are started only by their owning recovery
# tests with explicit synthetic environment, never selected on their own.
run_suite jshost 'TestDOMEventPropagationAndTrust TestWorkerScriptExceptionsAndRejectionsNeverPrintSecrets TestWorkerOwnDeadlineAndParentCancellationRecover'
run_suite legacyruntime 'TestGoSentinelPureGoSharesCookiesAndBrowserIdentity TestGoSentinelInvalidSDKAndTokensFailClosed TestGoCloudflarePureGoParentChildBinaryAndClearance TestGoCloudflareRejectsStaleClearanceAndFailedVerification TestGoSentinelPureGoCancellationTimeoutAndSessionReuse TestGoCloudflarePureGoCancellationTimeoutAndSessionReuse'
run_suite cmd 'TestLegacyRuntimeProductionLoginAndResume'
printf '%s\n' 'All selected production-image fixtures ran and passed without skips (static binaries; no race/live validation).'
