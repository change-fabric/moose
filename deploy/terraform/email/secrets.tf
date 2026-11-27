# The tenant's Google OAuth client. Terraform creates only the container: put
# the value yourself so the client secret never enters state (see the
# google_oauth_secret_put output for the command).
resource "aws_secretsmanager_secret" "google_oauth" {
  name                    = "${local.email_name}/google-oauth"
  description             = "Google OAuth client for moose-email: JSON {client_id, client_secret}"
  recovery_window_in_days = 7
}

# The tenant's Microsoft Entra app client secret, as JSON {"client_secret": ...}.
# Same rule as Google: Terraform creates only the container and the owner puts
# the value (see the microsoft_oauth_secret_put output), so the secret never
# enters state. The client id is not secret and is var.microsoft_client_id.
# Nothing reads this until email.microsoft.enabled is true.
resource "aws_secretsmanager_secret" "microsoft_oauth" {
  name                    = "${local.email_name}/microsoft-oauth"
  description             = "Microsoft Entra client secret for moose-email: JSON {client_secret}"
  recovery_window_in_days = 7
}

# HMAC key that authenticates moose-email to moose-approver's service API
# (decision 29). Generated ephemerally and written write-only, so it is in
# neither the plan nor the state.
ephemeral "random_password" "service_hmac" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "service_hmac" {
  name                    = "${local.email_name}/service-hmac"
  description             = "HMAC-SHA256 key shared by moose-email and moose-approver"
  recovery_window_in_days = 7
}

resource "aws_secretsmanager_secret_version" "service_hmac" {
  secret_id                = aws_secretsmanager_secret.service_hmac.id
  secret_string_wo         = ephemeral.random_password.service_hmac.result
  secret_string_wo_version = 1
}
