#!/usr/bin/env bash
set -euo pipefail

lab_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manifest_dir="$lab_dir/manifests"

namespaces=(
  payments
  inventory
  auth
  notifications
  data-pipeline
  api-gateway
  platform-ops
  reporting
)

command -v kubectl >/dev/null 2>&1 || {
  echo "ERROR: kubectl is required."
  exit 1
}

context="$(kubectl config current-context 2>/dev/null || true)"

if [[ -z "$context" ]]; then
  echo "ERROR: no current Kubernetes context."
  exit 1
fi

echo "OpsCart failure lab"
echo "Context: $context"
echo
echo "This deploys intentionally broken workloads."
echo "Use a disposable or non-production cluster."
echo

for namespace in "${namespaces[@]}"; do
  if kubectl get namespace "$namespace" >/dev/null 2>&1; then
    owner="$(
      kubectl get namespace "$namespace" \
        -o jsonpath='{.metadata.labels.opscart\.io/failure-lab}' \
        2>/dev/null || true
    )"

    if [[ "$owner" != "true" ]]; then
      echo "ERROR: namespace '$namespace' already exists but is not owned by this lab."
      echo "No resources were applied."
      exit 1
    fi
  fi
done

if [[ "${1:-}" != "--yes" ]]; then
  read -r -p "Deploy to context '$context'? [y/N] " answer

  case "$answer" in
    y|Y|yes|YES) ;;
    *)
      echo "Cancelled."
      exit 0
      ;;
  esac
fi

kubectl apply -f "$manifest_dir/scenarios.yaml"
kubectl apply -f "$manifest_dir/probe-failure.yaml"

echo
echo "Failure lab applied."
echo "Allow 30-60 seconds for failures and Kubernetes events to develop."
echo
echo "Run:"
echo "  opscart-scan triage --cluster \"$context\" --next-steps"