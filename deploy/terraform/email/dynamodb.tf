# Both tables share the shape internal/email/store/dynamo expects: pk/sk, a
# sparse g1-index on the lowercased address, and a ttl attribute.
resource "aws_dynamodb_table" "email" {
  name                        = local.email_name
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = "pk"
  range_key                   = "sk"
  deletion_protection_enabled = var.deletion_protection

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  attribute {
    name = "g1"
    type = "S"
  }

  global_secondary_index {
    name            = "g1-index"
    hash_key        = "g1"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  # Accounts and the send ledger live here: keep a recovery window.
  point_in_time_recovery {
    enabled = true
  }
}

resource "aws_dynamodb_table" "approver" {
  name                        = local.approver_name
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = "pk"
  range_key                   = "sk"
  deletion_protection_enabled = var.deletion_protection

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  attribute {
    name = "g1"
    type = "S"
  }

  global_secondary_index {
    name            = "g1-index"
    hash_key        = "g1"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  # Passkeys, requests and receipts. Previews are short lived and sealed, so no
  # point-in-time copy of them is kept.
  point_in_time_recovery {
    enabled = false
  }
}
