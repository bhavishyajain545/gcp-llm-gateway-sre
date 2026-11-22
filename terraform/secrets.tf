# Tenant API keys, the admin key and the Redis connection live in Secret
# Manager. The gateway reads them over its Workload Identity at start-up and
# re-reads the tenant list every minute (key rotation without a restart).
# Nothing here is in a Kubernetes Secret, a ConfigMap or the image.

resource "random_password" "tenant_key" {
  for_each = local.tenants
  length   = 40
  special  = false
}

resource "random_password" "admin_key" {
  length  = 40
  special = false
}

locals {
  # name => quotas. Models empty = every configured model.
  tenants = {
    "team-platform" = { rpm = 600, burst = 100, daily_tokens = 2000000, models = [] }
    "team-support"  = { rpm = 60, burst = 10, daily_tokens = 50000, models = ["gemini-3.5-flash-lite"] }
    "team-burst"    = { rpm = 30, burst = 10, daily_tokens = 20000, models = [] }
  }
  tenants_json = jsonencode([
    for name, t in local.tenants : {
      name         = name
      key          = "sk-${name}-${random_password.tenant_key[name].result}"
      rpm          = t.rpm
      burst        = t.burst
      daily_tokens = t.daily_tokens
      models       = t.models
    }
  ])
}

resource "google_secret_manager_secret" "tenants" {
  secret_id = "llm-gateway-tenants"
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_version" "tenants" {
  secret      = google_secret_manager_secret.tenants.id
  secret_data = local.tenants_json
}

resource "google_secret_manager_secret" "admin" {
  secret_id = "llm-gateway-admin-key"
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_version" "admin" {
  secret      = google_secret_manager_secret.admin.id
  secret_data = random_password.admin_key.result
}

resource "google_secret_manager_secret" "redis" {
  secret_id = "llm-gateway-redis"
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_version" "redis" {
  secret = google_secret_manager_secret.redis.id
  secret_data = jsonencode({
    addr     = "${google_redis_instance.cache.host}:${google_redis_instance.cache.port}"
    password = google_redis_instance.cache.auth_string
  })
}

resource "google_secret_manager_secret_iam_member" "app_access" {
  for_each  = { tenants = google_secret_manager_secret.tenants.id, admin = google_secret_manager_secret.admin.id, redis = google_secret_manager_secret.redis.id }
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.app.member
}
