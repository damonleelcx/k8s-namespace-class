#!/usr/bin/env bash
set -euo pipefail

MAX_WAIT_ATTEMPTS=60

sleep_step() {
  sleep 2
}

confirm_continue() {
  local prompt="$1"
  read -r -p "$prompt [y/N]: " answer
  case "${answer,,}" in
    y|yes) ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

run_step() {
  local message="$1"
  shift
  echo
  echo "==> $message"
  "$@"
  sleep_step
}

wait_for_public_ready() {
  echo
  echo "==> Wait until web-portal is stable on public-network"
  local i
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    if kubectl -n web-portal get networkpolicy allow-public-ingress >/dev/null 2>&1; then
      last_class="$(kubectl get namespace web-portal -o jsonpath='{.metadata.annotations.namespaceclass\.akuity\.io/last-class}' 2>/dev/null || true)"
      if [[ "$last_class" == "public-network" ]]; then
        echo "Ready: allow-public-ingress exists and last-class=public-network"
        sleep_step
        return 0
      fi
    fi
    echo "Waiting ($i/$MAX_WAIT_ATTEMPTS)..."
    sleep_step
  done
  echo "Timed out waiting for initial public-network state."
  return 1
}

wait_recommendation_id() {
  local class_name="$1"
  local i
  echo
  echo "==> Wait for recommendation ID on ${class_name}"
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    rec_id="$(kubectl get namespaceclass "${class_name}" -o jsonpath='{.status.recommendations[0].id}' 2>/dev/null || true)"
    if [[ -n "${rec_id}" ]]; then
      echo "${rec_id}"
      sleep_step
      return 0
    fi
    echo "No recommendation yet ($i/$MAX_WAIT_ATTEMPTS)..."
    sleep_step
  done
  echo "Timed out waiting for recommendation on ${class_name}."
  return 1
}

echo "Starting NetworkPolicy / AI approval test flow..."
sleep_step

run_step "Apply CRD namespaceclasses" \
  kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
run_step "Apply CRD namespaceclasschangerequests" \
  kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
run_step "Apply sample namespace classes" \
  kubectl apply -f config/samples/namespaceclass-public-internal.yaml
run_step "Apply sample namespace web-portal" \
  kubectl apply -f config/samples/namespace-web-portal.yaml
wait_for_public_ready

run_step "Switch web-portal to internal-network" \
  kubectl label namespace web-portal namespaceclass.akuity.io/name=internal-network --overwrite
run_step "Check networkpolicy list" \
  kubectl -n web-portal get networkpolicy
run_step "Check allow-public-ingress (expected missing after switch)" \
  kubectl -n web-portal get networkpolicy allow-public-ingress
run_step "Check allow-vpn-only details" \
  kubectl -n web-portal get networkpolicy allow-vpn-only -o yaml
run_step "Wait for watcher analysis on internal-network" \
  kubectl get namespaceclass internal-network -o yaml
wait_recommendation_id "internal-network"

confirm_continue "Please update spec.recommendationID in config/samples/namespaceclass-change-request-internal.yaml. Continue?"
run_step "Apply internal change request" \
  kubectl apply -f config/samples/namespaceclass-change-request-internal.yaml

run_step "Re-check networkpolicy list" \
  kubectl -n web-portal get networkpolicy
run_step "Re-check allow-public-ingress" \
  kubectl -n web-portal get networkpolicy allow-public-ingress
run_step "Re-check allow-vpn-only details" \
  kubectl -n web-portal get networkpolicy allow-vpn-only -o yaml

run_step "Create drift by deleting allow-public-ingress" \
  kubectl annotate namespaceclass public-network namespaceclass.akuity.io/ai-drift-hold-seconds=45 --overwrite
run_step "Delete allow-public-ingress after hold is enabled" \
  kubectl -n web-portal delete networkpolicy allow-public-ingress
run_step "Wait for watcher analysis on public-network" \
  kubectl get namespaceclass public-network -o yaml
wait_recommendation_id "public-network"

confirm_continue "Please update spec.recommendationID in config/samples/namespaceclass-change-request.yaml. Continue?"
run_step "Apply public change request" \
  kubectl apply -f config/samples/namespaceclass-change-request.yaml

run_step "Verify public change request status" \
  kubectl get namespaceclasschangerequest public-network-approval -o yaml
run_step "Verify public-network class status" \
  kubectl get namespaceclass public-network -o yaml
run_step "Verify web-portal network policies" \
  kubectl -n web-portal get networkpolicy

echo
echo "NetworkPolicy test flow complete."
