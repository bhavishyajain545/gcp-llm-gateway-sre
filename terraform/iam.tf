# ---------------------------------------------------------------------------
# Gateway identity: KSA llm-gateway/llm-gateway -> GSA llm-gateway (Workload
# Identity). The pod calls Vertex AI, Secret Manager and Cloud Trace with no key.
# ---------------------------------------------------------------------------
resource "google_service_account" "app" {
  account_id   = "llm-gateway"
  display_name = "llm-gateway (GKE workload identity)"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "app_roles" {
  for_each = toset([
    "roles/aiplatform.user",   # generateContent / embeddings
    "roles/cloudtrace.agent",  # OpenTelemetry spans
    "roles/logging.logWriter", # structured logs
  ])
  project = var.project_id
  role    = each.value
  member  = google_service_account.app.member
}

resource "google_service_account_iam_member" "app_workload_identity" {
  service_account_id = google_service_account.app.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.app_namespace}/${var.app_ksa}]"
  depends_on         = [google_container_cluster.gw]
}

# ---------------------------------------------------------------------------
# Custom Metrics Stackdriver Adapter: lets the HPA read llm_inflight_requests
# (scraped by Managed Prometheus) through the external/custom metrics API.
# ---------------------------------------------------------------------------
resource "google_service_account" "metrics_adapter" {
  account_id   = "custom-metrics-adapter"
  display_name = "custom-metrics-stackdriver-adapter"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "metrics_adapter_viewer" {
  project = var.project_id
  role    = "roles/monitoring.viewer"
  member  = google_service_account.metrics_adapter.member
}

resource "google_service_account_iam_member" "metrics_adapter_wi" {
  service_account_id = google_service_account.metrics_adapter.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[custom-metrics/custom-metrics-stackdriver-adapter]"
  depends_on         = [google_container_cluster.gw]
}

# ---------------------------------------------------------------------------
# CI identity for GitHub Actions via Workload Identity Federation (keyless).
# ---------------------------------------------------------------------------
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  depends_on                = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-oidc"
  display_name                       = "GitHub OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
  }
  attribute_condition = "assertion.repository == \"${var.github_repo}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account" "ci" {
  account_id   = "sa-github-ci"
  display_name = "GitHub Actions CI (push images)"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "ci_roles" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = google_service_account.ci.member
}

resource "google_service_account_iam_member" "ci_wif" {
  service_account_id = google_service_account.ci.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repo}"
}

# ---------------------------------------------------------------------------
# Bootstrap build identity for `gcloud builds submit` (first image before CI exists).
# ---------------------------------------------------------------------------
resource "google_service_account" "build" {
  account_id   = "sa-build"
  display_name = "Cloud Build (bootstrap image build)"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "build_roles" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/artifactregistry.writer",
    "roles/storage.objectViewer",
  ])
  project = var.project_id
  role    = each.value
  member  = google_service_account.build.member
}
