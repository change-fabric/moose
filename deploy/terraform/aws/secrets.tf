# Secret shells only. Values are set out of band so key material never enters
# Terraform state:
#   aws secretsmanager put-secret-value --secret-id moose/<env>/gdrive-sa-key --secret-string file://gdrive-sa.json
#   aws secretsmanager put-secret-value --secret-id moose/<env>/relay-secret --secret-string "$(openssl rand -base64 48)"

resource "aws_secretsmanager_secret" "gdrive_sa_key" {
  name        = "moose/${var.env}/gdrive-sa-key"
  description = "Google Drive service account JSON key for moose"
}

resource "aws_secretsmanager_secret" "relay_secret" {
  name        = "moose/${var.env}/relay-secret"
  description = "Shared secret the moose relay proxy sends to the Lambda"
}
