variable "aws_profile" {
  description = "AWS CLI profile. Defaults to the change-fabric account (554340864373)."
  type        = string
  default     = "change-fabric"
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "env" {
  description = "Deployment environment name, used in resource names (moose-email-<env>)."
  type        = string
  default     = "dev"

  validation {
    condition     = can(regex("^[a-z0-9-]{1,16}$", var.env))
    error_message = "env must be 1-16 lowercase letters, digits, or dashes."
  }
}

variable "domain_name" {
  description = "Public hostname for /email/mcp and /approve (for example moose.changefabric.org). A second tenant sets its own."
  type        = string
}

variable "route53_zone_id" {
  description = "Existing Route53 hosted zone id for domain_name. The zone is never managed here; only aws_route53_record resources are."
  type        = string
}

variable "owner" {
  description = "Tenant owner id. moose-email issues tokens to it and moose-approver accepts approvals only from its passkeys. One owner per deployment."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,31}$", var.owner))
    error_message = "owner must be 1-32 lowercase letters, digits, or dashes."
  }
}

variable "email_zip_path" {
  description = "moose-email deployment zip. Build it with `make lambda-email`."
  type        = string
  default     = "../../../dist/moose-email.zip"
}

variable "approver_zip_path" {
  description = "moose-approver deployment zip. Build it with `make lambda-approver`."
  type        = string
  default     = "../../../dist/moose-approver.zip"
}

variable "sweeper_zip_path" {
  description = "moose-attach-sweeper deployment zip. Build it with `make lambda-sweeper`."
  type        = string
  default     = "../../../dist/moose-attach-sweeper.zip"
}

variable "alarm_emails" {
  description = "Addresses subscribed to the moose alerts topic (function, API, table and attachment sweeper alarms). Null means the same addresses as budget_emails. Each must confirm the SNS subscription once."
  type        = list(string)
  default     = null
}

variable "github_repository_id" {
  description = "Immutable numeric id of the GitHub repository whose Actions may assume the deploy role (gh api repos/<owner>/<repo> --jq .id). The id survives renames; the name does not."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.github_repository_id))
    error_message = "github_repository_id must be the numeric repository id."
  }
}

variable "budget_limit_usd" {
  description = "Monthly cost budget for resources tagged Project=moose, in USD."
  type        = number
  default     = 25
}

variable "budget_emails" {
  description = "Addresses notified when the budget reaches 80 percent (actual) or 100 percent (forecast)."
  type        = list(string)

  validation {
    condition     = length(var.budget_emails) > 0
    error_message = "budget_emails needs at least one address."
  }
}

variable "deletion_protection" {
  description = "Protect the DynamoDB tables from deletion. Turn off only to tear a tenant down."
  type        = bool
  default     = true
}

variable "api_throttle_rate" {
  description = "Steady-state requests per second the API accepts. The Lambda concurrency quota is shared across the account, so this also bounds how much of it moose can use."
  type        = number
  default     = 20
}

variable "api_throttle_burst" {
  description = "Burst requests the API accepts."
  type        = number
  default     = 40
}
