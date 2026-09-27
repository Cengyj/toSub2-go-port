#!/bin/sh
# Building needs Go; production runs with macOS system tools and Chrome.
set -eu
umask 077
fail() { printf 'toSub2 local: %s\n' "$*" >&2; exit 1; }
usage() {
  printf '%s\n' 'Offline import: scripts/local-macos.sh import-credentials <legacy-directory> <job-meta.json-or-email-list>'
  printf '%s\n' 'Usage: scripts/local-macos.sh start|stop|status|restart|clean --yes' \
    'TOSUB2_LOCAL_PORT sets the local HTTP port on start/restart (default 4400).' \
    'TOSUB2_LOCAL_REBUILD=1 rebuilds on start/restart; an existing binary otherwise needs no Go.' \
    'CHROME_PATH optionally selects Chrome. Runtime: $HOME/Library/Application Support/toSub2-local/<project-hash>.'
}
command_name=${1:-}
case "$command_name" in
  start|stop|status|restart) [ "$#" -eq 1 ] || { usage; exit 2; } ;;
  clean) [ "$#" -eq 2 ] && [ "$2" = --yes ] || { usage; exit 2; } ;;
  import-credentials) [ "$#" -eq 3 ] || { usage; exit 2; } ;;
  *) usage; exit 2 ;;
