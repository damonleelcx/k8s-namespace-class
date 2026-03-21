#!/usr/bin/env bash
set -euo pipefail

MAX_WAIT_ATTEMPTS=60

sleep_step() {
  sleep 2
}

run_step() {
  local message="$1"
  shift
  echo
  echo "==> $message"
  "$@"
  sleep_step
}

wait_multikind_v1() {
  echo
  echo "==> Wait until app-sandbox has ServiceAccount + ConfigMap (profile=v1)"
  local i
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    if kubectl -n app-sandbox get serviceaccount app-runner >/dev/null 2>&1 \
      && kubectl -n app-sandbox get configmap class-config >/dev/null 2>&1; then
      local prof
      prof="$(kubectl -n app-sandbox get configmap class-config -o jsonpath='{.data.profile}' 2>/dev/null || true)"
      if [[ "$prof" == "v1" ]]; then
        echo "Ready: app-runner + class-config profile=v1"
        sleep_step
        return 0
      fi
    fi
    echo "Waiting ($i/$MAX_WAIT_ATTEMPTS)..."
    sleep_step
  done
  echo "Timed out waiting for multi-kind v1 reconcile."
  return 1
}

wait_multikind_v2() {
  echo
  echo "==> Wait until NamespaceClass v2 is reconciled (profile=v2 + LimitRange)"
  local i
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    local prof
    prof="$(kubectl -n app-sandbox get configmap class-config -o jsonpath='{.data.profile}' 2>/dev/null || true)"
    if [[ "$prof" == "v2" ]] && kubectl -n app-sandbox get limitrange mem-limit-range >/dev/null 2>&1; then
      echo "Ready: profile=v2 and mem-limit-range exists"
      sleep_step
      return 0
    fi
    echo "Waiting ($i/$MAX_WAIT_ATTEMPTS)..."
    sleep_step
  done
  echo "Timed out waiting for multi-kind v2 reconcile."
  return 1
}

wait_sa_recreated() {
  echo
  echo "==> Wait until controller recreates ServiceAccount app-runner"
  local i
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    if kubectl -n app-sandbox get serviceaccount app-runner >/dev/null 2>&1; then
      echo "ServiceAccount app-runner is present"
      sleep_step
      return 0
    fi
    echo "Waiting ($i/$MAX_WAIT_ATTEMPTS)..."
    sleep_step
  done
  echo "Timed out waiting for ServiceAccount recreate."
  return 1
}

echo "Starting multi-kind (ServiceAccount / ConfigMap / LimitRange) test flow..."
sleep_step

run_step "Apply CRD namespaceclasses" \
  kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
run_step "Apply CRD namespaceclasschangerequests" \
  kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
run_step "Apply NamespaceClass multi-kind-demo v1 (SA + ConfigMap)" \
  kubectl apply -f config/samples/namespaceclass-multikind-v1.yaml
run_step "Apply namespace app-sandbox" \
  kubectl apply -f config/samples/namespace-app-sandbox.yaml
wait_multikind_v1

run_step "Show v1 objects" \
  kubectl -n app-sandbox get serviceaccount,configmap,limitrange
run_step "Upgrade NamespaceClass to v2 (ConfigMap data + LimitRange)" \
  kubectl apply -f config/samples/namespaceclass-multikind-v2.yaml
wait_multikind_v2

run_step "Show v2 objects" \
  kubectl -n app-sandbox get serviceaccount,configmap,limitrange
run_step "ConfigMap data (expect profile=v2 and note key)" \
  kubectl -n app-sandbox get configmap class-config -o yaml
run_step "LimitRange detail" \
  kubectl -n app-sandbox get limitrange mem-limit-range -o yaml

run_step "Delete managed ServiceAccount to simulate drift" \
  kubectl -n app-sandbox delete serviceaccount app-runner --wait=true
wait_sa_recreated
run_step "Verify ServiceAccount after reconcile" \
  kubectl -n app-sandbox get serviceaccount app-runner -o yaml

echo
echo "Multi-kind test flow complete."
