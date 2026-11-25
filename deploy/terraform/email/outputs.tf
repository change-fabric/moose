output "account_id" {
  description = "AWS account the root is applied into."
  value       = data.aws_caller_identity.current.account_id
}

output "mcp_url" {
  description = "Connector URL for Claude.ai and Claude Code."
  value       = "${local.origin}/email/mcp"
}

output "google_redirect_uri" {
  description = "Authorized redirect URI to register on the tenant's Google OAuth client."
  value       = "${local.origin}/connect/google/callback"
}

output "connect_url" {
  description = "Start here to sign in and connect Google accounts."
  value       = "${local.origin}/connect/login"
}

output "google_oauth_secret_arn" {
  description = "Secrets Manager secret that holds the Google OAuth client as JSON {client_id, client_secret}. Terraform never writes its value."
  value       = aws_secretsmanager_secret.google_oauth.arn
}

output "google_oauth_secret_put" {
  description = "Command that stores the Google OAuth client without it passing through Terraform. Fill in the two values."
  value       = "aws secretsmanager put-secret-value --profile ${var.aws_profile} --region ${var.region} --secret-id ${aws_secretsmanager_secret.google_oauth.arn} --secret-string '{\"client_id\":\"<id>\",\"client_secret\":\"<secret>\"}'"
}

output "email_function_name" {
  value = module.email.function_name
}

output "approver_function_name" {
  value = module.approver.function_name
}

output "deploy_role_arn" {
  description = "Role GitHub Actions assumes to update function code."
  value       = aws_iam_role.deploy.arn
}

output "receipt_public_key_pem" {
  description = "Public half of the receipt signing key, pinned into moose-email."
  value       = data.aws_kms_public_key.receipt.public_key_pem
}

# Environment for `moose-approver enroll`, which runs from your machine (see
# scripts/approver-enroll.sh) and prints the one-time passkey enrollment link.
output "approver_env" {
  description = "Environment moose-approver needs outside Lambda, for the enroll command."
  value = {
    MOOSE_ENV                      = "prod"
    MOOSE_DYNAMO_TABLE             = aws_dynamodb_table.approver.name
    MOOSE_APPROVER_RP_ID           = var.domain_name
    MOOSE_APPROVER_ORIGIN          = local.origin
    MOOSE_APPROVER_OWNER           = var.owner
    MOOSE_APPROVER_SIGNING_KEY_ID  = aws_kms_key.receipt.arn
    MOOSE_APPROVER_KMS_KEY_ID      = aws_kms_key.preview.arn
    MOOSE_APPROVER_HMAC_SECRET_ARN = aws_secretsmanager_secret.service_hmac.arn
  }
}

output "attachments_bucket" {
  description = "Private bucket email_get_attachment streams into (objects live about an hour)."
  value       = aws_s3_bucket.attachments.bucket
}

output "sweeper_function_name" {
  value = module.sweeper.function_name
}