esac
[ "$(uname -s)" = Darwin ] || fail 'This command requires a logged-in macOS desktop session.'
[ ! -L "$0" ] || fail 'Refusing a symlinked launcher.'
project_dir=$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd -P)
[ "$project_dir" != / ] || fail 'The filesystem root cannot be a project runtime.'
case "$project_dir" in *'
'*) fail 'Project paths containing line breaks are not supported.' ;; esac
user_id=$(id -u)
[ "$user_id" != 0 ] || fail 'Run as the logged-in desktop user, not root.'
project_hash=$(printf '%s' "$project_dir" | shasum -a 256 | awk '{print substr($1, 1, 16)}')
case "$project_hash" in ''|*[!0-9a-f]*) fail 'Could not derive the project namespace.' ;; esac
[ "${#project_hash}" -eq 16 ] || fail 'Invalid project namespace.'
case "$HOME" in /*) : ;; *) fail 'HOME must be an absolute macOS home directory.' ;; esac
[ "$HOME" != / ] || fail 'The filesystem root cannot be a macOS home directory.'
runtime_parent="$HOME/Library/Application Support/toSub2-local"
runtime_root="$runtime_parent/$project_hash"
namespace="com.local.chatgpt-onboarding.credentials.local.$project_hash"
label="com.tosub2.local.$project_hash"
domain="gui/$user_id"
target="$domain/$label"
binary="$runtime_root/bin/tosub2"
plist="$runtime_root/launchd/$label.plist"
marker="$runtime_root/.owner"
pid_file="$runtime_root/service.pid"
port_file="$runtime_root/port"
lock_dir="$runtime_root/.operation-lock"
lock_active=0
stop_deadline=0
expected_marker=$(printf 'tosub2-local-runtime-v1\nproject=%s\nnamespace=%s\nlabel=%s\n' "$project_dir" "$namespace" "$label")
assert_directory() {
  [ ! -L "$1" ] || fail "Refusing symlinked directory: $1"
  [ -d "$1" ] || fail "Expected a directory: $1"
  [ "$(stat -f '%u' "$1")" = "$user_id" ] || fail "Directory belongs to another user: $1"
}
assert_file_if_present() {
  [ ! -L "$1" ] || fail "Refusing symlinked file: $1"
  if [ -e "$1" ]; then
    [ -f "$1" ] || fail "Expected a regular file: $1"
    [ "$(stat -f '%u' "$1")" = "$user_id" ] || fail "File belongs to another user: $1"
    [ "$(stat -f '%l' "$1")" = 1 ] || fail "Refusing hard-linked runtime file: $1"
  fi
}
validate_runtime_parent() {
  [ ! -L "$runtime_parent" ] || fail "Refusing symlinked runtime parent: $runtime_parent"
  if [ -e "$runtime_parent" ]; then assert_directory "$runtime_parent"; fi
}
validate_runtime() {
  validate_runtime_parent
  assert_directory "$runtime_root"
  assert_file_if_present "$marker"
  [ -f "$marker" ] || fail 'Runtime owner marker is missing; no files were removed.'
  [ "$(cat "$marker")" = "$expected_marker" ] || fail 'Runtime owner marker does not match this project.'
  for part in bin data data/credentials logs tmp launchd .operation-lock; do
    item="$runtime_root/$part"
    [ ! -L "$item" ] || fail "Refusing symlinked runtime directory: $item"
    if [ -e "$item" ]; then assert_directory "$item"; fi
  done
  for item in "$binary" "$plist" "$pid_file" "$port_file" "$runtime_root/logs/stdout.log" "$runtime_root/logs/stderr.log"; do
    assert_file_if_present "$item"
  done
}
release_lock() {
  if [ "$lock_active" = 1 ] && [ ! -L "$lock_dir" ] && [ -d "$lock_dir" ]; then
    rm -f -- "$lock_dir/pid"
    rmdir -- "$lock_dir" 2>/dev/null || :
  fi
}
trap release_lock 0
trap 'exit 130' INT
trap 'exit 143' TERM HUP
acquire_lock() {
  if ! mkdir "$lock_dir" 2>/dev/null; then
    assert_directory "$lock_dir"
    assert_file_if_present "$lock_dir/pid"
    [ -f "$lock_dir/pid" ] || fail "A lifecycle operation has an incomplete lock; inspect $lock_dir."
    previous_pid=$(cat "$lock_dir/pid")
    case "$previous_pid" in ''|*[!0-9]*|0|1) fail 'Invalid lifecycle lock; refusing to replace it.' ;; esac
    if kill -0 "$previous_pid" 2>/dev/null; then fail 'Another local lifecycle operation is still running.'; fi
    rm -- "$lock_dir/pid"
    rmdir -- "$lock_dir" || fail 'Unexpected files in the lifecycle lock.'
    mkdir "$lock_dir" || fail 'Could not acquire the lifecycle lock.'
  fi
  lock_active=1
  printf '%s\n' "$$" > "$lock_dir/pid"
}
ensure_runtime() {
  validate_runtime_parent
  if [ ! -e "$runtime_parent" ]; then
    [ -d "$HOME/Library/Application Support" ] || fail 'The macOS Application Support directory is unavailable.'
    mkdir "$runtime_parent" || fail 'Could not create the dedicated local runtime parent.'
  fi
  assert_directory "$runtime_parent"
  chmod 700 "$runtime_parent"
  [ ! -L "$runtime_root" ] || fail "Refusing symlinked runtime: $runtime_root"
  if [ ! -e "$runtime_root" ]; then
    mkdir "$runtime_root" || fail 'Could not create the local runtime.'
    (set -C; printf '%s\n' "$expected_marker" > "$marker") || fail 'Could not safely create the runtime owner marker.'
  fi
  validate_runtime
  acquire_lock
  validate_runtime
  for part in bin data data/credentials logs tmp launchd; do
    item="$runtime_root/$part"
    if [ ! -e "$item" ]; then mkdir "$item"; fi
    assert_directory "$item"
    chmod 700 "$item"
  done
  chmod 700 "$runtime_root"
  chmod 600 "$marker"
}
# Apply the same wall-clock budget to bootout, inspection, and PID checks.
# The only directly signaled processes here are our own short-lived CLI children.
run_with_deadline() {
  if [ "$stop_deadline" -eq 0 ]; then "$@"; return; fi
  [ "$(date +%s)" -lt "$stop_deadline" ] || return 124
  "$@" &
  bounded_pid=$!
  while kill -0 "$bounded_pid" 2>/dev/null; do
    if [ "$(date +%s)" -ge "$stop_deadline" ]; then
      kill -TERM "$bounded_pid" 2>/dev/null || :
      kill -KILL "$bounded_pid" 2>/dev/null || :
      wait "$bounded_pid" 2>/dev/null || :
      return 124
    fi
    sleep 0.05
  done
  wait "$bounded_pid"
}
# A matching label must point to this exact plist and executable. Never kill by
# process name or unload any other project's service.
read_service() {
  service_loaded=0
  service_pid=
  if service_output=$(run_with_deadline launchctl print "$target" 2>/dev/null); then
    service_loaded=1
    service_program=$(printf '%s\n' "$service_output" | awk '/^[[:space:]]*program = / {sub(/^[[:space:]]*program = /, ""); sub(/^"/, ""); sub(/"$/, ""); print; exit}')
    service_path=$(printf '%s\n' "$service_output" | awk '/^[[:space:]]*path = / {sub(/^[[:space:]]*path = /, ""); sub(/^"/, ""); sub(/"$/, ""); print; exit}')
    [ "$service_program" = "$binary" ] && [ "$service_path" = "$plist" ] || fail 'The launchd label points outside this owned runtime; refusing to manage it.'
    service_pid=$(printf '%s\n' "$service_output" | awk '/^[[:space:]]*pid = [0-9]+/ {print $3; exit}')
    case "$service_pid" in ''|0|1) service_pid= ;; *[!0-9]*) fail 'Invalid launchd PID.' ;; esac
  else
    result=$?
    [ "$result" -ne 124 ] || fail 'Stop exceeded 30 seconds; runtime files were retained.'
    [ "$result" -eq 113 ] || fail 'Cannot inspect this launchd service; no service or files were changed.'
  fi
}
read_saved_pid() {
  saved_pid=
  if [ -f "$pid_file" ]; then
    saved_pid=$(cat "$pid_file")
    case "$saved_pid" in ''|0|1|*[!0-9]*) fail 'Invalid saved service PID.' ;; esac
  fi
}
owned_pid_alive() {
  [ -n "$1" ] || return 1
  kill -0 "$1" 2>/dev/null || return 1
  if process_owner=$(run_with_deadline ps -p "$1" -o uid= 2>/dev/null); then
    process_owner=$(printf '%s' "$process_owner" | tr -d '[:space:]')
  else
    result=$?
    [ "$result" -ne 124 ] || fail 'Stop exceeded 30 seconds; runtime files were retained.'
    if kill -0 "$1" 2>/dev/null; then fail 'Cannot verify the live service PID; runtime files were retained.'; fi
    return 1
  fi
  [ "$process_owner" = "$user_id" ] || return 1
  if process_command=$(run_with_deadline ps -ww -p "$1" -o command= 2>/dev/null); then :; else
    result=$?
    [ "$result" -ne 124 ] || fail 'Stop exceeded 30 seconds; runtime files were retained.'
    if kill -0 "$1" 2>/dev/null; then fail 'Cannot verify the live service PID; runtime files were retained.'; fi
    return 1
  fi
  case "$process_command" in "$binary --host=127.0.0.1 --port="*) return 0 ;; *) return 1 ;; esac
}
save_service_pid() {
  [ -n "$service_pid" ] || return 0
  assert_file_if_present "$pid_file"
  printf '%s\n' "$service_pid" > "$pid_file"
  chmod 600 "$pid_file"
}
stop_service() {
  stop_deadline=$(( $(date +%s) + 30 ))
  read_service
  read_saved_pid
  stopping_pid=$saved_pid
  if [ "$service_loaded" = 1 ]; then
    if [ -n "$service_pid" ]; then stopping_pid=$service_pid; save_service_pid; fi
    if run_with_deadline launchctl bootout "$target" >/dev/null 2>&1; then
      :
    else
      result=$?
      [ "$result" -ne 124 ] || fail 'Stop exceeded 30 seconds; runtime files were retained.'
      read_service
      [ "$service_loaded" = 0 ] || fail 'launchd refused to unload this service; runtime files were retained.'
    fi
  fi
  attempt=0
  while [ "$attempt" -lt 150 ] && [ "$(date +%s)" -lt "$stop_deadline" ]; do
    read_service
    if [ -n "$service_pid" ]; then stopping_pid=$service_pid; save_service_pid; fi
    if [ "$service_loaded" = 0 ] && ! owned_pid_alive "$stopping_pid"; then
      rm -f -- "$pid_file"
      stop_deadline=0
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.2
  done
  fail 'Service did not fully unload and exit within 30 seconds; runtime files were retained. Retry stop after it exits.'
}
xml_escape() {
  printf '%s' "$1" | sed -e 's/\&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"
}
write_plist() {
  staged_plist=$(mktemp "$runtime_root/launchd/.plist.XXXXXX")
  cat > "$staged_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>ProgramArguments</key><array>
<string>$(xml_escape "$binary")</string>
<string>--host=127.0.0.1</string><string>--port=$port</string>
<string>--output-root=$(xml_escape "$runtime_root/data")</string>
<string>--browser=true</string>
</array>
<key>WorkingDirectory</key><string>$(xml_escape "$runtime_root")</string>
<key>EnvironmentVariables</key><dict>
<key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string>
<key>HOME</key><string>$(xml_escape "$HOME")</string>
<key>TMPDIR</key><string>$(xml_escape "$runtime_root/tmp/")</string>
<key>TOSUB2_BROWSER_ENABLED</key><string>1</string>
<key>TOSUB2_BROWSER_HEADLESS</key><string>0</string>
<key>TOSUB2_BROWSER_NO_SANDBOX</key><string>0</string>
<key>TOSUB2_MAC_KEYCHAIN_SERVICE</key><string>$namespace</string>
<key>TOSUB2_MAC_CREDENTIAL_ROOT</key><string>$(xml_escape "$runtime_root/data/credentials")</string>
<key>CHROME_PATH</key><string>$(xml_escape "${CHROME_PATH:-}")</string>
</dict>
<key>StandardOutPath</key><string>$(xml_escape "$runtime_root/logs/stdout.log")</string>
<key>StandardErrorPath</key><string>$(xml_escape "$runtime_root/logs/stderr.log")</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><false/>
<key>ProcessType</key><string>Interactive</string>
<key>ExitTimeOut</key><integer>30</integer>
<key>Umask</key><integer>63</integer>
</dict></plist>
PLIST
  plutil -lint "$staged_plist" >/dev/null || fail 'Generated launchd configuration is invalid.'
  chmod 600 "$staged_plist"
  mv -f -- "$staged_plist" "$plist"
}
show_running() {
  display_port=4400
  if [ -f "$port_file" ]; then display_port=$(cat "$port_file"); fi
  case "$display_port" in ''|*[!0-9]*) fail 'Invalid saved local port.' ;; esac
  printf 'Running: http://127.0.0.1:%s (PID %s)\nRuntime: %s\n' "$display_port" "$service_pid" "$runtime_root"
}
build_binary() {
  case "${TOSUB2_LOCAL_REBUILD:-0}" in 0|1) : ;; *) fail 'TOSUB2_LOCAL_REBUILD must be 0 or 1.' ;; esac
  if [ ! -x "$binary" ] || [ "${TOSUB2_LOCAL_REBUILD:-0}" = 1 ]; then
    command -v go >/dev/null 2>&1 || fail 'Go is required for the first build or TOSUB2_LOCAL_REBUILD=1.'
    build_dir=$(mktemp -d "$runtime_root/tmp/build.XXXXXX")
    printf '%s\n' 'Building the Go executable with embedded frontend assets...'
    if ! (cd "$project_dir" && TMPDIR="$runtime_root/tmp/" GOTMPDIR="$build_dir" go build -trimpath -ldflags='-s -w' -o "$build_dir/tosub2" ./cmd/tosub2); then
      rm -rf -- "$build_dir"
      fail 'Go build failed; no service was started.'
    fi
    [ -f "$build_dir/tosub2" ] && [ ! -L "$build_dir/tosub2" ] || fail 'Go build did not produce a regular executable.'
    chmod 700 "$build_dir/tosub2"
    mv -f -- "$build_dir/tosub2" "$binary"
    rmdir "$build_dir"
  fi
}
import_credentials() {
  read_service
  [ "$service_loaded" = 0 ] || fail 'Stop this local service before importing credentials; no running task was stopped.'
  read_saved_pid
  if owned_pid_alive "$saved_pid"; then
    fail 'The local service is still exiting; wait for stop to finish before importing credentials.'
  fi
  build_binary
  # Explicit offline import only; service credential lookup stays isolated.
  TOSUB2_MAC_KEYCHAIN_SERVICE="$namespace" \
  TOSUB2_MAC_CREDENTIAL_ROOT="$runtime_root/data/credentials" \
  TMPDIR="$runtime_root/tmp/" \
    "$binary" "--import-legacy-credentials=$1" "--credential-emails-file=$2" "--output-root=$runtime_root/data"
}
start_service() {
  read_service
  if [ "$service_loaded" = 1 ] && owned_pid_alive "$service_pid"; then
    show_running
    return 0
  fi
  # Wait for asynchronous unload/PID exit before bootstrap, including retries
  # after an interrupted stop. Repeated start never rebuilds a running service.
  stop_service
  port=${TOSUB2_LOCAL_PORT:-4400}
  case "$port" in ''|*[!0-9]*) fail 'TOSUB2_LOCAL_PORT must be an integer from 1 to 65535.' ;; esac
  [ "${#port}" -le 5 ] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || fail 'TOSUB2_LOCAL_PORT must be from 1 to 65535.'
  port=$(printf '%s' "$port" | sed 's/^0*//')
  command -v lsof >/dev/null 2>&1 || fail 'macOS lsof is required to verify the local listener.'
  launchctl print "$domain" >/dev/null 2>&1 || fail 'No logged-in GUI launchd session is available.'
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN -t >/dev/null 2>&1; then
    fail "Port $port already has a listener; choose TOSUB2_LOCAL_PORT or stop that service explicitly."
  else
    result=$?
    [ "$result" -eq 1 ] || fail 'Could not safely inspect the local HTTP port.'
  fi
  build_binary
  write_plist
  printf '%s\n' "$port" > "$port_file"
  chmod 600 "$port_file"
  for log in "$runtime_root/logs/stdout.log" "$runtime_root/logs/stderr.log"; do
    touch "$log"
    chmod 600 "$log"
  done
  launchctl bootstrap "$domain" "$plist" >/dev/null 2>&1 || fail 'launchd bootstrap failed; inspect the local logs and retry start after stop.'
  attempt=0
  while [ "$attempt" -lt 100 ]; do
    read_service
    if [ "$service_loaded" = 1 ] && owned_pid_alive "$service_pid"; then
      save_service_pid
      listener_pid=$(lsof -nP -a -p "$service_pid" -iTCP@"127.0.0.1:$port" -sTCP:LISTEN -t 2>/dev/null || :)
      if [ "$listener_pid" = "$service_pid" ] && curl --noproxy '*' --fail --silent --max-time 1 "http://127.0.0.1:$port/api/health" >/dev/null 2>&1; then
        show_running
        return 0
      fi
    fi
    attempt=$((attempt + 1))
    sleep 0.2
  done
  fail "Service did not become healthy; inspect $runtime_root/logs. Use stop or clean --yes to remove this instance."
}

