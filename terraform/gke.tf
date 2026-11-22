# GKE Standard (not Autopilot, which project 2 used): we own the node pools, so
# we can mix an on-demand "system" pool with a Spot "gateway" pool, run a
# DaemonSet-free custom-metrics adapter, and let the cluster autoscaler add Spot
# nodes when the HPA scales the gateway out. Zonal: no cluster management fee
# under the GKE free tier.
resource "google_container_cluster" "gw" {
  name     = var.cluster_name
  location = var.zone

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false

  network         = google_compute_network.vpc.id
  subnetwork      = google_compute_subnetwork.nodes.id
  networking_mode = "VPC_NATIVE"

  # Dataplane V2 (eBPF): NetworkPolicy enforcement and flow logs without Calico.
  datapath_provider = "ADVANCED_DATAPATH"

  release_channel {
    channel = "REGULAR"
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
  }

  master_authorized_networks_config {
    cidr_blocks {
      cidr_block   = var.authorized_cidr
      display_name = "operator"
    }
    gcp_public_cidrs_access_enabled = false
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  gateway_api_config {
    channel = "CHANNEL_STANDARD"
  }

  monitoring_config {
    enable_components = ["SYSTEM_COMPONENTS"]
    managed_prometheus {
      enabled = true
    }
  }

  logging_config {
    enable_components = ["SYSTEM_COMPONENTS", "WORKLOADS"]
  }

  addons_config {
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
  }

  depends_on = [google_project_service.apis, google_compute_router_nat.nat]
}

# Dedicated node identity with the minimum roles (org policy disables the
# automatic grants on the default compute SA anyway).
resource "google_service_account" "nodes" {
  account_id   = "gke-nodes"
  display_name = "GKE node service account"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
    "roles/autoscaling.metricsWriter",
  ])
  project = var.project_id
  role    = each.value
  member  = google_service_account.nodes.member
}

resource "google_artifact_registry_repository_iam_member" "nodes_pull" {
  location   = google_artifact_registry_repository.apps.location
  repository = google_artifact_registry_repository.apps.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.nodes.member
}

# On-demand pool for cluster add-ons and the custom-metrics adapter.
resource "google_container_node_pool" "system" {
  name     = "system"
  cluster  = google_container_cluster.gw.name
  location = var.zone

  autoscaling {
    min_node_count = 1
    max_node_count = 2
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = var.system_machine_type
    disk_size_gb    = 50
    disk_type       = "pd-balanced"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { pool = "system" }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
}

# Spot pool for the gateway itself: stateless, fronted by a global LB, so
# preemption is survivable (PDB + 2 replicas) and ~60-90% cheaper.
resource "google_container_node_pool" "gateway" {
  name     = "gateway-spot"
  cluster  = google_container_cluster.gw.name
  location = var.zone

  autoscaling {
    min_node_count = var.gateway_min_nodes
    max_node_count = var.gateway_max_nodes
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = var.gateway_machine_type
    spot            = true
    disk_size_gb    = 50
    disk_type       = "pd-balanced"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { pool = "gateway" }

    taint {
      key    = "pool"
      value  = "gateway"
      effect = "NO_SCHEDULE"
    }
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
}
