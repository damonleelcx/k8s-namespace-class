#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Match main.go godotenv.Load(): load repo-root .env for the preflight check.
if [[ -f "${SCRIPT_DIR}/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${SCRIPT_DIR}/.env"
  set +a
fi
if [[ -n "${OPENAI_API_KEY:-}" ]]; then
  OPENAI_API_KEY="${OPENAI_API_KEY%$'\r'}"
  export OPENAI_API_KEY
fi

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

wait_cr_applied_or_fail() {
  echo
  echo "==> Wait until NamespaceClassChangeRequest multikind-demo-approval is Applied or Rejected"
  local i phase msg
  for ((i=1; i<=MAX_WAIT_ATTEMPTS; i++)); do
    phase="$(kubectl get namespaceclasschangerequest multikind-demo-approval -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    msg="$(kubectl get namespaceclasschangerequest multikind-demo-approval -o jsonpath='{.status.message}' 2>/dev/null || true)"
    if [[ "$phase" == "Applied" ]]; then
      echo "Change request phase=Applied"
      sleep_step
      return 0
    fi
    if [[ "$phase" == "Rejected" ]]; then
      echo "Change request rejected: ${msg}"
      kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml || true
      return 1
    fi
    echo "Waiting ($i/$MAX_WAIT_ATTEMPTS) phase=${phase:-...}..."
    sleep_step
  done
  echo "Timed out waiting for change request to finish."
  kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml || true
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

if [[ -z "${OPENAI_API_KEY:-}" ]]; then
  echo "OPENAI_API_KEY is not set after loading .env (if any). Add it to repo-root .env or export it, then run the manager with AI enabled, e.g.:"
  echo "  export OPENAI_API_KEY=your-key"
  echo "  go run . --ai-poll-interval=2s"
  exit 1
fi

echo "Starting multi-kind AI watcher + pull-request gate test flow..."
echo "(Requires controller built from this repo, with OPENAI_API_KEY set in this shell when you run the manager.)"
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

run_step "Require pull request URL on approvals for multi-kind-demo (annotation)" \
  kubectl annotate namespaceclass multi-kind-demo namespaceclass.akuity.io/require-pull-request=true --overwrite

run_step "Show class before drift" \
  kubectl get namespaceclass multi-kind-demo -o yaml

# Deleting a managed ServiceAccount is healed immediately by the namespace controller, so the AI
# watcher often never sees "missing resource" drift. Switch class twice to record switch annotations
# and produce durable drift for multi-kind-demo (same idea as public/internal in §7).
run_step "Apply empty staging NamespaceClass (for label switch only)" \
  kubectl apply -f config/samples/namespaceclass-multikind-staging.yaml
run_step "Switch app-sandbox to multi-kind-staging (strips demo resources)" \
  kubectl label namespace app-sandbox namespaceclass.akuity.io/name=multi-kind-staging --overwrite
sleep_step
sleep_step
run_step "Switch app-sandbox back to multi-kind-demo (recreates SA/CM; leaves switch drift ~10m)" \
  kubectl label namespace app-sandbox namespaceclass.akuity.io/name=multi-kind-demo --overwrite
wait_multikind_v1

run_step "Hint: watcher status (expect DriftDetected / recommendation soon)" \
  kubectl get namespaceclass multi-kind-demo -o yaml

wait_recommendation_id "multi-kind-demo"

confirm_continue "Update spec.recommendationID in config/samples/namespaceclass-change-request-multikind.yaml (pullRequestURL is already set for the PR gate). Continue?"

run_step "Apply multi-kind change request" \
  kubectl apply -f config/samples/namespaceclass-change-request-multikind.yaml

wait_cr_applied_or_fail
wait_sa_recreated

run_step "Verify change request (phase, appliedPullRequestURL)" \
  kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml
run_step "Verify namespaceclass status after apply" \
  kubectl get namespaceclass multi-kind-demo -o yaml
run_step "Verify app-sandbox ServiceAccount" \
  kubectl -n app-sandbox get serviceaccount app-runner -o yaml

echo
echo "Multi-kind AI + PR gate test flow complete."
