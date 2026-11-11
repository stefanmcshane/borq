#!/usr/bin/env bash
set -euo pipefail
umask 077

cd "$(dirname "$0")/../.."
: "${DEV_DIR:?Run this through make dev, make logs, or make dev-stop}"
session_lock="$DEV_DIR/session.lock"
session_pid="$DEV_DIR/session.pid"
session_log="$DEV_DIR/session.log"

require_tool() {
  command -v "$1" >/dev/null 2>&1 || { echo "Missing development tool: $1" >&2; exit 1; }
}

running() {
  [ -f "$session_pid" ] || return 1
  read -r saved_pid < "$session_pid"
  case "$saved_pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$saved_pid" 2>/dev/null || return 1
  case "$(ps -p "$saved_pid" -o args=)" in *zarf/dev/session.sh\ run*) return 0 ;; *) return 1 ;; esac
}

check_cluster_port() {
  local port="$1" setting="$2" owner
  if ! (true <>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then return; fi
  owner=$(docker ps --filter "publish=$port" --format '{{.Names}}')
  if [ "$owner" = "k3d-$DEV_CLUSTER-serverlb" ]; then return; fi
  echo "Local development port $port is already in use outside cluster $DEV_CLUSTER." >&2
  if [ -n "$owner" ]; then echo "Docker container: $owner" >&2; fi
  if [ -n "$setting" ]; then
    echo "Choose another port with make dev $setting=<free-port>." >&2
  else
    echo "Stop the other Borq API listener on port $port before running make dev." >&2
  fi
  exit 1
}

start() {
  case "$DEV_DETACH" in 0|1) ;; *) echo 'DEV_DETACH must be 0 or 1.' >&2; exit 1 ;; esac
  if running; then
    if [ ! -f "$session_lock/ready" ]; then
      echo 'Development setup is still in progress; follow it with make logs.' >&2
      return
    fi
    echo "Development session is already running. Use make logs or make dev-stop."
    return
  fi
  for tool in go node npm python3 docker kubectl air mirrord curl; do require_tool "$tool"; done
  mkdir -p "$DEV_DIR"
  if ! mkdir "$session_lock" 2>/dev/null; then
    if [ -f "$session_lock/owner" ]; then
      read -r owner < "$session_lock/owner"
      if kill -0 "$owner" 2>/dev/null; then
        echo 'Development setup is already in progress.' >&2
        exit 1
      fi
    fi
    rm -f "$session_lock/owner" "$session_lock/ready"
    rmdir "$session_lock"
    mkdir "$session_lock"
  fi
  echo "$$" > "$session_lock/owner"
  trap 'rm -f "$session_lock/owner" "$session_lock/ready"; rmdir "$session_lock" 2>/dev/null || true' EXIT
  for port in 27833 27834; do
    if (true <>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      echo "Local development port $port is already in use; stop the other development server first." >&2
      exit 1
    fi
  done
  check_cluster_port "$DEV_API_PORT" ''
  check_cluster_port "$DEV_HTTP_PORT" DEV_HTTP_PORT
  check_cluster_port "$DEV_HTTPS_PORT" DEV_HTTPS_PORT
  : > "$session_log"
  # Keep the worker alive when a terminal or task runner cleans up its launcher.
  set -m
  nohup bash zarf/dev/session.sh run >> "$session_log" 2>&1 </dev/null &
  runner=$!
  set +m
  trap 'kill -TERM "$runner" 2>/dev/null || true' EXIT
  # Only wait for the supervisor to register, not for bootstrap or readiness.
  for ((attempt=0; attempt<50; attempt++)); do
    if running; then break; fi
    if ! kill -0 "$runner" 2>/dev/null; then
      cat "$session_log" >&2
      echo 'Development startup failed. See the output above.' >&2
      exit 1
    fi
    sleep 0.1
  done
  if ! running; then
    echo "Development supervisor did not start; inspect $session_log." >&2
    exit 1
  fi
  if [ "$DEV_DETACH" = 1 ]; then
    trap - EXIT
    echo "Development starting in background (PID $runner); API: $DEV_API_URL."
    echo 'Use make logs for progress, make dev-wait to wait for readiness, and make dev-stop to stop.'
    return
  fi
  tail -n +1 -f "$session_log" &
  follower=$!
  trap 'kill -TERM "$runner" "$follower" 2>/dev/null || true' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  wait "$runner"
}

