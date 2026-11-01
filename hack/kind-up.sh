#!/usr/bin/env bash
# Create a kind cluster with metrics-server. Uses the podman provider when
# podman is available.
set -euo pipefail

KIND_CLUSTER=${KIND_CLUSTER:-dryrun-hpa}
METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION:-latest}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-}

if [[ -z "${KIND_EXPERIMENTAL_PROVIDER:-}" ]] && command -v podman >/dev/null 2>&1; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  args=(create cluster --name "${KIND_CLUSTER}" --wait 2m)
  [[ -n "${KIND_NODE_IMAGE}" ]] && args+=(--image "${KIND_NODE_IMAGE}")
  kind "${args[@]}"
fi
kubectl config use-context "kind-${KIND_CLUSTER}"

if [[ "${METRICS_SERVER_VERSION}" == latest ]]; then
  url=https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
else
  url=https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION}/components.yaml
fi
kubectl apply -f "${url}"
# kind kubelets serve self-signed certificates; scrape every 15s like the defaults of most clusters.
kubectl -n kube-system patch deployment metrics-server --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--metric-resolution=15s"}
]' 2>/dev/null || true
kubectl -n kube-system rollout status deployment metrics-server --timeout=3m
echo "Cluster kind-${KIND_CLUSTER} is ready."
