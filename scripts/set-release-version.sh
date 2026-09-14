#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./scripts/set-release-version.sh vMAJOR.MINOR.PATCH

Example:
  ./scripts/set-release-version.sh v1.14.0

Updates the OpsCart application release version across the same release
surfaces validated by scripts/check-release-version.sh.

This script intentionally does NOT change Helm Chart.yaml "version:".
It only changes Chart.yaml "appVersion:".
EOF
}

if [[ $# -ne 1 ]]; then
  usage >&2
  exit 2
fi

target="$1"

if [[ ! "$target" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: version must match vMAJOR.MINOR.PATCH (example: v1.14.0)" >&2
  exit 2
fi

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$repo_root" ]]; then
  echo "error: run this script from inside the OpsCart git repository" >&2
  exit 1
fi

cd "$repo_root"

checker="./scripts/check-release-version.sh"
if [[ ! -x "$checker" ]]; then
  echo "error: $checker is missing or not executable" >&2
  exit 1
fi

files=(
  "cmd/opscart-dashboard/version.go"
  "helm/opscart-watcher/Chart.yaml"
  "helm/opscart-watcher/values.yaml"
  "helm/opscart-watcher/values-oauth2-proxy-example.yaml"
  "deploy/dashboard.yaml"
  "helm/opscart-watcher/README.md"
)

for file in "${files[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "error: required release surface not found: $file" >&2
    exit 1
  fi
done

current_output="$("$checker")"
echo "$current_output"

current="$(printf '%s\n' "$current_output" | sed -nE 's/^Release surfaces consistently use (v[0-9]+\.[0-9]+\.[0-9]+)\.$/\1/p')"

if [[ -z "$current" ]]; then
  echo "error: could not determine current consistent release version" >&2
  exit 1
fi

if [[ "$current" == "$target" ]]; then
  echo "Release surfaces already use $target."
  exit 0
fi

echo
echo "Updating release version: $current -> $target"

python3 - "$current" "$target" <<'PY'
from pathlib import Path
import re
import sys

current, target = sys.argv[1], sys.argv[2]

def replace_exact(path_str, old, new, description):
    path = Path(path_str)
    text = path.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(
            f"error: {description}: expected exactly 1 occurrence in {path}, found {count}"
        )
    path.write_text(text.replace(old, new, 1))
    print(f"updated: {path} ({description})")

def replace_regex(path_str, pattern, repl, description):
    path = Path(path_str)
    text = path.read_text()
    updated, count = re.subn(pattern, repl, text, count=1)
    if count != 1:
        raise SystemExit(
            f"error: {description}: expected exactly 1 matching line in {path}, found {count}"
        )
    path.write_text(updated)
    print(f"updated: {path} ({description})")

replace_regex(
    "cmd/opscart-dashboard/version.go",
    rf'(?m)^(\s*(?:const\s+)?(?:Version|version)\s*=\s*["\']){re.escape(current)}(["\']\s*)$',
    rf'\g<1>{target}\g<2>',
    "dashboard binary version",
)

replace_regex(
    "helm/opscart-watcher/Chart.yaml",
    rf'(?m)^(\s*appVersion:\s*["\']?){re.escape(current)}(["\']?\s*)$',
    rf'\g<1>{target}\g<2>',
    "Helm appVersion",
)

replace_regex(
    "helm/opscart-watcher/values.yaml",
    rf'(?m)^(\s*tag:\s*["\']?){re.escape(current)}(["\']?\s*)$',
    rf'\g<1>{target}\g<2>',
    "Helm default image tag",
)

replace_regex(
    "helm/opscart-watcher/values-oauth2-proxy-example.yaml",
    rf'(?m)^(\s*tag:\s*["\']?){re.escape(current)}(["\']?\s*)$',
    rf'\g<1>{target}\g<2>',
    "Helm OAuth example image tag",
)

replace_exact(
    "deploy/dashboard.yaml",
    f"image: ghcr.io/opscart/opscart-dashboard:{current}",
    f"image: ghcr.io/opscart/opscart-dashboard:{target}",
    "standalone dashboard image tag",
)

replace_exact(
    "helm/opscart-watcher/README.md",
    f"| `image.tag` | `{current}` | Image tag |",
    f"| `image.tag` | `{target}` | Image tag |",
    "Helm README image tag",
)
PY

echo
echo "Validating release surfaces..."
"$checker" "$target"

echo
echo "Changed release files:"
git diff --name-only -- "${files[@]}"

echo
echo "Review with:"
echo "  git diff -- ${files[*]}"
