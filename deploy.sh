#!/usr/bin/env bash
# End-to-end bring-up: Terraform -> first image -> metrics adapter -> manifests.
#   ./deploy.sh            # full bring-up
#   ./deploy.sh rollout    # only re-apply k8s/base (after CI bumped the image tag)
#   ./deploy.sh slo        # second terraform apply once metrics exist (SLOs, alerts)
set -euo pipefail
cd "$(dirname "$0")"
export USE_GKE_GCLOUD_AUTH_PLUGIN=True

PROJECT_ID="$(grep -E '^project_id' terraform/terraform.tfvars | cut -d'"' -f2)"
REGION="$(grep -E '^region' terraform/terraform.tfvars | cut -d'"' -f2)"
ZONE="$(grep -E '^zone' terraform/terraform.tfvars | cut -d'"' -f2)"
TAG="${TAG:-v1}"
MODE="${1:-full}"
ADAPTER_URL="https://raw.githubusercontent.com/GoogleCloudPlatform/k8s-stackdriver/master/custom-metrics-stackdriver-adapter/deploy/production/adapter_new_resource_model.yaml"

kubeconfig() {
  gcloud container clusters get-credentials "$(terraform -chdir=terraform output -raw cluster_name)" --zone "$ZONE" --project "$PROJECT_ID" >/dev/null
}

gateway_ip() {
  for _ in $(seq 1 60); do
    IP="$(kubectl -n llm-gateway get gateway external -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
    [[ -n "$IP" ]] && break; sleep 10
  done
  echo "${IP:-<pending>}"
}

case "$MODE" in
  rollout)
    kubeconfig
    kubectl apply -k k8s/base
    kubectl -n llm-gateway rollout status deployment/llm-gateway --timeout=10m
    exit 0 ;;
  slo)
    terraform -chdir=terraform apply -input=false -auto-approve
    exit 0 ;;
esac

echo "== 1/6 terraform apply (the two SLOs fail until the gateway has served traffic: expected, see ./deploy.sh slo)"
terraform -chdir=terraform init -input=false >/dev/null
terraform -chdir=terraform apply -input=false -auto-approve || echo "   (continuing: re-run './deploy.sh slo' after step 6 has generated traffic)"

echo "== 2/6 build + push llm-gateway:${TAG} (Cloud Build, sa-build)"
gcloud builds submit --project "$PROJECT_ID" --config cloudbuild.yaml --substitutions="_TAG=${TAG},_REGION=${REGION}" . --quiet

echo "== 3/6 kubeconfig"
kubeconfig

echo "== 4/6 custom-metrics-stackdriver-adapter (HPA on Prometheus metrics)"
kubectl apply -f "$ADAPTER_URL" >/dev/null
kubectl -n custom-metrics annotate serviceaccount custom-metrics-stackdriver-adapter \
  "iam.gke.io/gcp-service-account=custom-metrics-adapter@${PROJECT_ID}.iam.gserviceaccount.com" --overwrite >/dev/null
kubectl -n custom-metrics rollout restart deployment custom-metrics-stackdriver-adapter >/dev/null
kubectl -n custom-metrics rollout status deployment custom-metrics-stackdriver-adapter --timeout=5m

echo "== 5/6 kubectl apply -k k8s/base"
kubectl apply -k k8s/base
kubectl -n llm-gateway rollout status deployment/llm-gateway --timeout=10m

echo "== 6/6 gateway address"
IP="$(gateway_ip)"
echo
echo "Gateway:   http://${IP}/v1/chat/completions   (x-api-key or Authorization: Bearer <tenant key>)"
echo "Keys:      terraform -chdir=terraform output -json tenant_keys ; admin: terraform -chdir=terraform output -raw admin_key"
echo "Dashboard: $(terraform -chdir=terraform output -raw dashboard_url)"
echo "Next:      loadtest/run.sh ; then ./deploy.sh slo  (creates the SLOs + burn-rate alerts once metrics exist)"
