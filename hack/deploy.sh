#!/usr/bin/env bash
# Deploy a kustomize directory with the controller image set to IMG.
#   hack/deploy.sh config/default localhost/dryrun-hpa:dev
set -euo pipefail

DIR=$(realpath "${1:?usage: deploy.sh KUSTOMIZE_DIR IMAGE}")
IMG=${2:?usage: deploy.sh KUSTOMIZE_DIR IMAGE}
name=${IMG%:*}
tag=${IMG##*:}
[[ "${name}" == "${IMG}" ]] && tag=latest

overlay=$(mktemp -d)
trap 'rm -rf "${overlay}"' EXIT
cat >"${overlay}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $(realpath --relative-to="${overlay}" "${DIR}")
images:
  - name: localhost/dryrun-hpa
    newName: ${name}
    newTag: ${tag}
EOF
# Apply CRDs first so the rest of the objects can be validated against them.
kubectl apply -k "${DIR}/../crd" 2>/dev/null || true
kubectl apply -k "${overlay}"
kubectl -n dryrun-hpa-system rollout status deployment dryrun-hpa-controller --timeout=3m
