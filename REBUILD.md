# Rebuild from zero

Terraform owns every cloud resource (network, GKE Standard cluster and node pools, Memorystore, Cloud Armor,
Secret Manager, IAM/WIF, SLOs, alerts, dashboard); Cloud Build makes the first image; GitHub Actions builds the
rest keylessly; `deploy.sh` sequences the bring-up. This page is the exact order of operations plus the gotchas.

Time: ~25 minutes (cluster ~7 min, Memorystore ~5 min in parallel, image ~2 min, adapter + rollout ~3 min).
Cost while up: ~₹400-500/day (1 on-demand + 1-3 Spot e2-standard-2, Memorystore Basic 1 GB, global LB, NAT,
Cloud Armor policy). Zonal cluster = no GKE management fee. Tear it down after the demo.

## 0. Prerequisites

| Need | Notes |
|---|---|
| Billing-linked GCP account | Budget + kill switch recommended (this account unlinks billing automatically once net cost > ₹10). |
| `gcloud` (>= 590), `terraform` >= 1.6, `kubectl`, `gke-gcloud-auth-plugin` | `gcloud components install gke-gcloud-auth-plugin kubectl`; `export USE_GKE_GCLOUD_AUTH_PLUGIN=True`. |
| Go 1.27 (local tests only) | `cd gateway && go test ./...` (13 tests, miniredis, fake upstream). |
| Vertex AI access to `gemini-3.8-flash` (global), `gemini-3.5-flash-lite` (global), `gemini-embedding-001` (us-central1) | Default quota is enough for the demo. |
| A GitHub repo at `github_repo` (tfvars) | CI auth is bound to exactly this `owner/repo` by the WIF attribute condition. |
| Your public IP | `curl -s https://api.ipify.org` → `authorized_cidr`. Behind CGNAT the address rotates; use the ISP's block. |

Organisation constraints handled by the repo: `iam.automaticIamGrantsForDefaultServiceAccounts` → dedicated
node SA with explicit roles (`terraform/gke.tf`). Domain-restricted sharing: this project needs the per-project
override below because Memorystore's service-networking peering and Secret Manager bindings add Google-managed
principals.

```bash
export PROJECT_ID=bj-llm-gateway
gcloud services enable orgpolicy.googleapis.com --project "$PROJECT_ID"
cat > /tmp/drs.yaml <<EOF
name: projects/$PROJECT_ID/policies/iam.allowedPolicyMemberDomains
spec:
  rules:
  - allowAll: true
EOF
gcloud org-policies set-policy /tmp/drs.yaml --project "$PROJECT_ID" --billing-project "$PROJECT_ID"
```

## 1. Project

```bash
gcloud projects create "$PROJECT_ID" --name "LLM gateway SRE" --organization <ORG_ID>   # or without --organization
gcloud billing projects link "$PROJECT_ID" --billing-account <BILLING_ACCOUNT_ID>
gcloud config set project "$PROJECT_ID"
gcloud auth application-default set-quota-project "$PROJECT_ID"
```

## 2. Configure

```bash
git clone https://github.com/bhavishyajain545/gcp-llm-gateway-sre && cd gcp-llm-gateway-sre
cp terraform/terraform.tfvars.example terraform/terraform.tfvars
# project_id, zone, authorized_cidr, github_repo, alert_email; optionally armor_rpm_per_ip, node counts
```

If you change `project_id`, also change it in `k8s/base/serviceaccount.yaml`, `k8s/base/deployment.yaml`
(image, env) and `k8s/base/kustomization.yaml` (image name).

## 3. Bring-up

```bash
./deploy.sh      # 1 terraform apply   2 Cloud Build first image (sa-build)   3 kubeconfig
                 # 4 custom-metrics-stackdriver-adapter + WI annotation      5 kubectl apply -k k8s/base
                 # 6 wait for the Gateway address
```

Expected on the first apply: the two SLOs fail with "Cannot find metric" because the Prometheus series do not
exist yet. Generate traffic (step 4), wait ~5 minutes, then `./deploy.sh slo`: SLOs, burn-rate alert policies
and nothing else are created on the second pass.

## 4. Verify

`./smoke.sh` runs all of the following in one go (`./smoke.sh --port-forward` before the LB is ready).

```bash
IP=$(kubectl -n llm-gateway get gateway external -o jsonpath='{.status.addresses[0].value}')
KEY=$(terraform -chdir=terraform output -json tenant_keys | jq -r '."team-platform"')
curl -s http://$IP/v1/models -H "Authorization: Bearer $KEY" | jq
curl -sN http://$IP/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"What is an error budget?"}]}'
curl -si http://$IP/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"What is an error budget?"}]}' | grep -i x-cache   # exact
kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta2/namespaces/llm-gateway/pods/*/prometheus.googleapis.com|llm_inflight_requests|gauge" | jq '.items|length'
kubectl -n llm-gateway get pods,hpa,networkpolicy,podmonitoring,gateway,gcpbackendpolicy
```

