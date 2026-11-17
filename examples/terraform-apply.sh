#!/usr/bin/env bash
# Wrap `terraform apply` so every apply lands on the whatchanged timeline.
# Usage: WHATCHANGED_URL=... WHATCHANGED_TOKEN=... ./terraform-apply.sh [apply args]
set -euo pipefail

stack="$(basename "$PWD")"
status=0
out="$(terraform apply "$@" 2>&1 | tee /dev/stderr)" || status=$?

summary="$(grep -Eo 'Apply complete! Resources: .*' <<<"$out" | head -1 || true)"
[ -n "$summary" ] || summary="apply failed (exit $status)"

whatchanged push \
  --source terraform \
  --kind Stack \
  --name "$stack" \
  --action config \
  --impact high \
  --summary "$summary" \
  --actor "${USER:-ci}" || echo "warning: could not record change in whatchanged" >&2

exit "$status"
