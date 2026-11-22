# Cloud Armor at the load balancer edge, attached to the Service through a
# GCPBackendPolicy (k8s/base/backendpolicy.yaml). Tenant quotas live in the
# gateway; this layer stops abusive clients and injection attempts before they
# consume a pod.
resource "google_compute_security_policy" "edge" {
  name        = "${var.cluster_name}-edge"
  description = "Per-IP throttle + OWASP SQLi/XSS in front of the LLM gateway"
  depends_on  = [google_project_service.apis]

  rule {
    priority    = 1000
    action      = "throttle"
    description = "Per client IP: ${var.armor_rpm_per_ip} requests/minute, then 429"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"
      rate_limit_threshold {
        count        = var.armor_rpm_per_ip
        interval_sec = 60
      }
    }
  }

  rule {
    priority    = 2000
    action      = "deny(403)"
    description = "Preconfigured WAF: SQL injection"
    match {
      expr {
        expression = "evaluatePreconfiguredWaf('sqli-v33-stable', {'sensitivity': 1})"
      }
    }
  }

  rule {
    priority    = 2001
    action      = "deny(403)"
    description = "Preconfigured WAF: cross-site scripting"
    match {
      expr {
        expression = "evaluatePreconfiguredWaf('xss-v33-stable', {'sensitivity': 1})"
      }
    }
  }

  rule {
    priority    = 2147483647
    action      = "allow"
    description = "default allow"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
  }
}
