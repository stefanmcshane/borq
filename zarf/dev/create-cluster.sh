#!/usr/bin/env bash
set -euo pipefail
umask 077

# Development scaffolding owns disposable Kubernetes; Borq only bootstraps components.
borq_k3d=$1
borq_cluster=$2
borq_kubeconfig=$3
borq_http_port=$4
borq_https_port=$5
borq_api_port=$6
existing=$("$borq_k3d" cluster list --no-headers | awk -v name="$borq_cluster" '$1 == name { print $1 }')
if [ -n "$existing" ]; then
  "$borq_k3d" cluster start "$borq_cluster"
  bindings=$(docker port "k3d-$borq_cluster-serverlb" 31833/tcp 2>/dev/null || true)
  case $'\n'"$bindings"$'\n' in
    *$'\n'"127.0.0.1:$borq_api_port"$'\n'*) ;;
    *) "$borq_k3d" node edit "k3d-$borq_cluster-serverlb" --port-add "127.0.0.1:$borq_api_port:31833" ;;
  esac
else
  # k3d exports a literal zero port; select a free loopback port first.
  while :; do
    borq_kubernetes_port=$((20000 + RANDOM))
    if ! (true <>"/dev/tcp/127.0.0.1/$borq_kubernetes_port") 2>/dev/null; then break; fi
  done
  arguments=(cluster create "$borq_cluster" --image rancher/k3s:v1.35.3-k3s1
    --api-port "127.0.0.1:$borq_kubernetes_port"
    --kubeconfig-update-default=false --kubeconfig-switch-context=false
    --k3s-arg '--disable=traefik@server:*'
    --k3s-arg '--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*'
    --k3s-arg '--kube-proxy-arg=conntrack-max-per-core=0@server:*'
    --port "127.0.0.1:$borq_http_port:80@loadbalancer"
    --port "127.0.0.1:$borq_https_port:443@loadbalancer"
    --port "127.0.0.1:$borq_api_port:31833@loadbalancer" --wait --timeout 5m)
  if [ "$(uname -s)" = Linux ]; then arguments+=(--volume '/dev/null:/dev/kmsg@server:*'); fi
  "$borq_k3d" "${arguments[@]}"
fi
mkdir -p "$(dirname "$borq_kubeconfig")"
"$borq_k3d" kubeconfig get "$borq_cluster" > "$borq_kubeconfig"
chmod 600 "$borq_kubeconfig"
