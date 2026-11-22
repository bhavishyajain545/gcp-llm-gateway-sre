# Custom Metrics Stackdriver Adapter

The HPA in `k8s/base/hpa.yaml` scales on `llm_inflight_requests`, a Prometheus
gauge scraped by Google Managed Prometheus. Kubernetes reads it through the
custom metrics API, served by Google's adapter:

```bash
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/k8s-stackdriver/master/custom-metrics-stackdriver-adapter/deploy/production/adapter_new_resource_model.yaml
kubectl -n custom-metrics annotate serviceaccount custom-metrics-stackdriver-adapter \
  iam.gke.io/gcp-service-account=custom-metrics-adapter@<project>.iam.gserviceaccount.com --overwrite
kubectl -n custom-metrics rollout restart deployment custom-metrics-stackdriver-adapter
```

`deploy.sh` runs exactly this. The adapter authenticates with Workload Identity
(`terraform/iam.tf`: GSA `custom-metrics-adapter`, `roles/monitoring.viewer`).

Check that the metric is visible to the HPA:

```bash
kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta2/namespaces/llm-gateway/pods/*/prometheus.googleapis.com|llm_inflight_requests|gauge" | jq
kubectl -n llm-gateway describe hpa llm-gateway
```
