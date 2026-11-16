mock_provider "google" {}
variables {
  project_id        = "example-project"
  state_bucket_name = "example-state-bucket"
  state_operator    = "user:operator@example.com"
}
run "protected_state" {
  command = plan
  assert {
    condition     = google_storage_bucket.state.versioning[0].enabled && google_storage_bucket.state.public_access_prevention == "enforced" && !google_storage_bucket.state.force_destroy
    error_message = "State must be private, versioned and protected from forced deletion."
  }
}
