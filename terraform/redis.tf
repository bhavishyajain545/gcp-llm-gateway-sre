# Memorystore for Redis: shared state for every gateway replica (exact +
# semantic cache, token buckets, daily budgets). BASIC tier is a single node:
# fine here because the gateway fails open on Redis errors (limits off, cache
# miss) rather than failing requests. Use STANDARD_HA for a real deployment.
resource "google_redis_instance" "cache" {
  name           = "${var.cluster_name}-cache"
  tier           = "BASIC"
  memory_size_gb = var.redis_memory_gb
  region         = var.region
  location_id    = var.zone

  authorized_network = google_compute_network.vpc.id
  connect_mode       = "PRIVATE_SERVICE_ACCESS"
  redis_version      = "REDIS_7_2"
  auth_enabled       = true

  redis_configs = {
    maxmemory-policy = "allkeys-lru" # the cache may be evicted; limiter keys are tiny and refreshed constantly
  }

  depends_on = [google_service_networking_connection.psa]
}
