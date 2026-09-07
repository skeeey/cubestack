#!/usr/bin/env bash
# Installs the upstream LeaderWorkerSet controller AND its
# leaderworkerset/disaggregatedset CRDs into the helm-e2e kind cluster from the
# pinned lws module's config/default (the chart ships only the ai.cubestack.io
# CRDs). Without the controller and its CRDs LeaderWorkerSets never materialize
# pods, so ISVC Ready is unreachable.
#
# The controller image reference in the manifests points at the upstream
# staging registry, which is unreachable from this network. Image fallback
# chain: (1) docker pull of the manifest image, (2) the same image via the
# docker.1ms.run mirror (retagged to the original ref), (3) a local build from
# the pinned lws go module (golang:1.26 + distroless bases are cached; the
# build uses the goproxy.cn mirror because proxy.golang.org is unreachable).
# The picked image is then loaded into kind so the kubelet runs it locally.
#
# When CI is set (GitHub Actions sets CI=true) steps 1-2 are skipped: the
# pinned module is always built so e2e runs are deterministic.
#
# Idempotent: exits early when lws-controller-manager is already Available.
set -euo pipefail

NS="lws-system"
DEPLOY="lws-controller-manager"

KIND_CLUSTER="${KIND_CLUSTER_HELM:-cubestack-helm-e2e}"
CTX="kind-${KIND_CLUSTER}"
KUBECTL="${KUBECTL:-kubectl}"
DOCKER="${DOCKER:-docker}"
KIND_BIN="${KIND:-kind}"

cd "$(dirname "$0")/.." # operator/

if [ "$("${KUBECTL}" --context "${CTX}" get deployment -n "${NS}" "${DEPLOY}" \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)" = "True" ]; then
  echo "lws-controller-manager already Available in ${NS} on ${CTX} — skipping"
  exit 0
fi

# --- locate the pinned lws go module (version source of truth: go.mod) ---
LWS_VER="$(awk '$1=="sigs.k8s.io/lws" {print $2}' go.mod)"
[ -n "${LWS_VER}" ] || { echo "sigs.k8s.io/lws not found in go.mod" >&2; exit 1; }
go mod download sigs.k8s.io/lws
LWS_MOD="$(go env GOMODCACHE)/sigs.k8s.io/lws@${LWS_VER}"
[ -f "${LWS_MOD}/config/default/kustomization.yaml" ] || { echo "lws module config missing: ${LWS_MOD}" >&2; exit 1; }

KUSTOMIZE="${KUSTOMIZE:-$(pwd)/bin/kustomize}"
if [ ! -x "${KUSTOMIZE}" ]; then
  make -s kustomize
fi

# --- image ref baked into the module manifests (config/manager images) ---
MANIFEST_REF="$("${KUSTOMIZE}" build "${LWS_MOD}/config/manager" 2>/dev/null | awk '/^[[:space:]]*image: /{print $2; exit}')"
MANIFEST_REF="${MANIFEST_REF:-us-central1-docker.pkg.dev/k8s-staging-images/lws/lws:main}"

# NOTE: the pinned v0.10.0 module's own config/manager kustomization sets
# newTag: main, so a successful direct/mirror pull runs a mutable `main`
# controller against v0.10.0 CRDs (nondeterministic, and staging GCs
# non-release tags so the pull can silently start failing later). The local
# build of the pinned module is the deterministic path: it builds v0.10.0
# source. Under CI (any non-empty CI value; GitHub Actions sets CI=true) the
# pulls are skipped entirely so e2e always runs the pinned v0.10.0 build;
# local devs on open networks keep the pull fast path.
REF=""
if [ -z "${CI:-}" ] && "${DOCKER}" pull "${MANIFEST_REF}" >/dev/null 2>&1; then
  REF="${MANIFEST_REF}"
  echo "pulled upstream manifest image ${REF}"
elif [ -z "${CI:-}" ] && "${DOCKER}" pull "docker.1ms.run/${MANIFEST_REF}" >/dev/null 2>&1; then
  "${DOCKER}" tag "docker.1ms.run/${MANIFEST_REF}" "${MANIFEST_REF}"
  REF="${MANIFEST_REF}"
  echo "pulled docker.1ms.run/${MANIFEST_REF} via mirror, retagged to ${REF}"
else
  REF="example.com/lws/lws:${LWS_VER}"
  if [ -n "${CI:-}" ]; then
    echo "CI set: skipping registry pulls — building the pinned lws module locally as ${REF}"
  else
    echo "registry pulls unavailable — building the lws manager locally from the pinned go module as ${REF}"
  fi
  "${DOCKER}" build -t "${REF}" -f - "${LWS_MOD}" <<'DOCKERFILE'
FROM golang:1.26 AS builder
# proxy.golang.org is unreachable from this network; mirror the operator
# image build (operator/Dockerfile) and use the goproxy.cn module proxy.
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Plain go build of cmd/main.go: the module's `make build` would also run
# controller-gen (manifests) which needs extra downloads.
RUN CGO_ENABLED=0 GOOS=linux go build -o manager ./cmd
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
DOCKERFILE
fi

echo "loading ${REF} into kind cluster ${KIND_CLUSTER}"
"${KIND_BIN}" load docker-image "${REF}" --name "${KIND_CLUSTER}"

# --- render + apply the full upstream default, CRDs included ---
# The leaderworkerset/disaggregatedset CRDs are part of this install: the
# chart ships only the ai.cubestack.io CRDs. Server-side apply (below) is
# required because client-side apply stamps a last-applied-configuration
# annotation that exceeds the per-object annotation limit on these large CRDs.
OUT="$(mktemp)"
trap 'rm -f "${OUT}"' EXIT
"${KUSTOMIZE}" build "${LWS_MOD}/config/default" > "${OUT}"
# replace(..., 1) assumes the image ref occurs exactly once in the bundle (true
# for v0.10.0: one manager Deployment). A future lws bump that duplicates the
# ref would leave the second occurrence at MANIFEST_REF — an image that cannot
# be pulled here — which fails loudly at rollout instead of confusingly.
if [ "${REF}" != "${MANIFEST_REF}" ]; then
  python3 - "${OUT}" "${MANIFEST_REF}" "${REF}" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read().replace("image: " + old, "image: " + new, 1)
open(path, "w").write(s)
PY
fi

echo "applying LWS controller manifests (incl. CRDs) to ${CTX}"
"${KUBECTL}" --context "${CTX}" apply --server-side -f "${OUT}"

echo "waiting for ${DEPLOY} in ${NS} to be ready..."
"${KUBECTL}" --context "${CTX}" rollout status deployment/"${DEPLOY}" -n "${NS}" --timeout=300s
