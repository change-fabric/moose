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
}

# Outside the VPC on purpose: Drive, DynamoDB, and Secrets Manager are reached
# over public endpoints without a NAT gateway. Ingress is the private API only.
module "lambda" {
  source = "../modules/lambda-service"

  name               = local.name
  zip_path           = var.lambda_zip
  policy_json        = data.aws_iam_policy_document.lambda.json
  api_execution_arn  = aws_api_gateway_rest_api.moose.execution_arn
  log_retention_days = var.log_retention_days

  environment = {
    MOOSE_CONFIG                = "/var/task/moose.yaml"
    MOOSE_ENV                   = "prod"
    MOOSE_DYNAMO_TABLE          = aws_dynamodb_table.moose.name
    MOOSE_RELAY_SECRET_ARN      = aws_secretsmanager_secret.relay_secret.arn
    MOOSE_GDRIVE_KEY_SECRET_ARN = aws_secretsmanager_secret.gdrive_sa_key.arn
    MOOSE_BOOTSTRAP_ADMIN       = var.bootstrap_admin
  }
}
