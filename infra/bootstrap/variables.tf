variable "project_id" { type = string }
variable "region" {
  type    = string
  default = "us-central1"
}
variable "state_bucket_name" { type = string }
variable "state_operator" {
  description = "IAM member permitted to read/write Terraform state, e.g. user:you@example.com."
  type        = string
  validation {
    condition     = can(regex("^(user|serviceAccount|group):[^ ]+$", var.state_operator))
    error_message = "Use an explicit user, group, or serviceAccount IAM member."
  }
}
