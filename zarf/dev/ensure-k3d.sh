#!/usr/bin/env bash
set -euo pipefail
# Keep the optional development tool inside the repository rather than changing PATH.
borq_k3d=${1:-tmp/tools/k3d}
if command -v "$borq_k3d" >/dev/null 2>&1 || [ -x "$borq_k3d" ]; then
  exit 0
fi
mkdir -p "$(dirname "$borq_k3d")"
borq_install_script=$(mktemp)
trap 'rm -f "$borq_install_script"' EXIT
curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/k3d-io/k3d/v5.8.3/install.sh --output "$borq_install_script"
TAG=v5.8.3 USE_SUDO=false K3D_INSTALL_DIR="$(cd "$(dirname "$borq_k3d")" && pwd)" bash "$borq_install_script"
