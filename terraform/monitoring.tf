# Managed Prometheus scrapes the gateway (PodMonitoring in k8s/base); metrics
# land in Cloud Monitoring as prometheus.googleapis.com/<name>/<kind>. Two
# request-based SLOs with multi-window burn-rate alerts, plus a dashboard.

resource "google_monitoring_notification_channel" "email" {
  display_name = "LLM gateway SLO alerts"
  type         = "email"
  labels = {
    email_address = var.alert_email
  }
  depends_on = [google_project_service.apis]
}

resource "google_monitoring_custom_service" "gw" {
  service_id   = "llm-gateway"
  display_name = "llm-gateway"
  depends_on   = [google_project_service.apis]
}

locals {
  prom_filter  = "resource.type=\"prometheus_target\" resource.labels.namespace=\"${var.app_namespace}\""
  requests_cnt = "metric.type=\"prometheus.googleapis.com/llm_requests_total/counter\" ${local.prom_filter}"
  # Good = everything except gateway/provider failures. 429s from quotas are
  # correct behaviour, not errors, and count as good.
  requests_good = "${local.requests_cnt} metric.labels.outcome!=monitoring.regex.full_match(\"upstream_error|internal_error\")"
  ttft_hist     = "metric.type=\"prometheus.googleapis.com/llm_ttft_seconds/histogram\" ${local.prom_filter}"
}

# 99.5% of chat requests succeed (no upstream/internal error), rolling 28 days.
resource "google_monitoring_slo" "availability" {
  service      = google_monitoring_custom_service.gw.service_id
  slo_id       = "chat-availability"
  display_name = "99.5% of chat completions without gateway/provider error"
  goal         = 0.995

  rolling_period_days = 28

  request_based_sli {
    good_total_ratio {
      good_service_filter  = local.requests_good
      total_service_filter = local.requests_cnt
    }
  }
}

# 95% of streamed responses deliver the first token within 2 s (cache hits included).
resource "google_monitoring_slo" "ttft" {
  service      = google_monitoring_custom_service.gw.service_id
  slo_id       = "ttft-p95-2s"
  display_name = "95% of streams: first token under 2s"
  goal         = 0.95

  rolling_period_days = 28

  request_based_sli {
    distribution_cut {
      distribution_filter = local.ttft_hist
      range {
        min = 0
        max = 2
      }
    }
  }
}

# Multi-window burn-rate alerts (SRE workbook thresholds for a 28-day budget).
resource "google_monitoring_alert_policy" "burn" {
  for_each = {
    availability = google_monitoring_slo.availability.name
    ttft         = google_monitoring_slo.ttft.name
  }
  display_name = "llm-gateway ${each.key} SLO burn rate"
  combiner     = "OR"

  conditions {
    display_name = "fast burn (1h > 14.4x)"
    condition_threshold {
      filter          = "select_slo_burn_rate(\"${each.value}\", \"3600s\")"
      comparison      = "COMPARISON_GT"
      threshold_value = 14.4
      duration        = "0s"
    }
  }

  conditions {
    display_name = "slow burn (6h > 6x)"
    condition_threshold {
      filter          = "select_slo_burn_rate(\"${each.value}\", \"21600s\")"
      comparison      = "COMPARISON_GT"
      threshold_value = 6
      duration        = "0s"
    }
  }

  notification_channels = [google_monitoring_notification_channel.email.id]

  documentation {
    content   = "The llm-gateway ${each.key} SLO is burning its error budget. Check llm_breaker_open and llm_upstream_requests_total by code (provider), llm_limiter_errors_total (Redis), HPA replicas vs llm_inflight_requests, and Cloud Trace for slow spans."
    mime_type = "text/markdown"
  }
}

locals {
  tile = { w = 4, h = 4 }
  panels = [
    { title = "TTFT p50 / p95 (s)", q = "histogram_quantile(0.95, sum by (le) (rate(llm_ttft_seconds_bucket[5m])))", q2 = "histogram_quantile(0.50, sum by (le) (rate(llm_ttft_seconds_bucket[5m])))", type = "LINE" },
    { title = "Requests/s by outcome", q = "sum by (outcome) (rate(llm_requests_total[5m]))", q2 = "", type = "STACKED_AREA" },
    { title = "Tokens/s by model", q = "sum by (model, kind) (rate(llm_tokens_total[5m]))", q2 = "", type = "STACKED_AREA" },
    { title = "Cache hit ratio", q = "sum(rate(llm_requests_total{outcome=\"cache_hit\"}[5m])) / sum(rate(llm_requests_total{outcome=~\"ok|fallback_ok|cache_hit\"}[5m]))", q2 = "", type = "LINE" },
    { title = "Estimated cost USD/hour by tenant", q = "sum by (tenant) (rate(llm_cost_usd_total[5m])) * 3600", q2 = "", type = "STACKED_AREA" },
    { title = "Rate limited/s by tenant and reason", q = "sum by (tenant, reason) (rate(llm_rate_limited_total[5m]))", q2 = "", type = "STACKED_AREA" },
    { title = "Upstream requests/s by model and code", q = "sum by (model, code) (rate(llm_upstream_requests_total[5m]))", q2 = "", type = "STACKED_AREA" },
    { title = "Breaker open (1) / fallbacks per s", q = "max by (model) (llm_breaker_open)", q2 = "sum(rate(llm_fallbacks_total[5m]))", type = "LINE" },
    { title = "In-flight requests (HPA signal) and replicas", q = "sum(llm_inflight_requests)", q2 = "count(up{namespace=\"${var.app_namespace}\"})", type = "LINE" },
    { title = "Gateway p95 latency by model (s)", q = "histogram_quantile(0.95, sum by (model, le) (rate(llm_request_duration_seconds_bucket[5m])))", q2 = "", type = "LINE" },
    { title = "Semantic cache best score (p50)", q = "histogram_quantile(0.5, sum by (le) (rate(llm_semantic_cache_score_bucket[5m])))", q2 = "", type = "LINE" },
    { title = "Limiter Redis errors/s (fail-open)", q = "sum by (op) (rate(llm_limiter_errors_total[5m]))", q2 = "", type = "LINE" },
  ]
}

resource "google_monitoring_dashboard" "gw" {
  dashboard_json = jsonencode({
    displayName = "llm-gateway"
    mosaicLayout = {
      columns = 12
      tiles = [
        for i, p in local.panels : {
          xPos   = (i % 3) * local.tile.w
          yPos   = floor(i / 3) * local.tile.h
          width  = local.tile.w
          height = local.tile.h
          widget = {
            title = p.title
            xyChart = {
              dataSets = concat(
                [{ timeSeriesQuery = { prometheusQuery = p.q }, plotType = p.type }],
                p.q2 == "" ? [] : [{ timeSeriesQuery = { prometheusQuery = p.q2 }, plotType = "LINE" }]
              )
            }
          }
        }
      ]
    }
  })
}
