mock_provider "google" {}
variables {
  project_id        = "example-project"
  model_bucket_name = "example-model-bucket"
  authorized_cidrs  = ["192.0.2.1/32"]
}
run "cpu_defaults" {
  command = plan
  assert {
    condition     = google_container_cluster.lab.private_cluster_config[0].enable_private_nodes
    error_message = "Cluster nodes must be private."
  }
  assert {
    condition     = length(google_container_node_pool.gpu) == 0 && google_container_node_pool.cpu.node_count == 1
    error_message = "Default must be one CPU node without a GPU pool."
  }
  assert {
    condition     = google_storage_bucket.models.force_destroy == false
    error_message = "Model storage must not be silently emptied on destroy."
  }
}
run "optional_gpu" {
  command = plan
  variables { enable_gpu = true }
  assert {
    condition     = length(google_container_node_pool.gpu) == 1 && google_container_node_pool.gpu[0].node_count == 1
    error_message = "GPU mode must provision one bounded GPU node."
  }
}
run "reject_open_control_plane" {
  command = plan
  variables { authorized_cidrs = ["0.0.0.0/0"] }
  expect_failures = [var.authorized_cidrs]
}
