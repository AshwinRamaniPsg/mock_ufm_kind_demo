#!/usr/bin/env bash
#
# Tear down every kind cluster, build a brand-new one, and deploy only the
# mock UFM into it. One command, from nothing to a working demo.
#
#   ./fresh-cluster.sh            # prompts before deleting existing clusters
#   ./fresh-cluster.sh --yes      # no prompt (CI / repeat runs)
#   ./fresh-cluster.sh --keep     # keep other clusters, replace ufm-demo only
#   ./fresh-cluster.sh --down     # delete the ufm-demo cluster and exit
#
# WARNING: without --keep this deletes ALL kind clusters on this machine,
# including unrelated ones. It never touches non-kind clusters.

set -euo pipefail

CLUSTER_NAME="ufm-demo"
NAMESPACE="ufm-demo"
IMAGE="ufm-mock:dev"
HOST_PORT=9888   # mock UFM REST + dashboard
EFS_PORT=8989    # EFS plugin configuration API
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ASSUME_YES=false
KEEP_OTHERS=false
TEAR_DOWN=false

for arg in "$@"; do
  case "${arg}" in
    -y|--yes)  ASSUME_YES=true ;;
    --keep)    KEEP_OTHERS=true ;;
    --down)    TEAR_DOWN=true ;;
    # Print the header comment block, stopping at the first non-comment line.
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' \
                 "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown option: ${arg} (try --help)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# output helpers
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  BOLD=$'\033[1m'; CYAN=$'\033[1;36m'; GREEN=$'\033[1;32m'
  YELLOW=$'\033[1;33m'; RED=$'\033[1;31m'; DIM=$'\033[0;90m'; RESET=$'\033[0m'
else
  BOLD=""; CYAN=""; GREEN=""; YELLOW=""; RED=""; DIM=""; RESET=""
fi

step() { printf '\n%s── %s%s\n' "${CYAN}" "$*" "${RESET}"; }
info() { printf '%s   %s%s\n' "${DIM}" "$*" "${RESET}"; }
ok()   { printf '%s   ✓ %s%s\n' "${GREEN}" "$*" "${RESET}"; }
warn() { printf '%s   ! %s%s\n' "${YELLOW}" "$*" "${RESET}"; }
die()  { printf '\n%serror: %s%s\n' "${RED}" "$*" "${RESET}" >&2; exit 1; }

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
step "Preflight"

for tool in docker kind kubectl go; do
  command -v "${tool}" >/dev/null 2>&1 || die "${tool} not found on PATH"
done
ok "docker, kind, kubectl, go present"

docker info >/dev/null 2>&1 || die "the Docker daemon is not running — start Docker Desktop"
ok "docker daemon is running"

for manifest in kind-cluster.yaml ufm-mock.yaml ufm-mock-nodeport.yaml; do
  [ -f "${REPO_DIR}/deploy/${manifest}" ] || die "missing deploy/${manifest}"
done
ok "manifests found"

# ---------------------------------------------------------------------------
# teardown-only mode
# ---------------------------------------------------------------------------
if [ "${TEAR_DOWN}" = true ]; then
  step "Deleting cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" 2>&1 | sed 's/^/   /' || true
  ok "done"
  exit 0
fi

# ---------------------------------------------------------------------------
# destroy existing clusters
# ---------------------------------------------------------------------------
step "Existing kind clusters"

EXISTING="$(kind get clusters 2>/dev/null | grep -v '^No kind clusters' || true)"

if [ -z "${EXISTING}" ]; then
  info "none found"
  DOOMED=""
elif [ "${KEEP_OTHERS}" = true ]; then
  DOOMED="$(printf '%s\n' "${EXISTING}" | grep -Fx "${CLUSTER_NAME}" || true)"
  printf '%s\n' "${EXISTING}" | sed "s/^/     /"
  info "--keep given: only ${CLUSTER_NAME} will be replaced"
else
  DOOMED="${EXISTING}"
  printf '%s\n' "${EXISTING}" | sed "s/^/     /"
fi

if [ -n "${DOOMED}" ]; then
  COUNT="$(printf '%s\n' "${DOOMED}" | wc -l | tr -d ' ')"
  if [ "${ASSUME_YES}" != true ]; then
    printf '\n%sThis will permanently delete %s kind cluster(s) and everything in them:%s\n' \
      "${YELLOW}${BOLD}" "${COUNT}" "${RESET}"
    printf '%s\n' "${DOOMED}" | sed 's/^/     - /'
    printf '%sType %sdelete%s to continue: %s' "${YELLOW}" "${BOLD}" "${RESET}${YELLOW}" "${RESET}"
    read -r REPLY
    [ "${REPLY}" = "delete" ] || die "aborted — nothing was deleted"
  fi

  step "Deleting cluster(s)"
  while IFS= read -r cluster; do
    [ -n "${cluster}" ] || continue
    info "deleting ${cluster}..."
    kind delete cluster --name "${cluster}" >/dev/null 2>&1 || warn "could not delete ${cluster}"
  done <<< "${DOOMED}"
  ok "deleted"
fi

