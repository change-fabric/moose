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
  description = "Public hostname for /mcp and /approve (for example moose.changefabric.org). A second tenant sets its own."
  type        = string
}

variable "route53_zone_id" {
  description = "Existing Route53 hosted zone id for domain_name. The zone is never managed here; only aws_route53_record resources are."
  type        = string
}
