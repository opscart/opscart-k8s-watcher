#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
cd "${repo_root}"

declared_version="$(sed -n 's/^const Version = "\(v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)"$/\1/p' cmd/opscart-dashboard/version.go)"
release_version="${1:-${declared_version}}"

if [[ ! "${release_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: release version must use vMAJOR.MINOR.PATCH; got '${release_version}'" >&2
  exit 1
fi

failed=0

expect_fixed() {
  local file="$1"
  local expected="$2"
  local description="$3"
  if ! grep -Fq -- "${expected}" "${file}"; then
    echo "error: ${description} in ${file} does not match ${release_version}" >&2
    failed=1
  fi
}

expect_regex() {
  local file="$1"
  local expected="$2"
  local description="$3"
  if ! grep -Eq -- "${expected}" "${file}"; then
    echo "error: ${description} in ${file} does not match ${release_version}" >&2
    failed=1
  fi
}

version_regex="${release_version//./\\.}"

expect_fixed cmd/opscart-dashboard/version.go "const Version = \"${release_version}\"" "dashboard binary version"
expect_regex helm/opscart-watcher/Chart.yaml "^[[:space:]]*appVersion:[[:space:]]*\"?${version_regex}\"?[[:space:]]*$" "Helm appVersion"
expect_regex helm/opscart-watcher/values.yaml "^[[:space:]]*tag:[[:space:]]*\"?${version_regex}\"?[[:space:]]*$" "Helm default image tag"
expect_regex helm/opscart-watcher/values-oauth2-proxy-example.yaml "^[[:space:]]*tag:[[:space:]]*\"?${version_regex}\"?[[:space:]]*$" "Helm OAuth example image tag"
expect_fixed deploy/dashboard.yaml "image: ghcr.io/opscart/opscart-dashboard:${release_version}" "standalone dashboard image tag"
expect_fixed helm/opscart-watcher/README.md "| \`image.tag\` | \`${release_version}\` | Image tag |" "Helm README image tag"

if ! grep -Eq '^version:[[:space:]]+[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$' helm/opscart-watcher/Chart.yaml; then
  echo "error: Helm chart version must use MAJOR.MINOR.PATCH" >&2
  failed=1
fi

if ((failed != 0)); then
  exit 1
fi

echo "Release surfaces consistently use ${release_version}."