# ---------------------------------------------------------------------------
# create the cluster
# ---------------------------------------------------------------------------
# The kind extraPortMappings bind host ports, so they must be free. This is
# checked after deleting clusters, since the cluster being replaced is normally
# the thing holding them.
step "Checking host ports"
for port in "${HOST_PORT}" "${EFS_PORT}"; do
  if lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1; then
    warn "port ${port} is still in use:"
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | tail -n +2 | sed 's/^/     /'
    die "free port ${port} first (a stray ./ufm-mock? try: pkill -f ufm-mock)"
  fi
done
ok "host ports ${HOST_PORT} and ${EFS_PORT} are free"

step "Creating kind cluster '${CLUSTER_NAME}'"
info "this usually takes 30-60 seconds"
kind create cluster --config "${REPO_DIR}/deploy/kind-cluster.yaml" --wait 120s 2>&1 | sed 's/^/   /'
kubectl cluster-info --context "kind-${CLUSTER_NAME}" >/dev/null 2>&1 \
  || die "cluster came up but is not reachable"
ok "cluster ready, kubectl context is kind-${CLUSTER_NAME}"

# ---------------------------------------------------------------------------
# build and load the image
# ---------------------------------------------------------------------------
# The binary is cross-compiled on the host and packaged into a FROM scratch
# image, so this step pulls nothing — no builder image, no base image. The
# host Go may be a different GOARCH than the cluster node (an amd64 Go
# toolchain on an arm64 Mac is common), so target the node explicitly.
step "Building ${IMAGE}"

NODE_ARCH="$(docker exec "${CLUSTER_NAME}-control-plane" uname -m 2>/dev/null || uname -m)"
case "${NODE_ARCH}" in
  aarch64|arm64) GOARCH=arm64; PLATFORM=linux/arm64 ;;
  x86_64|amd64)  GOARCH=amd64; PLATFORM=linux/amd64 ;;
  *) die "unsupported cluster node architecture: ${NODE_ARCH}" ;;
esac
info "cluster node is ${NODE_ARCH}, building for ${PLATFORM}"

mkdir -p "${REPO_DIR}/dist"
( cd "${REPO_DIR}" && CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" \
    go build -trimpath -ldflags="-s -w" -o dist/ufm-mock . ) \
  || die "go build failed"
ok "binary built ($(du -h "${REPO_DIR}/dist/ufm-mock" | cut -f1 | tr -d ' '))"

docker build -q --platform "${PLATFORM}" -t "${IMAGE}" "${REPO_DIR}" >/dev/null \
  || die "docker build failed"
ok "image built ($(docker images "${IMAGE}" --format '{{.Size}}' | head -1))"

step "Loading image into the cluster"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}" 2>&1 | sed 's/^/   /'
ok "loaded"

# ---------------------------------------------------------------------------
# deploy
# ---------------------------------------------------------------------------
step "Deploying the mock UFM and the EFS pipeline"
kubectl apply -f "${REPO_DIR}/deploy/ufm-mock.yaml" 2>&1 | sed 's/^/   /'
kubectl apply -f "${REPO_DIR}/deploy/ufm-mock-nodeport.yaml" 2>&1 | sed 's/^/   /'
kubectl apply -f "${REPO_DIR}/deploy/ufm-efs.yaml" 2>&1 | sed 's/^/   /'

step "Waiting for rollout"
for deployment in ufm-efs ufm-collector; do
  kubectl -n "${NAMESPACE}" rollout status "deploy/${deployment}" --timeout=120s \
    >/dev/null 2>&1 || warn "${deployment} did not become ready"
done
kubectl -n "${NAMESPACE}" rollout status deploy/ufm-mock --timeout=120s 2>&1 | sed 's/^/   /' \
  || {
    warn "rollout did not complete — recent events:"
    kubectl -n "${NAMESPACE}" get events --sort-by=.lastTimestamp 2>/dev/null | tail -10 | sed 's/^/     /'
    die "deployment failed"
  }
kubectl -n "${NAMESPACE}" rollout status deploy/ufm-client --timeout=120s >/dev/null 2>&1 || true
ok "pods are running"

# ---------------------------------------------------------------------------
# smoke test through the published host port
# ---------------------------------------------------------------------------
step "Smoke test via http://127.0.0.1:${HOST_PORT}"

BASE="http://127.0.0.1:${HOST_PORT}"
TOKEN="$(kubectl -n "${NAMESPACE}" get secret ufm-mock-auth -o jsonpath='{.data.token}' | base64 -d)"
AUTH="Authorization: Basic ${TOKEN}"

for attempt in $(seq 1 30); do
  if curl -sf -o /dev/null "${BASE}/health" 2>/dev/null; then break; fi
  [ "${attempt}" -eq 30 ] && die "mock never became reachable on port ${HOST_PORT}"
  sleep 1
done
ok "health endpoint responding"

VERSION="$(curl -s -H "${AUTH}" "${BASE}/ufmRestV3/app/ufm_version")"
info "GET /ufmRestV3/app/ufm_version -> ${VERSION}"