Load test (runs inside the cluster, 2 minutes, three scenarios) and the second apply:

```bash
loadtest/run.sh                      # watch: kubectl -n llm-gateway get hpa -w
./deploy.sh slo
terraform -chdir=terraform output dashboard_url slo_url trace_url
```

Fault drill (fallback + breaker), Cloud Armor checks:

```bash
ADMIN=$(terraform -chdir=terraform output -raw admin_key)
curl -s -X POST http://$IP/admin/chaos -H "x-admin-key: $ADMIN" -d '{"model":"gemini-3.8-flash","mode":"error","seconds":90}'
# requests now return x-model-served: gemini-3.5-flash-lite; llm_breaker_open{model="gemini-3.8-flash"} = 1
curl -s -o /dev/null -w '%{http_code}\n' "http://$IP/healthz?q=%27%20or%201%3D1%20--%20"         # 403 (WAF)
seq 1 200 | xargs -P 20 -I{} curl -s -o /dev/null -w '%{http_code}\n' http://$IP/healthz | sort | uniq -c   # 429s after 120/min
```

Note: faults are per pod. With 2+ replicas, send the chaos call to each pod (port-forward) or scale to 1 for the drill.

## 5. CI (keyless)

```bash
terraform -chdir=terraform output -raw workload_identity_provider
gh secret set GCP_WORKLOAD_IDENTITY_PROVIDER --repo <owner/repo> --body "<that value>"
```

A push under `gateway/` runs tests, builds `sha-<short>`, pushes it as `sa-github-ci` (OIDC → WIF, no key) and
commits the tag into `k8s/base/kustomization.yaml`. Deploy it with `git pull && ./deploy.sh rollout`.

## 6. Re-record the demo (optional)

DemoProfile Chrome with `--remote-debugging-port=9222` signed in to the console; `DEMO_EXTRA_PATH` with gcloud
and ffmpeg; the cluster up with the load test done (dashboards have data). Then `python -u demo\scenes.py`.

## 7. Tear down (stop all cost)

```bash
./teardown.sh        # 1 kubectl delete -k k8s/base (Gateway → LB cleanup)  2 terraform destroy
                     # 3 Gateway-controller leftovers by hand  4 destroy again (VPC / PSA peering)  5 audit
./audit-billing.sh   # every section should be empty
```

Memorystore takes a few minutes to delete and the service-networking peering can refuse to go until it has;
the script waits and retries. The WIF pool is soft-deleted for 30 days (free). The empty project costs nothing.

## Gotchas met while building

- Gemini 3 models think by default: with `maxOutputTokens: 64` and no `thinkingConfig`, the reply was empty
  (61 thinking tokens ate the budget). The gateway sends `thinkingLevel: LOW` for `gemini-3*`.
- `gemini-3.8-flash-lite` does not exist as a text model (only `-tts`); the fallback is `gemini-3.5-flash-lite`.
- Pricing: the Billing catalog lists Gemini 3.8 Flash at $1.50 / $7.50 per 1M tokens; it is billed at half
  that through 2026-12-31. The gateway's cost metric uses the list price (`PRICES_JSON` overrides).
- HPA on a Prometheus metric needs the custom-metrics-stackdriver-adapter with Workload Identity
  (`monitoring.viewer`); the metric name in the HPA is `prometheus.googleapis.com|<metric>|gauge`.
- Workload Identity token requests go to the GKE metadata server on 169.254.169.252:988 and 169.254.169.254:80;
  a default-deny NetworkPolicy must allow both or every Google API call fails with "metadata: timeout".
- The Gateway's backend timeout defaults to 30 s: long streams need `GCPBackendPolicy.timeoutSec`.
- Cloud Armor and k6: run the load generator inside the cluster against the Service, otherwise the whole test
  comes from one NAT IP and the edge throttle rejects it.
- Cloud Armor stops at the first matching rule: a catch-all throttle at priority 1000 silently disabled the WAF
  rules at 2000/2001. WAF deny rules now come first. Rule changes take a minute or two to propagate.
- k6 against a rate-limited tenant must back off on 429 and use think time, or it spins at 1,800 req/s of 429s
  (first run). The platform tenant is sized for the profile (3000 rpm); `noConnectionReuse` spreads load to new pods.
- The first TTFT SLO (2 s) burned its budget immediately: cold p95 on gemini-3.8-flash is 2.04 s. It is now 3 s.
