#!/usr/bin/env bash
set -euo pipefail

# These are local development dependencies, never part of the Borq apply path.
# OpenBao dev mode keeps its data in memory and resets it when the pod restarts.
borq_kubeconfig=${1:?a kubeconfig path is required}
borq_manifest_dir=$(mktemp -d)
trap 'rm -rf "$borq_manifest_dir"' EXIT
chmod 700 "$borq_manifest_dir"

kubectl --kubeconfig "$borq_kubeconfig" create namespace borq-system --dry-run=client --output yaml |
  kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --field-manager=borq-local-secrets --filename -

borq_operator_manifest="$borq_manifest_dir/external-secrets.yaml"
curl --fail --silent --show-error --location --retry 3 \
  https://github.com/external-secrets/external-secrets/releases/download/v2.4.0/external-secrets.yaml \
  --output "$borq_operator_manifest"
printf '%s  %s\n' e09d3ad3c9050ebd622bf619a68edaec5febea08998cec3f83fddc6b3aa74fa9 "$borq_operator_manifest" | sha256sum --check
sed \
  -e 's/namespace: default$/namespace: borq-system/' \
  -e 's/--service-namespace=default/--service-namespace=borq-system/' \
  -e 's/--secret-namespace=default/--secret-namespace=borq-system/' \
  -e 's/external-secrets-webhook.default.svc/external-secrets-webhook.borq-system.svc/' \
  "$borq_operator_manifest" > "$borq_manifest_dir/borq-external-secrets.yaml"
kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --field-manager=borq-local-secrets \
  --filename "$borq_manifest_dir/borq-external-secrets.yaml"
kubectl --kubeconfig "$borq_kubeconfig" wait --for=condition=Established \
  crd/externalsecrets.external-secrets.io crd/clustersecretstores.external-secrets.io --timeout=60s

# Keep the local root token in a Kubernetes Secret, out of files and command output.
if ! kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system get secret borq-openbao >/dev/null 2>&1; then
  umask 077
  openssl rand -hex 32 | tr -d '\n' > "$borq_manifest_dir/token"
  kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system create secret generic borq-openbao \
    --from-file="token=$borq_manifest_dir/token"
  rm "$borq_manifest_dir/token"
fi

cat > "$borq_manifest_dir/openbao.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openbao
  namespace: borq-system
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: openbao
  template:
    metadata:
      labels:
        app.kubernetes.io/name: openbao
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
      containers:
        - name: openbao
          image: quay.io/openbao/openbao:2.7.1
          command: [bao]
          args: [server, -dev, -dev-no-store-token, -dev-listen-address=0.0.0.0:8200]
          env:
            - name: BAO_ADDR
              value: http://127.0.0.1:8200
            - name: BAO_DEV_ROOT_TOKEN_ID
              valueFrom:
                secretKeyRef:
                  name: borq-openbao
                  key: token
          ports:
            - name: http
              containerPort: 8200
          readinessProbe:
            httpGet:
              path: /v1/sys/health
              port: http
            periodSeconds: 2
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              memory: 512Mi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
---
apiVersion: v1
kind: Service
metadata:
  name: openbao
  namespace: borq-system
spec:
  selector:
    app.kubernetes.io/name: openbao
  ports:
    - name: http
      port: 8200
      targetPort: http
YAML
kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --field-manager=borq-local-secrets \
  --filename "$borq_manifest_dir/openbao.yaml"
kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system rollout status deployment/openbao --timeout=180s
kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system exec deployment/openbao -- \
  sh -c 'BAO_TOKEN="$BAO_DEV_ROOT_TOKEN_ID" bao write secret/config max_versions=11 >/dev/null'

for borq_deployment in external-secrets external-secrets-cert-controller external-secrets-webhook; do
  kubectl --kubeconfig "$borq_kubeconfig" --namespace borq-system rollout status \
    "deployment/$borq_deployment" --timeout=180s
done

cat > "$borq_manifest_dir/store.yaml" <<'YAML'
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: borq-openbao
spec:
  provider:
    vault:
      server: http://openbao.borq-system.svc:8200
      path: secret
      version: v2
      auth:
        tokenSecretRef:
          name: borq-openbao
          key: token
          namespace: borq-system
YAML
for borq_attempt in {1..30}; do
  if kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --dry-run=server --field-manager=borq-local-secrets \
    --filename "$borq_manifest_dir/store.yaml" > "$borq_manifest_dir/probe.log" 2>&1; then
    kubectl --kubeconfig "$borq_kubeconfig" apply --server-side --field-manager=borq-local-secrets \
      --filename "$borq_manifest_dir/store.yaml"
    kubectl --kubeconfig "$borq_kubeconfig" wait --for=condition=Ready clustersecretstore/borq-openbao --timeout=120s
    printf 'OpenBao and External Secrets are ready for local deployment tests\n'
    exit 0
  fi
  sleep 2
done
cat "$borq_manifest_dir/probe.log" >&2
exit 1
