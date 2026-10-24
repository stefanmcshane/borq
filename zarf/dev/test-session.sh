#!/usr/bin/env bash
set -euo pipefail

# Exercise process ownership and persistence against real Air, Mirrord, and Kubernetes.
cd "$(dirname "$0")/../.."
mkdir -p tmp
test_root=$(mktemp -d "$PWD/tmp/dev-test.XXXXXX")
export DEV_DIR="$test_root/runtime" DEV_CLUSTER="borq-dev-test-$$" DEV_PROFILE=development
export DEV_ORGANIZATION=dev DEV_SPACE=default DEV_DETACH=1 XDG_CONFIG_HOME="$test_root/config"
export DEV_HTTP_PORT=18080 DEV_HTTPS_PORT=19443
export DEV_API_PORT=17833 DEV_API_URL=http://localhost:17833
logs_pid=''
cleanup() {
  if [ -n "$logs_pid" ]; then kill -TERM -- "-$logs_pid" 2>/dev/null || true; fi
  make --no-print-directory dev-stop || true
  if [ "${DEV_TEST_KEEP:-0}" = 1 ]; then
    echo "Development test files and cluster retained: $test_root ($DEV_CLUSTER)"
    return
  fi
  if command -v k3d >/dev/null 2>&1; then k3d=k3d
  elif [ -x tmp/tools/k3d ]; then k3d=tmp/tools/k3d
  else k3d="$XDG_CONFIG_HOME/borq/bin/k3d"; fi
  if [ -x "$k3d" ] || command -v "$k3d" >/dev/null 2>&1; then
    printf 'apiVersion: v1\nkind: Config\n' > "$test_root/cleanup-kubeconfig"
    KUBECONFIG="$test_root/cleanup-kubeconfig" "$k3d" cluster delete "$DEV_CLUSTER"
  fi
  rm -rf "$test_root"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

started=$SECONDS
set -m
make --no-print-directory dev &
launcher=$!
set +m
wait "$launcher"
(( SECONDS - started < 10 )) || { echo 'Detached startup waited for bootstrap.' >&2; exit 1; }
read -r supervisor < "$DEV_DIR/session.pid"
# Terminal and task runners may clean up the launcher's process group on exit.
kill -TERM -- "-$launcher" 2>/dev/null || true
sleep 1
kill -0 "$supervisor"
make --no-print-directory dev-wait
make --no-print-directory dev
[ "$(cat "$DEV_DIR/session.pid")" = "$supervisor" ]
cp zarf/dev/testdata/apps.yaml "$test_root/apps.yaml"
"$DEV_DIR/borq" apply -f "$test_root/apps.yaml" --wait
namespace=$(kubectl --kubeconfig "$DEV_DIR/kubeconfig" get deployments -A --field-selector metadata.name=workflow -o 'jsonpath={.items[0].metadata.namespace}')
deployment_uid=$(kubectl --kubeconfig "$DEV_DIR/kubeconfig" -n "$namespace" get deployment workflow -o 'jsonpath={.metadata.uid}')

set -m
make --no-print-directory logs > "$test_root/logs" 2>&1 &
logs_pid=$!
set +m
sleep 5
kill -0 "$logs_pid"
kill -TERM -- "-$logs_pid"
wait "$logs_pid" || true
logs_pid=''
kill -0 "$supervisor"
grep -q 'watching api' "$test_root/logs"
grep -Eq '^(\+ )?borq-[^[:space:]]+[[:space:]]+(›[[:space:]]+)?borq([[:space:]]|$)' "$test_root/logs"
if grep -Eq '^(\+ )?alloy-[^[:space:]]+[[:space:]]' "$test_root/logs"; then
  echo 'Development logs included Alloy pod output.' >&2
  exit 1
fi

server_pid() {
  ps -A -o pid= -o args= | awk '/tmp\/air\/borq server --port=27833/ && !found { print $1; found=1 }'
}
previous_server=$(server_pid)
[ -n "$previous_server" ]
# Generate a real watcher event without changing the chart's contents or adding Go code.
chart=business/server/apps/charts/app/templates/services.yaml
cp "$chart" "$test_root/chart.yaml"
cat "$test_root/chart.yaml" > "$chart"
reloaded=false
for ((elapsed=0; elapsed<180; elapsed++)); do
  current_server=$(server_pid)
  if [ -n "$current_server" ] && [ "$current_server" != "$previous_server" ] &&
    curl --fail --silent --max-time 2 "$DEV_API_URL/api/v1/organizations/dev/spaces/default/apps" | grep -q '"name":"workflow"'; then
    reloaded=true
    break
  fi
  sleep 1
done
[ "$reloaded" = true ] || { tail -n 60 "$DEV_DIR/session.log" >&2; echo 'Air reload did not preserve app state.' >&2; exit 1; }
make --no-print-directory dev-stop
! kill -0 "$supervisor" 2>/dev/null
[ -s "$DEV_DIR/data/borq.sqlite" ]
kubectl --kubeconfig "$DEV_DIR/kubeconfig" -n "$namespace" rollout status deployment/workflow --timeout=30s
# A second cluster must reject the occupied API port before building or provisioning.
if DEV_CLUSTER="$DEV_CLUSTER-other" DEV_DIR="$test_root/blocked" make --no-print-directory dev > "$test_root/blocked.log" 2>&1; then
  echo 'Development startup accepted a port owned by another cluster.' >&2
  exit 1
fi
grep -q "Local development port $DEV_API_PORT is already in use" "$test_root/blocked.log"
[ ! -d "$test_root/blocked/image" ]
# Stopping immediately after a detached restart must stop bootstrap too.
make --no-print-directory dev
read -r starting_supervisor < "$DEV_DIR/session.pid"
make --no-print-directory dev-stop
! kill -0 "$starting_supervisor" 2>/dev/null
make --no-print-directory dev
make --no-print-directory dev-wait
"$DEV_DIR/borq" apps list | grep -q 'name: workflow'
[ "$(kubectl --kubeconfig "$DEV_DIR/kubeconfig" -n "$namespace" get deployment workflow -o 'jsonpath={.metadata.uid}')" = "$deployment_uid" ]
# Killing development removes both Kubernetes state and the local database.
# A user-managed kubeconfig must survive even when it contains this cluster.
cp "$DEV_DIR/kubeconfig" "$test_root/user-kubeconfig"
cp "$test_root/user-kubeconfig" "$test_root/user-kubeconfig.before"
export KUBECONFIG="$test_root/user-kubeconfig"
read -r supervisor < "$DEV_DIR/session.pid"
mkdir "$test_root/unrelated"
printf 'keep\n' > "$test_root/unrelated/sentinel"
if make --no-print-directory dev-kill DEV_DIR="$test_root/unrelated" > "$test_root/guard.log" 2>&1; then
  echo 'Development kill accepted an unrelated directory.' >&2
  exit 1
fi
grep -q 'Refusing to remove a directory without a Borq development session' "$test_root/guard.log"
[ -s "$test_root/unrelated/sentinel" ]
kill -0 "$supervisor"
make --no-print-directory dev-kill
! kill -0 "$supervisor" 2>/dev/null
[ ! -e "$DEV_DIR" ]
[ -z "$(docker ps -aq --filter "label=k3d.cluster=$DEV_CLUSTER")" ]
[ -z "$(docker volume ls -q --filter "name=k3d-$DEV_CLUSTER-")" ]
[ -z "$(docker network ls -q --filter "name=^k3d-$DEV_CLUSTER$")" ]
cmp "$test_root/user-kubeconfig.before" "$test_root/user-kubeconfig"
# Repeating cleanup is harmless, and the next start must not restore the old app.
make --no-print-directory dev-kill
make --no-print-directory dev
make --no-print-directory dev-wait
"$DEV_DIR/borq" apps list > "$test_root/fresh-apps.yaml"
if grep -q 'name: workflow' "$test_root/fresh-apps.yaml"; then
  echo 'Development kill retained the previous application database.' >&2
  exit 1
fi
kubectl --kubeconfig "$DEV_DIR/kubeconfig" get deployments --all-namespaces \
  -o 'jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' > "$test_root/fresh-deployments"
if grep -Fxq "$namespace/workflow" "$test_root/fresh-deployments"; then
  echo 'Development kill retained the previous Kubernetes application.' >&2
  exit 1
fi
if [ "${DEV_TEST_KEEP:-0}" != 1 ]; then make --no-print-directory dev-kill; fi
cmp "$test_root/user-kubeconfig.before" "$test_root/user-kubeconfig"
cleanup
trap - EXIT
echo 'Development lifecycle passed: immediate detach, repeat start, Borq logs, hot reload, port conflicts, startup cancellation, restart persistence, full cleanup, and fresh setup.'
