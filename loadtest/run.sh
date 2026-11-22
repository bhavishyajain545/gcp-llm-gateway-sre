#!/usr/bin/env bash
# Run the k6 load profile inside the cluster and stream its output.
#   loadtest/run.sh            # 2-minute mixed profile (see k6.js)
#   DURATION=60s PEAK_VUS=12 loadtest/run.sh
set -euo pipefail
cd "$(dirname "$0")/.."
NS=llm-gateway

KEYS_JSON="$(terraform -chdir=terraform output -json tenant_keys)"
PLATFORM="$(echo "$KEYS_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["team-platform"])')"
BURST="$(echo "$KEYS_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["team-burst"])')"

kubectl -n "$NS" delete job k6 --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic k6-keys --from-literal=platform="$PLATFORM" --from-literal=burst="$BURST" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create configmap k6-script --from-file=k6.js=loadtest/k6.js --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed -e "s|value: 120s|value: ${DURATION:-120s}|" -e "s|value: \"24\"|value: \"${PEAK_VUS:-24}\"|" loadtest/job.yaml | kubectl apply -f -

echo "waiting for the k6 pod…"
kubectl -n "$NS" wait --for=condition=Ready pod -l app=k6 --timeout=180s >/dev/null || true
kubectl -n "$NS" logs -f job/k6
