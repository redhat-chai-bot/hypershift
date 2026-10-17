#!/usr/bin/env bash
set -euo pipefail

repo_root="$1"
output="$2"
shift 2
source_dir="$(mktemp -d)"
trap 'rm -rf "$source_dir"' EXIT

awk '
    /^func fieldName\(/ { skipping = 1; next }
    skipping && /^}/ { skipping = 0; next }
    !skipping { print }
' "$repo_root/hack/tools/vendor/github.com/ahmetb/gen-crd-api-reference-docs/main.go" > "$source_dir/main.go"
cp "$repo_root/hack/tools/api-docs/field_name.go" "$source_dir/field_name.go"
cd "$repo_root/hack/tools"
env "$@" build -tags=tools -o "$output" "$source_dir/main.go" "$source_dir/field_name.go"
