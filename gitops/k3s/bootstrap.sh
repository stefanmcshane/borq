#!/usr/bin/env bash

set -euo pipefail

readonly DEFAULT_K3S_VERSION="v1.36.4+k3s1"
readonly DEFAULT_FLUX_VERSION="2.9.3"
readonly DEFAULT_GITHUB_BRANCH="main"
readonly FLUX_PATH="./gitops/k3s"
readonly FLUX_COMPONENTS="source-controller,kustomize-controller,helm-controller"

log() {
  printf '\n==> %s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage:
  sudo ./bootstrap.sh <github-owner/repository> [branch]

The script prompts for a GitHub token and Doppler service token. For unattended
use, provide them as GITHUB_TOKEN and DOPPLER_TOKEN environment variables.

Optional version overrides:
  K3S_VERSION   Defaults to v1.36.4+k3s1
  FLUX_VERSION  Defaults to 2.9.3

The GitHub token needs read access to the repository. The repository must
already contain gitops/k3s on the selected branch.
EOF
}

read_secret() {
  local variable_name="$1"
  local prompt="$2"
  local value="${!variable_name:-}"

  if [[ -z "$value" ]]; then
    [[ -r /dev/tty ]] || fail "$variable_name is required for unattended use"
    read -r -s -p "$prompt" value </dev/tty
    printf '\n' >/dev/tty
  fi

  [[ -n "$value" ]] || fail "$variable_name cannot be empty"
  printf -v "$variable_name" '%s' "$value"
}

[[ "${1:-}" != "--help" && "${1:-}" != "-h" ]] || {
  usage
  exit 0
}

[[ $# -ge 1 && $# -le 2 ]] || {
  usage >&2
  exit 2
}

[[ $EUID -eq 0 ]] || fail "run this script as root (for example, with sudo)"
[[ -r /etc/os-release ]] || fail "cannot identify the operating system"

# shellcheck source=/dev/null
source /etc/os-release
case "${ID:-}" in
  debian | ubuntu) ;;
  *) fail "this script supports Debian and Ubuntu; found ${ID:-unknown}" ;;
esac

command -v systemctl >/dev/null || fail "systemd is required"
command -v apt-get >/dev/null || fail "apt-get is required"

github_repository="${1%.git}"
github_repository="${github_repository#https://github.com/}"
github_repository="${github_repository#git@github.com:}"
github_branch="${2:-$DEFAULT_GITHUB_BRANCH}"

[[ "$github_repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || \
  fail "repository must be in github-owner/repository form"
[[ "$github_branch" =~ ^[A-Za-z0-9._/-]+$ ]] || fail "invalid Git branch"

readonly github_repository github_branch
readonly github_url="https://github.com/${github_repository}.git"
readonly k3s_version="${K3S_VERSION:-$DEFAULT_K3S_VERSION}"
readonly flux_version="${FLUX_VERSION:-$DEFAULT_FLUX_VERSION}"

read_secret GITHUB_TOKEN "GitHub token: "
read_secret DOPPLER_TOKEN "Doppler service token: "
export -n GITHUB_TOKEN DOPPLER_TOKEN

temporary_directory="$(mktemp -d)"
cleanup() {
  rm -rf -- "$temporary_directory"
  unset GITHUB_TOKEN DOPPLER_TOKEN
}
trap cleanup EXIT

log "Installing host prerequisites"
apt-get update
apt-get install -y ca-certificates curl

log "Installing K3s ${k3s_version}"
curl -fsSL https://get.k3s.io -o "$temporary_directory/install-k3s.sh"
env \
  INSTALL_K3S_VERSION="$k3s_version" \
  INSTALL_K3S_EXEC="server --disable=servicelb" \
  sh "$temporary_directory/install-k3s.sh"

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
systemctl is-active --quiet k3s || fail "K3s did not start"
k3s kubectl wait --for=condition=Ready node --all --timeout=5m

log "Installing Flux CLI ${flux_version}"
curl -fsSL https://fluxcd.io/install.sh -o "$temporary_directory/install-flux.sh"
env FLUX_VERSION="$flux_version" bash "$temporary_directory/install-flux.sh"

flux check --pre --components="$FLUX_COMPONENTS"

log "Installing Flux controllers"
flux install --components="$FLUX_COMPONENTS"
flux check --components="$FLUX_COMPONENTS"

log "Creating bootstrap credentials"
printf '%s' "$GITHUB_TOKEN" >"$temporary_directory/github-token"
printf '%s' "$DOPPLER_TOKEN" >"$temporary_directory/doppler-token"
chmod 600 "$temporary_directory/github-token" "$temporary_directory/doppler-token"

kubectl -n flux-system create secret generic flux-system \
  --from-literal=username=x-access-token \
  --from-file=password="$temporary_directory/github-token" \
  --dry-run=client \
  -o yaml | kubectl apply -f -

kubectl create namespace external-secrets \
  --dry-run=client \
  -o yaml | kubectl apply -f -

kubectl -n external-secrets create secret generic doppler-token \
  --from-file=dopplerToken="$temporary_directory/doppler-token" \
  --dry-run=client \
  -o yaml | kubectl apply -f -

unset GITHUB_TOKEN DOPPLER_TOKEN

log "Connecting Flux to ${github_repository}@${github_branch}"
flux create source git flux-system \
  --url="$github_url" \
  --branch="$github_branch" \
  --secret-ref=flux-system \
  --interval=1m

flux create kustomization flux-system \
  --source=GitRepository/flux-system \
  --path="$FLUX_PATH" \
  --prune=true \
  --wait=true \
  --health-check-timeout=30m \
  --interval=10m

log "Reconciling the platform"
flux reconcile kustomization flux-system --with-source --timeout=30m

log "Bootstrap complete"
flux get kustomizations
kubectl get nodes
