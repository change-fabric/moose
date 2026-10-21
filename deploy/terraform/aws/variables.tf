variable "env" {
  description = "Deployment environment name, used in resource names (moose-<env>)."
  type        = string
  default     = "dev"

  validation {
    condition     = can(regex("^[a-z0-9-]{1,16}$", var.env))
    error_message = "env must be 1-16 lowercase letters, digits, or dashes."
  }
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "tailnet" {
  description = "Tailnet name (e.g. example.ts.net), or \"-\" for the credential's default tailnet."
  type        = string
  default     = "-"
}

variable "relay_hostname" {
  description = "Tailnet hostname of the relay. Use moose-aws if a Mac Mini already owns moose in the same tailnet."
  type        = string
  default     = "moose"
}

variable "vpc_cidr" {
  description = "VPC CIDR, advertised by the relay as a subnet route."
  type        = string
  default     = "10.42.0.0/24"
}

variable "bootstrap_admin" {
  description = "Tailnet login made admin on first start when the users table is empty."
  type        = string
}

variable "lambda_zip" {
  description = "Path to the Lambda zip built by `make lambda`."
  type        = string
  default     = "../../../dist/moose-lambda.zip"
}

variable "relay_proxy_binary" {
  description = "Path to the relay proxy binary built by `make relay-proxy`."
  type        = string
  default     = "../../../dist/moose-relay-proxy"
}

variable "relay_proxy_image" {
  description = "Container image the relay proxy binary runs in (static binary; needs CA certificates only)."
  type        = string
  default     = "gcr.io/distroless/static-debian12:latest"
}

variable "lambda_reserved_concurrency" {
  description = "Reserved concurrency for the Lambda (cost guard)."
  type        = number
  default     = 5
}

variable "deletion_protection" {
  description = "Enable DynamoDB deletion protection."
  type        = bool
  default     = true
}

variable "log_retention_days" {
  description = "CloudWatch log retention for the Lambda."
  type        = number
  default     = 14
}
