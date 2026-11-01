#!/usr/bin/env bash
# Generates deepcopy functions, CRDs, RBAC and the typed clientset, listers
# and informers for the APIs in api/.

set -o errexit
set -o nounset
set -o pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${ROOT}"

MODULE="$(go list -m)"
BIN="${ROOT}/bin"
CONTROLLER_GEN="${CONTROLLER_GEN:-${BIN}/controller-gen}"
# shellcheck source=/dev/null
source "${ROOT}/hack/upstream.lock"
CODEGEN_VERSION="${CODEGEN_VERSION:-${KUBERNETES_VERSION/v1./v0.}}"
BOILERPLATE="${ROOT}/hack/boilerplate.go.txt"

CODEGEN_PKG="$(go mod download -json "k8s.io/code-generator@${CODEGEN_VERSION}" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')"
# shellcheck source=/dev/null
source "${CODEGEN_PKG}/kube_codegen.sh"

"${CONTROLLER_GEN}" object:headerFile="${BOILERPLATE}" paths="./api/..."

"${CONTROLLER_GEN}" \
  crd:crdVersions=v1 \
  rbac:roleName=dryrun-hpa-controller \
  paths="./api/..." paths="./internal/..." paths="./cmd/dryrun-hpa-controller/..." \
  output:crd:artifacts:config=config/crd/bases \
  output:rbac:artifacts:config=config/rbac

rm -rf "${ROOT}/pkg/generated"
kube::codegen::gen_client \
  --with-watch \
  --output-dir "${ROOT}/pkg/generated" \
  --output-pkg "${MODULE}/pkg/generated" \
  --boilerplate "${BOILERPLATE}" \
  "${ROOT}/api"
