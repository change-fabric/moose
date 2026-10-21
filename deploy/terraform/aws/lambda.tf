data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${local.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "lambda" {
  statement {
    sid = "Table"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:Query",
      "dynamodb:Scan",
      "dynamodb:UpdateItem",
      "dynamodb:ConditionCheckItem",
    ]
    resources = [aws_dynamodb_table.moose.arn]
  }

  # TransactWriteItems has no IAM action of its own: each item is authorized
  # by the per-item actions above.
  statement {
    sid       = "Secrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.gdrive_sa_key.arn, aws_secretsmanager_secret.relay_secret.arn]
  }

  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "${local.name}-lambda"
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}

resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${local.name}"
  retention_in_days = var.log_retention_days
}

# Outside the VPC on purpose: Drive, DynamoDB, and Secrets Manager are reached
# over public endpoints without a NAT gateway. Ingress is the private API only.
resource "aws_lambda_function" "moose" {
  function_name                  = local.name
  role                           = aws_iam_role.lambda.arn
  filename                       = var.lambda_zip
  source_code_hash               = filebase64sha256(var.lambda_zip)
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  memory_size                    = 256
  timeout                        = 29
  reserved_concurrent_executions = var.lambda_reserved_concurrency

  environment {
    variables = {
      MOOSE_CONFIG                = "/var/task/moose.yaml"
      MOOSE_ENV                   = "prod"
      MOOSE_DYNAMO_TABLE          = aws_dynamodb_table.moose.name
      MOOSE_RELAY_SECRET_ARN      = aws_secretsmanager_secret.relay_secret.arn
      MOOSE_GDRIVE_KEY_SECRET_ARN = aws_secretsmanager_secret.gdrive_sa_key.arn
      MOOSE_BOOTSTRAP_ADMIN       = var.bootstrap_admin
    }
  }

  depends_on = [aws_iam_role_policy.lambda, aws_cloudwatch_log_group.lambda]
}

resource "aws_lambda_permission" "apigw" {
  statement_id  = "AllowPrivateApiInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.moose.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.moose.execution_arn}/*/*"
}
