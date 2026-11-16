variable "project_id" { type = string }
variable "region" {
  type    = string
  default = "us-central1"
}
variable "zone" {
  type    = string
  default = "us-central1-a"
  validation {
    condition     = startswith(var.zone, "${var.region}-")
    error_message = "The zone must be within the selected region."
  }
}
variable "name" {
  type    = string
  default = "llm-lab"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,19}$", var.name))
    error_message = "Use 3-20 lowercase letters, digits, or hyphens, starting with a letter."
  }
}
variable "model_bucket_name" { type = string }
variable "machine_type" {
  type    = string
  default = "e2-standard-4"
}
variable "node_count" {
  type    = number
  default = 1
  validation {
    condition     = var.node_count >= 1 && var.node_count <= 3 && floor(var.node_count) == var.node_count
    error_message = "This test cluster supports 1-3 fixed CPU nodes."
  }
}
variable "authorized_cidrs" {
  description = "Public client/VPN CIDRs allowed to reach the Kubernetes control plane."
  type        = set(string)
  validation {
    condition     = length(var.authorized_cidrs) > 0 && alltrue([for cidr in var.authorized_cidrs : can(cidrnetmask(cidr)) && cidr != "0.0.0.0/0"])
    error_message = "Provide at least one restricted IPv4 CIDR; 0.0.0.0/0 is not allowed."
  }
}
variable "labels" {
  type    = map(string)
  default = { purpose = "llm-test" }
}
variable "enable_gpu" {
  type    = bool
  default = false
}
variable "gpu_type" {
  type    = string
  default = "nvidia-tesla-t4"
}
variable "gpu_machine_type" {
  type    = string
  default = "n1-standard-4"
}
variable "deletion_protection" {
  description = "Set false and apply before destroying a protected cluster."
  type        = bool
  default     = false
}
