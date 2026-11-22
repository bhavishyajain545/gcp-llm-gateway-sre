output "cluster_name" {
  value = google_container_cluster.gw.name
}

output "get_credentials" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.gw.name} --zone ${var.zone} --project ${var.project_id}"
}

output "artifact_registry" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.apps.repository_id}"
}

output "redis_host" {
  value = "${google_redis_instance.cache.host}:${google_redis_instance.cache.port}"
}

output "security_policy" {
  value = google_compute_security_policy.edge.name
}

output "app_service_account" {
  value = google_service_account.app.email
}

output "workload_identity_provider" {
  description = "Set as GitHub Actions secret GCP_WORKLOAD_IDENTITY_PROVIDER."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "tenant_keys" {
  description = "API keys per tenant (also in Secret Manager llm-gateway-tenants)."
  value       = { for name in keys(local.tenants) : name => "sk-${name}-${random_password.tenant_key[name].result}" }
  sensitive   = true
}

output "admin_key" {
  value     = random_password.admin_key.result
  sensitive = true
}

output "dashboard_url" {
  value = "https://console.cloud.google.com/monitoring/dashboards/builder/${element(split("/", google_monitoring_dashboard.gw.id), length(split("/", google_monitoring_dashboard.gw.id)) - 1)}?project=${var.project_id}"
}

data "google_project" "this" {}

output "slo_url" {
  value = "https://console.cloud.google.com/monitoring/services/${data.google_project.this.number}/${google_monitoring_custom_service.gw.service_id}?project=${var.project_id}"
}

output "trace_url" {
  value = "https://console.cloud.google.com/traces/list?project=${var.project_id}"
}
