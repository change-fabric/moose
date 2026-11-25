# The two functions. Each role gets exactly what its service uses; only the
# approver role holds kms:Sign on the receipt key.

data "aws_iam_policy_document" "email" {
  statement {
    sid = "Table"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
      "dynamodb:Query",
    ]
    resources = [aws_dynamodb_table.email.arn, "${aws_dynamodb_table.email.arn}/index/*"]
  }

  statement {
    sid       = "Seal"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
    resources = [aws_kms_key.data.arn]
  }

  # email_get_attachment streams into att/ and presigns the link; the sweeper
  # (its own role) deletes. The key above covers SSE-KMS on these objects.
  statement {
    sid       = "Attachments"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${aws_s3_bucket.attachments.arn}/att/*"]
  }

  statement {
    sid     = "Secrets"
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      aws_secretsmanager_secret.google_oauth.arn,
      aws_secretsmanager_secret.service_hmac.arn,
    ]
  }
}

module "email" {
  source = "../modules/lambda-service"

  name              = local.email_name
  zip_path          = var.email_zip_path
  policy_json       = data.aws_iam_policy_document.email.json
  api_execution_arn = aws_apigatewayv2_api.this.execution_arn
  memory_size       = 512

  environment = {
    MOOSE_CONFIG                    = "/var/task/moose.yaml"
    MOOSE_ENV                       = "prod"
    MOOSE_DYNAMO_TABLE              = aws_dynamodb_table.email.name
    MOOSE_EMAIL_BASE_URL            = local.origin
    MOOSE_EMAIL_KMS_KEY_ID          = aws_kms_key.data.arn
    MOOSE_EMAIL_ATTACH_BUCKET       = aws_s3_bucket.attachments.bucket
    MOOSE_GOOGLE_OAUTH_SECRET_ARN   = aws_secretsmanager_secret.google_oauth.arn
    MOOSE_EMAIL_OWNER               = var.owner
    MOOSE_EMAIL_RP_ID               = var.domain_name
    MOOSE_EMAIL_RECEIPT_PUBLIC_KEY  = data.aws_kms_public_key.receipt.public_key_pem
    MOOSE_EMAIL_APPROVER_SECRET_ARN = aws_secretsmanager_secret.service_hmac.arn
  }
}

data "aws_iam_policy_document" "approver" {
  statement {
    sid = "Table"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
      "dynamodb:Query",
    ]
    resources = [aws_dynamodb_table.approver.arn, "${aws_dynamodb_table.approver.arn}/index/*"]
  }

  statement {
    sid       = "Seal"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
    resources = [aws_kms_key.preview.arn]
  }

  statement {
    sid       = "SignReceipts"
    actions   = ["kms:Sign", "kms:GetPublicKey"]
    resources = [aws_kms_key.receipt.arn]
  }

  statement {
    sid       = "Secrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.service_hmac.arn]
  }
}

module "approver" {
  source = "../modules/lambda-service"

  name              = local.approver_name
  zip_path          = var.approver_zip_path
  policy_json       = data.aws_iam_policy_document.approver.json
  api_execution_arn = aws_apigatewayv2_api.this.execution_arn

  environment = {
    MOOSE_CONFIG                   = "/var/task/moose.yaml"
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

# The approver also runs its expiry sweep on a schedule (it deletes the sealed
# previews of lapsed requests). A scheduled event carries a detail-type, which
# the handler recognizes.
resource "aws_cloudwatch_event_rule" "approver_sweep" {
  name                = "${local.approver_name}-sweep"
  description         = "Expire overdue approval requests and delete their previews"
  schedule_expression = "rate(15 minutes)"
}

resource "aws_cloudwatch_event_target" "approver_sweep" {
  rule = aws_cloudwatch_event_rule.approver_sweep.name
  arn  = module.approver.function_arn
}

resource "aws_lambda_permission" "approver_sweep" {
  statement_id  = "AllowSweepSchedule"
  action        = "lambda:InvokeFunction"
  function_name = module.approver.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.approver_sweep.arn
}
