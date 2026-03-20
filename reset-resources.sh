#!/usr/bin/env bash
set -euo pipefail

echo "==> Deleting sample resources"
kubectl delete -f config/samples/namespaceclass-change-request.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-change-request-internal.yaml --ignore-not-found
kubectl delete -f config/samples/namespace-web-portal.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-public-internal.yaml --ignore-not-found

echo "==> Deleting CRDs (and all related custom resources)"
kubectl delete -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml --ignore-not-found
kubectl delete -f config/crd/bases/akuity.io_namespaceclasses.yaml --ignore-not-found

echo "==> Optional force cleanup in demo namespace"
kubectl -n web-portal delete configmap namespaceclass-inventory --ignore-not-found
kubectl -n web-portal delete networkpolicy allow-public-ingress allow-vpn-only --ignore-not-found
kubectl delete namespace web-portal --ignore-not-found

echo "==> Verifying cleanup"
kubectl get namespaceclass || true
kubectl get namespaceclasschangerequest || true
kubectl get ns web-portal || true

echo "Reset complete."
