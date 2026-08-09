#!/usr/bin/env bash
set -euo pipefail

command -v kubectl >/dev/null 2>&1 || {
  echo "ERROR: kubectl is required."
  exit 1
}

context="$(kubectl config current-context 2>/dev/null || true)"

if [[ -z "$context" ]]; then
  echo "ERROR: no current Kubernetes context."
  exit 1
fi

namespaces="$(
  kubectl get namespaces \
    -l opscart.io/failure-lab=true \
    -o name
)"

if [[ -z "$namespaces" ]]; then
  echo "No OpsCart failure-lab namespaces found."
  exit 0
fi

echo "Context: $context"
echo "The following namespaces will be deleted:"
echo "$namespaces"
echo

if [[ "${1:-}" != "--yes" ]]; then
  read -r -p "Delete the OpsCart failure lab? [y/N] " answer

  case "$answer" in
    y|Y|yes|YES) ;;
    *)
      echo "Cancelled."
      exit 0
      ;;
  esac
fi

kubectl delete namespace \
  -l opscart.io/failure-lab=true \
  --ignore-not-found=true \
  --wait=true

echo "OpsCart failure lab removed."