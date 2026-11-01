#!/usr/bin/env bash
# Prepare the current kube context for the perf matrix:
#   - install kwok and create simulated nodes for the load pods,
#   - replace metrics-server with fake-metrics-server,
#   - deploy the controller with config/perf.
# The cluster should be disposable (for example `make kind-up`).
#
#   IMG=localhost/dryrun-hpa:dev test/perf/setup.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
IMG=${IMG:-localhost/dryrun-hpa:dev}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
KWOK_NODES=${KWOK_NODES:-10}
KWOK_PODS_PER_NODE=${KWOK_PODS_PER_NODE:-2000}

echo "Context: $(kubectl config current-context)"

echo "Installing kwok ${KWOK_VERSION}"
kwok_url="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"
kubectl apply -f "${kwok_url}/kwok.yaml"
kubectl apply -f "${kwok_url}/stage-fast.yaml"
kubectl -n kube-system rollout status deployment kwok-controller --timeout=3m

echo "Creating ${KWOK_NODES} kwok nodes"
for i in $(seq 0 $((KWOK_NODES - 1))); do
  cat <<EOF
---
apiVersion: v1
kind: Node
metadata:
  name: kwok-node-${i}
  annotations:
    kwok.x-k8s.io/node: fake
    node.alpha.kubernetes.io/ttl: "0"
  labels:
    type: kwok
    kubernetes.io/hostname: kwok-node-${i}
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    kubernetes.io/role: agent
spec:
  taints:
    - key: kwok.x-k8s.io/node
      value: fake
      effect: NoSchedule
status:
  allocatable:
    cpu: "1000"
    memory: 4Ti
    pods: "${KWOK_PODS_PER_NODE}"
  capacity:
    cpu: "1000"
    memory: 4Ti
    pods: "${KWOK_PODS_PER_NODE}"
EOF
done | kubectl apply -f -

echo "Replacing metrics-server with fake-metrics-server"
kubectl -n kube-system delete deployment metrics-server --ignore-not-found
kubectl kustomize "${ROOT}/test/perf/fake-metrics-server" |
  sed "s|image: localhost/dryrun-hpa:dev|image: ${IMG}|" |
  kubectl apply -f -
kubectl -n kube-system rollout status deployment fake-metrics-server --timeout=3m
for _ in $(seq 60); do
  if kubectl get --raw /apis/metrics.k8s.io/v1beta1/pods >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl get --raw /apis/metrics.k8s.io/v1beta1/pods >/dev/null

echo "Deploying the controller (${IMG})"
"${ROOT}/hack/deploy.sh" "${ROOT}/config/perf" "${IMG}"
