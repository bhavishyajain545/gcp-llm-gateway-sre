#!/usr/bin/env bash
# Smoke test against a running gateway: models, stream + TTFT, exact and semantic cache, allow-list, quotas.
#   ./smoke.sh                 # through the load balancer (Gateway address)
#   ./smoke.sh --port-forward  # through kubectl port-forward (before the LB is ready)
set -uo pipefail
cd "$(dirname "$0")"
export USE_GKE_GCLOUD_AUTH_PLUGIN=True
NS=llm-gateway

if [[ "${1:-}" == "--port-forward" ]]; then
  (timeout 120 kubectl -n "$NS" port-forward deploy/llm-gateway 18080:8080 >/dev/null 2>&1 &)
  sleep 3; GW=http://localhost:18080
else
  GW="http://$(kubectl -n "$NS" get gateway external -o jsonpath='{.status.addresses[0].value}')"
fi
KEYS="$(terraform -chdir=terraform output -json tenant_keys)"
key() { echo "$KEYS" | python3 -c "import sys,json; print(json.load(sys.stdin)['$1'])"; }
K_PLAT=$(key team-platform); K_SUP=$(key team-support); K_BURST=$(key team-burst)
Q='What is an error budget? Answer in two sentences.'
H=(-H "content-type: application/json")
c() { curl -s --max-time 60 "$@"; }

echo "== $GW"
echo "-- version:  $(c $GW/version)"
echo "-- models:   $(c $GW/v1/models -H "Authorization: Bearer $K_PLAT" | python3 -c 'import sys,json; print([m["id"] for m in json.load(sys.stdin)["data"]])')"
echo "-- stream:"
c -N $GW/v1/chat/completions -H "Authorization: Bearer $K_PLAT" "${H[@]}" -d "{\"model\":\"gemini-3.8-flash\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"$Q\"}]}" \
  -w '\nTTFT %{time_starttransfer}s total %{time_total}s\n' | grep -v '^$' | cut -c1-150 | head -8
echo "-- exact hit:"
c $GW/v1/chat/completions -D - -o /dev/null -H "Authorization: Bearer $K_PLAT" "${H[@]}" -d "{\"model\":\"gemini-3.8-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"$Q\"}]}" -w 'time %{time_total}s\n' | grep -iE 'x-cache|x-model|^time' | tr -d '\r'
echo "-- semantic hit (paraphrase):"
c $GW/v1/chat/completions -D - -o /dev/null -H "Authorization: Bearer $K_PLAT" "${H[@]}" -d '{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"In two sentences, what is an error budget?"}]}' -w 'time %{time_total}s\n' | grep -iE 'x-cache|x-model|^time' | tr -d '\r'
echo "-- team-support asks for flash (expect 403): $(c -o /dev/null -w '%{http_code}' $GW/v1/chat/completions -H "Authorization: Bearer $K_SUP" "${H[@]}" -d '{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}')"
echo "-- team-burst 14 parallel requests (burst 10, expect 429s):"
seq 1 14 | xargs -P 14 -I{} curl -s --max-time 60 -o /dev/null -w '%{http_code} ' $GW/v1/chat/completions -H "Authorization: Bearer $K_BURST" -H 'content-type: application/json' -H 'cache-control: no-cache' -d '{"model":"gemini-3.5-flash-lite","max_tokens":20,"messages":[{"role":"user","content":"Say ok number {}."}]}'; echo
echo "-- WAF payload (expect 403 through Cloud Armor): $(c -o /dev/null -w '%{http_code}' "$GW/healthz?q=%27%20OR%201%3D1--")"
echo "-- no key (expect 401): $(c -o /dev/null -w '%{http_code}' $GW/v1/chat/completions "${H[@]}" -d '{"messages":[{"role":"user","content":"hi"}]}')"
echo "-- metrics via LB (expect 404 when through the GFE): $(c -o /dev/null -w '%{http_code}' $GW/metrics)"
