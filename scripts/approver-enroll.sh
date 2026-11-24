#!/usr/bin/env bash
# Print the single-use link that enrolls your first passkey with moose-approver.
#
# `moose-approver enroll` writes a hashed enrollment token into the approver's
# DynamoDB table, so it needs the deployment's settings and AWS credentials. This
# runs it in a container (the binary is built for linux), with the settings taken
# from `terraform output approver_env` and short-lived credentials exported from
# your AWS profile. Run it after the apply and open the printed link on the phone
# that will hold the passkey. The link works once and expires in 15 minutes.
#
#   scripts/approver-enroll.sh            # AWS_PROFILE defaults to change-fabric
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tf_dir="${TF_DIR:-$repo/deploy/terraform/email}"
profile="${AWS_PROFILE:-change-fabric}"
region="${AWS_REGION:-us-east-1}"
image="${MOOSE_GO_IMAGE:-golang:1.27.3}"
bin="$repo/dist/moose-approver"

if [[ ! -x "$bin" ]]; then
  echo "dist/moose-approver is missing; run 'make build' first" >&2
  exit 1
fi

env_json="$(terraform -chdir="$tf_dir" output -json approver_env)"
env_args=()
while IFS= read -r line; do
  env_args+=(-e "$line")
done < <(printf '%s' "$env_json" | python3 -c '
import json, sys
for k, v in json.load(sys.stdin).items():
    print(f"{k}={v}")
')

# Temporary credentials from the profile (works with SSO); nothing is written to disk.
creds="$(aws configure export-credentials --profile "$profile" --format env)"
cred_args=()
while IFS= read -r line; do
  line="${line#export }"
  cred_args+=(-e "$line")
done <<<"$creds"

exec docker run --rm \
  -v "$bin:/usr/local/bin/moose-approver:ro" \
  -e "AWS_REGION=$region" \
  -e "MOOSE_CONFIG=/etc/moose-approver.yaml" \
  -v "$repo/deploy/lambda/moose-approver.yaml:/etc/moose-approver.yaml:ro" \
  "${env_args[@]}" "${cred_args[@]}" \
  "$image" moose-approver enroll