run() {
  task_pid=''
  air_pid=''
  cleanup() {
    for child in "$task_pid" "$air_pid"; do
      if [ -n "$child" ]; then kill -TERM -- "-$child" 2>/dev/null || true; fi
    done
    wait 2>/dev/null || true
    rm -f "$session_pid" "$session_lock/owner" "$session_lock/ready"
    rmdir "$session_lock" 2>/dev/null || true
  }
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  echo "$$" > "$session_pid"
  echo "$$" > "$session_lock/owner"
  # Keep each startup task in its own group so stop also reaches its children.
  set -m
  make --no-print-directory _dev-bootstrap &
  task_pid=$!
  set +m
  wait "$task_pid"
  task_pid=''
  export KUBECONFIG="$DEV_KUBECONFIG" BORQ_KUBECONFIG="$DEV_KUBECONFIG"
  export BORQ_MANAGEMENT_KUBECONFIG="$DEV_KUBECONFIG"
  export BORQ_MANAGEMENT_CLUSTER_NAME="$DEV_CLUSTER"
  node=$(kubectl --kubeconfig "$DEV_KUBECONFIG" get nodes -l node-role.kubernetes.io/control-plane -o 'jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  [ -n "$node" ] || { echo 'Development cluster has no control-plane address.' >&2; exit 1; }
  export BORQ_API_URL="http://$node:31833"
  export BORQ_MANAGEMENT_ENABLED=true BORQ_INFRASTRUCTURE_ENABLED=true
  export BORQ_INFRASTRUCTURE_PROFILE=dev BORQ_INFRASTRUCTURE_STORAGE_CLASS=borq-local BORQ_LOG_LEVEL=debug
  export BORQ_DATA_DIR="$DEV_DIR/data"
  # A separate process group lets shutdown reach Mirrord, Air, and their children.
  set -m
  mirrord exec -f zarf/dev/mirrord.json -- air -c zarf/dev/.air.toml &
  air_pid=$!
  set +m
  for ((elapsed=0; elapsed<300; elapsed++)); do
    kill -0 "$air_pid" 2>/dev/null || { wait "$air_pid"; exit 1; }
    # Probe local management setup so configuration waits for both Mirrord and components.
    if curl --fail --silent --max-time 2 http://127.0.0.1:27833/api/healthz >/dev/null; then break; fi
    if (( elapsed % 10 == 0 )); then echo "Waiting for local management setup (${elapsed}s)..."; fi
    sleep 1
  done
  curl --fail --silent --show-error --max-time 3 http://127.0.0.1:27833/api/healthz >/dev/null
  set -m
  make --no-print-directory _dev-configure &
  task_pid=$!
  set +m
  wait "$task_pid"
  task_pid=''
  touch "$session_lock/ready"
  echo "Development ready: $DEV_API_URL (profile $DEV_PROFILE, organization $DEV_ORGANIZATION, space $DEV_SPACE)."
  echo 'Use make logs for Borq logs, make k9s for the cluster, and make dev-stop to stop hot reload.'
  wait "$air_pid"
}

wait_ready() {
  for ((elapsed=0; elapsed<900; elapsed++)); do
    if ! running; then
      if [ -f "$session_log" ]; then tail -n 50 "$session_log" >&2; fi
      echo 'Development session is not running; run make dev to start it.' >&2
      return 1
    fi
    if [ -f "$session_lock/ready" ]; then
      echo "Development ready: $DEV_API_URL (profile $DEV_PROFILE, organization $DEV_ORGANIZATION, space $DEV_SPACE)."
      return
    fi
    if (( elapsed % 10 == 0 )); then
      echo "Waiting for development setup (${elapsed}s); output: $session_log"
    fi
    sleep 1
  done
  tail -n 50 "$session_log" >&2
  echo 'Development startup timed out. Run make logs to inspect cluster readiness.' >&2
  return 1
}

stop() {
  if ! running; then echo 'No local development session is running.'; return; fi
  kill -TERM "$saved_pid"
  for ((elapsed=0; elapsed<30; elapsed++)); do
    if ! running; then echo 'Hot reload stopped. Cluster and development data retained.'; return; fi
    sleep 1
  done
  echo "Development session did not stop; inspect $session_log." >&2
  exit 1
}

kill_cluster() {
  local runtime_dir k3d cleanup_dir
  if [ -d "$DEV_DIR" ]; then
    runtime_dir=$(cd "$DEV_DIR" && pwd -P)
    case "$(pwd -P)/" in
      "${runtime_dir%/}/"*) echo "Refusing to remove the repository or a parent directory: $runtime_dir" >&2; return 1 ;;
    esac
    if [ ! -f "$session_log" ]; then
      echo "Refusing to remove a directory without a Borq development session: $DEV_DIR" >&2
      return 1
    fi
  fi
  if command -v k3d >/dev/null 2>&1; then k3d=k3d
  elif [ -x tmp/tools/k3d ]; then k3d=tmp/tools/k3d
  else k3d="${XDG_CONFIG_HOME:-$HOME/.config}/borq/bin/k3d"; fi
  require_tool "$k3d"
  stop
  echo "Removing development cluster $DEV_CLUSTER..."
  # k3d deletes kubeconfig entries too; keep it away from the user's kubeconfig.
  cleanup_dir=$(mktemp -d)
  printf 'apiVersion: v1\nkind: Config\n' > "$cleanup_dir/kubeconfig"
  if ! KUBECONFIG="$cleanup_dir/kubeconfig" "$k3d" cluster delete "$DEV_CLUSTER"; then
    rm -rf -- "$cleanup_dir"
    return 1
  fi
  rm -rf -- "$cleanup_dir"
  echo "Removing local development data: $DEV_DIR"
  rm -rf -- "$DEV_DIR"
  echo 'Development cluster and local data removed. The next make dev starts fresh.'
}

logs() {
  require_tool stern
  [ -f "$session_log" ] || { echo 'Development log is missing; run make dev first.' >&2; exit 1; }
  tail -n 100 -F "$session_log" &
  local_logs=$!
  (
    while [ ! -f "$DEV_KUBECONFIG" ]; do
      if ! running; then echo 'Development stopped before its kubeconfig was created.' >&2; exit 1; fi
      sleep 1
    done
    exec stern --kubeconfig "$DEV_KUBECONFIG" --namespace borq-mgmt \
      --selector app.kubernetes.io/name=borq --container '^borq$' --tail 20
  ) &
  cluster_logs=$!
  trap 'kill "$local_logs" "$cluster_logs" 2>/dev/null || true; wait 2>/dev/null || true' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  wait "$cluster_logs"
}

case "${1:-}" in
  start) start ;;
  run) run ;;
  wait) wait_ready ;;
  stop) stop ;;
  kill) kill_cluster ;;
  logs) logs ;;
  *) echo 'Expected start, run, wait, stop, kill, or logs.' >&2; exit 1 ;;
esac
