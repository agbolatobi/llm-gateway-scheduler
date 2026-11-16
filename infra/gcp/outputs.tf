output "cluster_name" { value = google_container_cluster.lab.name }
output "connect_command" { value = "gcloud container clusters get-credentials ${google_container_cluster.lab.name} --zone ${var.zone} --project ${var.project_id}" }
output "image_repository" { value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}" }
output "model_bucket" { value = google_storage_bucket.models.name }
output "model_service_account" { value = google_service_account.models.email }
