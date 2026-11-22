variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  description = "Zonal cluster: covered by the GKE free tier (no cluster management fee) and enough for a demo."
  type        = string
  default     = "us-central1-a"
}

variable "cluster_name" {
  type    = string
  default = "llm-gateway"
}

variable "authorized_cidr" {
  description = "CIDR allowed to reach the GKE control plane (your public IP/32, or your ISP's block behind CGNAT)."
  type        = string
}

variable "github_repo" {
  description = "GitHub repository (owner/name) allowed to use Workload Identity Federation for CI."
  type        = string
  default     = "bhavishyajain545/gcp-llm-gateway-sre"
}

variable "alert_email" {
  description = "Email for SLO burn-rate alerts."
  type        = string
}

variable "app_namespace" {
  type    = string
  default = "llm-gateway"
}

variable "app_ksa" {
  type    = string
  default = "llm-gateway"
}

variable "system_machine_type" {
  type    = string
  default = "e2-standard-2"
}

variable "gateway_machine_type" {
  type    = string
  default = "e2-standard-2"
}

variable "gateway_min_nodes" {
  type    = number
  default = 1
}

variable "gateway_max_nodes" {
  type    = number
  default = 3
}

variable "redis_memory_gb" {
  type    = number
  default = 1
}

variable "armor_rpm_per_ip" {
  description = "Cloud Armor per-client-IP throttle (requests per minute) at the load balancer edge."
  type        = number
  default     = 120
}

variable "gemini_location" {
  type    = string
  default = "global"
}

variable "models" {
  description = "Models the gateway serves, in the order shown by /v1/models."
  type        = list(string)
  default     = ["gemini-3.8-flash", "gemini-3.5-flash-lite"]
}