# awk rather than grep -o | wc -l: grep exits non-zero on no match, which
# pipefail would turn into a script abort on an empty fabric.
PORT_COUNT="$(curl -s -H "${AUTH}" "${BASE}/ufmRestV3/resources/ports" \
  | awk -F'"guid"' '{print NF-1}')"
ok "${PORT_COUNT} InfiniBand ports discovered"

UNAUTH="$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/ufmRestV3/app/ufm_version")"
[ "${UNAUTH}" = "401" ] && ok "auth enforced (401 without a token)" \
  || warn "expected 401 without a token, got ${UNAUTH}"

UI_CODE="$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/ui")"
[ "${UI_CODE}" = "200" ] && ok "dashboard served at ${BASE}/ui" \
  || warn "dashboard returned ${UI_CODE}"

# ---------------------------------------------------------------------------
# end-to-end EFS check: raise an event, confirm it reaches the collector
# ---------------------------------------------------------------------------
step "Smoke test: the EFS pipeline"

EFS_BASE="http://127.0.0.1:${EFS_PORT}"
for attempt in $(seq 1 30); do
  if curl -sf -o /dev/null "${EFS_BASE}/plugin/efs/health" 2>/dev/null; then break; fi
  [ "${attempt}" -eq 30 ] && die "EFS never became reachable on port ${EFS_PORT}"
  sleep 1
done
ok "EFS config API responding"

curl -s -X PUT -H 'Content-Type: application/json' "${EFS_BASE}/plugin/efs/conf" \
  -d '{"streaming":{"enabled":true},"fluent-bit-endpoint":{"enabled":true}}' >/dev/null
curl -s -X PUT -H "${AUTH}" -H 'Content-Type: application/json' \
  "${BASE}/ufmRestV3/app/syslog" \
  -d "{\"active\":true,\"destination\":\"ufm-efs.${NAMESPACE}.svc.cluster.local:5140\",\"level\":\"WARNING\",\"ufm_log\":true,\"events_log\":true}" >/dev/null
ok "UFM syslog pointed at the EFS plugin"

curl -s -X POST -H "${AUTH}" -H 'Content-Type: application/json' \
  "${BASE}/ufmRestV3/app/events/external_event" \
  -d '{"event_id":331,"name":"Link Down","severity":"Critical","object_name":"gpu-node-002","otype":"IBPort","category":"Hardware","description":"startup smoke test"}' >/dev/null
# Also raise an Info event, which the WARNING filter must drop.
curl -s -X POST -H "${AUTH}" -H 'Content-Type: application/json' \
  "${BASE}/ufmRestV3/app/events/external_event" \
  -d '{"event_id":67,"severity":"Info","object_name":"default","description":"must be filtered"}' >/dev/null
sleep 2

EFS_RECEIVED="$(curl -s "${EFS_BASE}/plugin/efs/stats" \
  | awk -F'"received":' '{print $2}' | awk -F',' '{print $1}' | tr -d ' }')"
if [ "${EFS_RECEIVED}" = "1" ]; then
  ok "EFS received 1 event — the Info event was correctly filtered out"
else
  warn "EFS received '${EFS_RECEIVED}', expected 1"
fi

ALARM_COUNT="$(curl -s -H "${AUTH}" "${BASE}/ufmRestV3/app/alarms" \
  | awk -F'"id"' '{print NF-1}')"
ok "${ALARM_COUNT} alarm(s) raised"

if kubectl -n "${NAMESPACE}" logs deploy/ufm-collector 2>/dev/null | grep -q "ufm_syslog"; then
  ok "collector received the forwarded event"
else
  warn "collector has not logged the event yet"
fi

# Leave a clean board for the demo.
curl -s -X DELETE -H "${AUTH}" "${BASE}/ufmRestV3/app/alarms" >/dev/null
ok "alarms cleared, ready to demo"

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
printf '\n%s%s\n' "${GREEN}${BOLD}" "Mock UFM is up.${RESET}"
cat <<EOF

  ${BOLD}From your laptop${RESET} (no port-forward needed)
    export UFM_URL=${BASE}
    export UFM_TOKEN=${TOKEN}
    curl -s -H "Authorization: Basic \$UFM_TOKEN" \$UFM_URL/ufmRestV3/resources/ports

  ${BOLD}Dashboard${RESET}
    open ${BASE}/ui

  ${BOLD}Run the walkthroughs${RESET}
    UFM_URL=${BASE} UFM_TOKEN=${TOKEN} ./demo.sh        # fabric + pkeys
    UFM_URL=${BASE} UFM_TOKEN=${TOKEN} ./demo-efs.sh    # alarms + EFS streaming

  ${BOLD}Watch events stream through EFS${RESET}
    kubectl -n ${NAMESPACE} logs -f deploy/ufm-collector

  ${BOLD}From inside the cluster${RESET}
    kubectl -n ${NAMESPACE} exec -it deploy/ufm-client -- sh
    # \$UFM_URL and \$UFM_TOKEN are already set in that pod

  ${BOLD}Watch it work${RESET}
    kubectl -n ${NAMESPACE} logs -f deploy/ufm-mock
    kubectl -n ${NAMESPACE} get pods

  ${BOLD}Tear down${RESET}
    ./fresh-cluster.sh --down

EOF
