variable "name" {
  description = "Function name. Also names the IAM role and the log group."
  type        = string
}

variable "zip_path" {
  description = "Path to the deployment zip (bootstrap binary for provided.al2023, arm64)."
  type        = string
}

variable "environment" {
  description = "Environment variables for the function."
  type        = map(string)
  default     = {}
}

variable "policy_json" {
  description = "IAM policy document (JSON) attached to the function role, beyond the log access the module always grants. Required, not optional: the attachment must not depend on a value only known after apply."
  type        = string
}

variable "api_execution_arn" {
  description = "Execution ARN of the API (REST or HTTP) allowed to invoke the function. The module creates the invoke permission only, never an API."
  type        = string
}

variable "memory_size" {
  type    = number
  default = 256
}

variable "timeout" {
  description = "Function timeout in seconds."
  type        = number
  default     = 29
}

variable "log_retention_days" {
  type    = number
  default = 14
}
