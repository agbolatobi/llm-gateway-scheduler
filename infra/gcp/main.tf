locals {
  services = toset(["compute.googleapis.com", "container.googleapis.com", "artifactregistry.googleapis.com", "storage.googleapis.com", "iam.googleapis.com", "iamcredentials.googleapis.com", "logging.googleapis.com", "monitoring.googleapis.com"])
}
resource "google_project_service" "required" {
  for_each           = local.services
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}
resource "google_compute_network" "lab" {
  name                    = var.name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.required]
}
resource "google_compute_subnetwork" "lab" {
  name                     = var.name
  region                   = var.region
  network                  = google_compute_network.lab.id
  ip_cidr_range            = "10.20.0.0/20"
  private_ip_google_access = true
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.24.0.0/14"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.28.0.0/20"
  }
}
# Private nodes need outbound access to fetch images and model artifacts.
resource "google_compute_router" "lab" {
  name    = var.name
  region  = var.region
  network = google_compute_network.lab.id
}
resource "google_compute_router_nat" "lab" {
  name                               = var.name
  router                             = google_compute_router.lab.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"
  subnetwork {
    name                    = google_compute_subnetwork.lab.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }
}
resource "google_service_account" "nodes" {
  account_id   = "${var.name}-nodes"
  display_name = "LLM lab nodes"
  depends_on   = [google_project_service.required]
}
resource "google_project_iam_member" "node_service" {
  project = var.project_id
  role    = "roles/container.defaultNodeServiceAccount"
  member  = "serviceAccount:${google_service_account.nodes.email}"
}
resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = var.name
  format        = "DOCKER"
  labels        = var.labels
  depends_on    = [google_project_service.required]
}
resource "google_artifact_registry_repository_iam_member" "pull" {
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.nodes.email}"
}
resource "google_container_cluster" "lab" {
  name                     = var.name
  location                 = var.zone
  network                  = google_compute_network.lab.id
  subnetwork               = google_compute_subnetwork.lab.id
  deletion_protection      = var.deletion_protection
  remove_default_node_pool = true
  initial_node_count       = 1
  networking_mode          = "VPC_NATIVE"
  datapath_provider        = "ADVANCED_DATAPATH"
  enable_shielded_nodes    = true
  resource_labels          = var.labels
  release_channel { channel = "REGULAR" }
  workload_identity_config { workload_pool = "${var.project_id}.svc.id.goog" }
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.0/28"
  }
  master_authorized_networks_config {
    dynamic "cidr_blocks" {
      for_each = var.authorized_cidrs
      content {
        cidr_block   = cidr_blocks.value
        display_name = "operator"
      }
    }
  }
  # Even the temporary default pool uses the dedicated node identity.
  node_config {
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
  }
  depends_on = [google_project_iam_member.node_service, google_compute_router_nat.lab]
}
resource "google_container_node_pool" "cpu" {
  name       = "cpu"
  cluster    = google_container_cluster.lab.name
  location   = var.zone
  node_count = var.node_count
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.machine_type
    disk_size_gb    = 50
    disk_type       = "pd-balanced"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { workload = "cpu" }
    metadata        = { disable-legacy-endpoints = "true" }
    workload_metadata_config { mode = "GKE_METADATA" }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  depends_on = [google_artifact_registry_repository_iam_member.pull]
}
# Optional, fixed single GPU node. Availability and GPU quota are project/zone-specific.
resource "google_container_node_pool" "gpu" {
  count      = var.enable_gpu ? 1 : 0
  name       = "gpu"
  cluster    = google_container_cluster.lab.name
  location   = var.zone
  node_count = 1
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.gpu_machine_type
    disk_size_gb    = 50
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { workload = "gpu" }
    workload_metadata_config { mode = "GKE_METADATA" }
    guest_accelerator {
      type  = var.gpu_type
      count = 1
      gpu_driver_installation_config { gpu_driver_version = "DEFAULT" }
    }
    taint {
      key    = "nvidia.com/gpu"
      value  = "present"
      effect = "NO_SCHEDULE"
    }
  }
  depends_on = [google_artifact_registry_repository_iam_member.pull]
}
resource "google_storage_bucket" "models" {
  name                        = var.model_bucket_name
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = var.labels
  depends_on                  = [google_project_service.required]
}
resource "google_service_account" "models" {
  account_id   = "${var.name}-models"
  display_name = "Read model weights"
  depends_on   = [google_project_service.required]
}
resource "google_storage_bucket_iam_member" "model_reader" {
  bucket = google_storage_bucket.models.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.models.email}"
}
resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.models.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[llm-gateway/gateway]"
  depends_on         = [google_container_cluster.lab]
}
