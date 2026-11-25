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
  description = "Execution ARN of the API (REST or HTTP) allowed to invoke the function. The module creates the invoke permission only, never an API. Ignored when allow_api is false."
  type        = string
  default     = ""
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

variable "allow_api" {
  description = "Create the API Gateway invoke permission. A separate flag, not a test on api_execution_arn, because that ARN is unknown until the API exists and a count cannot depend on it."
  type        = bool
  default     = true
}

variable "dead_letter_enabled" {
  description = "Attach dead_letter_target_arn as the function's dead-letter queue. A flag for the same reason as allow_api."
  type        = bool
  default     = false
}

variable "dead_letter_target_arn" {
  description = "SQS queue or SNS topic ARN that receives async invocations that fail after their retries. The function role needs permission to send to it."
  type        = string
  default     = ""
}
