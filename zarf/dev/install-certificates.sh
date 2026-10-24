#!/usr/bin/env bash
set -euo pipefail

# Keep the real issuance controller and webhook in the management namespace.
# Production issuers and DNS credentials belong to future infrastructure setup.
borq_kubeconfig=${1:?a kubeconfig path is required}
borq_manifest_dir=$(mktemp -d)
trap 'rm -rf "$borq_manifest_dir"' EXIT
borq_manifest="$borq_manifest_dir/cert-manager.yaml"
curl --fail --silent --show-error --location --retry 3 \
  https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml \
  --output "$borq_manifest"
printf '%s  %s\n' 1ce11cae912adecc69e6bb623435fafc9ed21505f9efff98bd71d7b80f01db1f "$borq_manifest" | sha256sum --check

# The pinned upstream manifest uses cert-manager for components and kube-system
# for election leases. Update every namespace reference, including webhook trust.
sed \
  -e '1,20s/^  name: cert-manager$/  name: borq-system/' \
  -e 's/namespace: cert-manager$/namespace: borq-system/' \
  -e 's/namespace: kube-system$/namespace: borq-system/' \
  -e 's/--leader-election-namespace=kube-system/--leader-election-namespace=borq-system/' \
  -e 's|"cert-manager/cert-manager-webhook-ca"|"borq-system/cert-manager-webhook-ca"|' \
  "$borq_manifest" > "$borq_manifest_dir/borq-cert-manager.yaml"
kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --field-manager=borq-local-certificates \
  --filename "$borq_manifest_dir/borq-cert-manager.yaml"
kubectl --kubeconfig "$borq_kubeconfig" wait --for=condition=Established \
  crd/certificates.cert-manager.io crd/clusterissuers.cert-manager.io --timeout=60s
kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system rollout status \
  deployment/cert-manager --timeout=180s
kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system rollout status \
  deployment/cert-manager-cainjector --timeout=180s
kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system rollout status \
  deployment/cert-manager-webhook --timeout=180s

# Deployment readiness can precede CA injection into admission configuration.
cat > "$borq_manifest_dir/probe-issuer.yaml" <<'YAML'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: borq-local-admission-probe
spec:
  selfSigned: {}
YAML
for borq_attempt in {1..30}; do
  if kubectl --kubeconfig "$borq_kubeconfig" create --dry-run=server \
    --filename "$borq_manifest_dir/probe-issuer.yaml" > "$borq_manifest_dir/probe.log" 2>&1; then
    printf 'cert-manager is ready for local HTTPS tests\n'
    exit 0
  fi
  sleep 2
done
cat "$borq_manifest_dir/probe.log" >&2
exit 1
