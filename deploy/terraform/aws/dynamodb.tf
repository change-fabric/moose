resource "aws_dynamodb_table" "moose" {
  name                        = local.name
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

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  # v0: point-in-time recovery off; SSE with the AWS-owned key (the default).
  point_in_time_recovery {
    enabled = false
  }
}
