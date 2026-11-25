# One Go Lambda (arm64, provided.al2023) with its role and log group. It creates
# no API, domain, or DNS resources: the calling root owns ingress. A function that
# only runs on a schedule sets allow_api = false and gets no invoke permission. It sets no
# reserved concurrency because the account-wide quota is shared.

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "this" {
  name               = "${var.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = var.log_retention_days
}

data "aws_iam_policy_document" "logs" {
  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.this.arn}:*"]
  }
}

resource "aws_iam_role_policy" "logs" {
  name   = "${var.name}-logs"
  role   = aws_iam_role.this.id
  policy = data.aws_iam_policy_document.logs.json
}

resource "aws_iam_role_policy" "extra" {
  name   = "${var.name}-lambda"
  role   = aws_iam_role.this.id
  policy = var.policy_json
}

resource "aws_lambda_function" "this" {
  function_name    = var.name
  role             = aws_iam_role.this.arn
  filename         = var.zip_path
  source_code_hash = filebase64sha256(var.zip_path)
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  memory_size      = var.memory_size
  timeout          = var.timeout

  environment {
    variables = var.environment
  }

  # Async invocations that still fail after the retries land on this queue.
  dynamic "dead_letter_config" {
    for_each = var.dead_letter_enabled ? [1] : []
    content {
      target_arn = var.dead_letter_target_arn
    }
  }

  depends_on = [aws_iam_role_policy.logs, aws_iam_role_policy.extra, aws_cloudwatch_log_group.this]
}

resource "aws_lambda_permission" "api" {
  count = var.allow_api ? 1 : 0

  statement_id  = "AllowApiInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.this.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${var.api_execution_arn}/*/*"
}
