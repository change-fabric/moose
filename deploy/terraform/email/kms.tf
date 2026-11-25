# Symmetric key: seals Google refresh tokens and stored message snapshots
# (envelope encryption, encryption context {owner, account_id}). Only the
# moose-email role uses it.
resource "aws_kms_key" "data" {
  description             = "moose-email ${var.env}: seals refresh tokens and approved message snapshots"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.email_name}-data"
  target_key_id = aws_kms_key.data.key_id
}

# Symmetric key: seals the exact previews the approver stores. Kept apart from
# the data key so the moose-email role cannot read a preview.
resource "aws_kms_key" "preview" {
  description             = "moose-approver ${var.env}: seals stored previews"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_kms_alias" "preview" {
  name          = "alias/${local.approver_name}-preview"
  target_key_id = aws_kms_key.preview.key_id
}

# Asymmetric key that signs receipts. Only the moose-approver role gets
# kms:Sign; moose-email verifies offline against the pinned public key.
resource "aws_kms_key" "receipt" {
  description              = "moose-approver ${var.env}: signs receipts"
  key_usage                = "SIGN_VERIFY"
  customer_master_key_spec = "ECC_NIST_P256"
  deletion_window_in_days  = 30
}

resource "aws_kms_alias" "receipt" {
  name          = "alias/${local.approver_name}-receipt"
  target_key_id = aws_kms_key.receipt.key_id
}

# The public half, pinned into moose-email's environment.
data "aws_kms_public_key" "receipt" {
  key_id = aws_kms_key.receipt.key_id
}
