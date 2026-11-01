#!/usr/bin/env bash
# Install metrics-server in the current kube context, configured for kind.
set -euo pipefail

METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION:-latest}

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