validate_runtime_parent
[ ! -L "$runtime_root" ] || fail "Refusing symlinked runtime: $runtime_root"
if [ ! -e "$runtime_root" ] && [ "$command_name" != start ] && [ "$command_name" != restart ] && [ "$command_name" != import-credentials ]; then
  printf '%s\n' 'No owned local runtime exists; nothing changed.'
  exit 0
fi
ensure_runtime
case "$command_name" in
  start) start_service ;;
  import-credentials) import_credentials "$2" "$3" ;;
  restart) stop_service; start_service ;;
  stop) stop_service; printf '%s\n' 'Stopped this project local service.' ;;
  status)
    read_service
    if [ "$service_loaded" = 1 ] && owned_pid_alive "$service_pid"; then show_running; else
      read_saved_pid
      if owned_pid_alive "$saved_pid"; then
        printf 'Stopping: PID %s has not exited. Runtime: %s\n' "$saved_pid" "$runtime_root"
      else printf 'Stopped. Runtime: %s\n' "$runtime_root"; fi
    fi
    ;;
  clean)
    stop_service
    validate_runtime
    command -v security >/dev/null 2>&1 || fail 'macOS security is unavailable; runtime files were retained.'
    while :; do
      if security delete-generic-password -s "$namespace" >/dev/null 2>&1; then
        :
      else
        result=$?
        [ "$result" -eq 44 ] && break
        fail 'Keychain cleanup failed; the local runtime was retained. Unlock Keychain and retry clean --yes.'
      fi
    done
    validate_runtime
    # Keep the lock until deletion finishes. Invalidate the marker first so a
    # concurrently paused start cannot claim the directory while rm removes it.
    cleaning_marker=$(printf '%s\ncleaning=true\n' "$expected_marker")
    printf '%s\n' "$cleaning_marker" > "$marker"
    if ! rm -rf -- "$runtime_root"; then
      if [ ! -L "$runtime_root" ] && [ -d "$runtime_root" ] && [ ! -L "$marker" ] && [ -f "$marker" ] && [ "$(cat "$marker")" = "$cleaning_marker" ]; then
        printf '%s\n' "$expected_marker" > "$marker"
      fi
      fail "Some runtime files could not be removed; inspect $runtime_root before retrying."
    fi
    lock_active=0
    validate_runtime_parent
    # Remove only our dedicated parent when empty. A sibling runtime keeps it.
    rmdir -- "$runtime_parent" 2>/dev/null || :
    printf 'Removed this project runtime and Keychain namespace: %s\n' "$runtime_root"
    ;;
esac
