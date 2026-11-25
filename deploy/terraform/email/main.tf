# Vertical slice of the moose email deployment: one HTTP API in front of two
# Lambdas (moose-email and moose-approver), their tables and keys, the Route53
# records, the GitHub deploy role and a budget. Rules for this root:
#   - DNS is aws_route53_record only; the hosted zone and state bucket are never created.
#   - The GitHub OIDC provider is looked up, never created.
#   - No function sets reserved concurrency (the account quota is shared).
data "aws_caller_identity" "current" {}

locals {
  email_name    = "moose-email-${var.env}"
  approver_name = "moose-approver-${var.env}"
  origin        = "https://${var.domain_name}"
}
