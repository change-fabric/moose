# Alarms for everything the slice and Phases 12 to 15 added: both functions, the
# one API and both tables. They notify the alerts topic that attachments.tf
# creates (the same addresses as the sweeper alarms, each confirming the SNS
# subscription once). treat_missing_data is notBreaching throughout: a quiet
# single-tenant mailbox is normal and must not page anyone. The sweeper has its
# own alarms in attachments.tf.

locals {
  alarm_functions = {
    email    = module.email.function_name
    approver = module.approver.function_name
  }
  alarm_tables = {
    email    = aws_dynamodb_table.email.name
    approver = aws_dynamodb_table.approver.name
  }
  # The function timeout is 29 seconds, one second under the API limit. Warn at 25.
  duration_warn_ms = 25000
}

# A failed invocation (panic, timeout, init failure) is never a normal outcome:
# tool and HTTP errors are returned as responses, not raised.
resource "aws_cloudwatch_metric_alarm" "function_errors" {
  for_each = local.alarm_functions

  alarm_name          = "${each.value}-errors"
  alarm_description   = "${each.value} had failed invocations (panic, timeout or init failure). Check its log group."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# The account-wide concurrency quota is shared and no function reserves any, so
# a throttle here means another workload is starving moose (or the reverse).
resource "aws_cloudwatch_metric_alarm" "function_throttles" {
  for_each = local.alarm_functions

  alarm_name          = "${each.value}-throttles"
  alarm_description   = "${each.value} invocations were throttled by the shared account concurrency quota."
  namespace           = "AWS/Lambda"
  metric_name         = "Throttles"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# A dispatch that runs close to the timeout risks the `unknown` send state: the
# Gmail call may have been accepted with the outcome never recorded.
resource "aws_cloudwatch_metric_alarm" "function_duration" {
  for_each = local.alarm_functions

  alarm_name          = "${each.value}-duration"
  alarm_description   = "${each.value} ran longer than ${local.duration_warn_ms / 1000} seconds, close to its timeout."
  namespace           = "AWS/Lambda"
  metric_name         = "Duration"
  dimensions          = { FunctionName = each.value }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = local.duration_warn_ms
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${local.email_name}-api-5xx"
  alarm_description   = "The moose API returned 5xx responses: a function failed or API Gateway could not reach it."
  namespace           = "AWS/ApiGateway"
  metric_name         = "5xx"
  dimensions          = { ApiId = aws_apigatewayv2_api.this.id }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 2
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# One owner and a handful of connectors: a burst of 4xx is a probe or a client
# that cannot sign in, never normal use.
resource "aws_cloudwatch_metric_alarm" "api_4xx" {
  alarm_name          = "${local.email_name}-api-4xx"
  alarm_description   = "An unusual number of 4xx responses on the moose API (probing, or a client that cannot sign in)."
  namespace           = "AWS/ApiGateway"
  metric_name         = "4xx"
  dimensions          = { ApiId = aws_apigatewayv2_api.this.id }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 100
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# Both tables are on-demand, so a throttle means a hot key or an account limit.
# A system error is AWS's own failure. Either one can stall a send claim.
resource "aws_cloudwatch_metric_alarm" "table_system_errors" {
  for_each = local.alarm_tables

  alarm_name          = "${each.value}-system-errors"
  alarm_description   = "DynamoDB table ${each.value} returned system errors."
  namespace           = "AWS/DynamoDB"
  metric_name         = "SystemErrors"
  dimensions          = { TableName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "table_write_throttles" {
  for_each = local.alarm_tables

  alarm_name          = "${each.value}-write-throttles"
  alarm_description   = "DynamoDB table ${each.value} throttled writes."
  namespace           = "AWS/DynamoDB"
  metric_name         = "WriteThrottleEvents"
  dimensions          = { TableName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# ---- Microsoft client secret expiry reminder ----
# An expired secret turns every Microsoft account needs_reauth at once. EventBridge
# publishes one message to the alerts topic 30 days before the date the owner
# records in var.microsoft_client_secret_expires_on. The cron has a year field,
# so it fires once. Rotation steps: docs/email-tenant.md.

locals {
  ms_secret_remind_at = var.microsoft_client_secret_expires_on == null ? null : timeadd("${var.microsoft_client_secret_expires_on}T09:00:00Z", "-720h")
}

resource "aws_cloudwatch_event_rule" "microsoft_secret_expiry" {
  count = local.ms_secret_remind_at == null ? 0 : 1

  name                = "${local.email_name}-microsoft-secret-expiry"
  description         = "One-off reminder, 30 days before the Microsoft client secret expires"
  schedule_expression = "cron(${formatdate("mm hh DD MM", local.ms_secret_remind_at)} ? ${formatdate("YYYY", local.ms_secret_remind_at)})"
}

resource "aws_cloudwatch_event_target" "microsoft_secret_expiry" {
  count = local.ms_secret_remind_at == null ? 0 : 1

  rule = aws_cloudwatch_event_rule.microsoft_secret_expiry[0].name
  arn  = aws_sns_topic.alerts.arn

  input_transformer {
    input_template = jsonencode("moose-email: the Microsoft client secret expires on ${var.microsoft_client_secret_expires_on} (30 days from now). Create a new secret in Entra and rotate it: see the rotation runbook in docs/email-tenant.md.")
  }
}

# Setting a topic policy replaces SNS's default one, so the default statement
# (same-account publishers, which CloudWatch alarms rely on) is restated here.
data "aws_iam_policy_document" "alerts_topic" {
  count = local.ms_secret_remind_at == null ? 0 : 1

  statement {
    sid = "DefaultSameAccount"
    actions = [
      "sns:Publish",
      "sns:Subscribe",
      "sns:Receive",
      "sns:GetTopicAttributes",
      "sns:SetTopicAttributes",
      "sns:AddPermission",
      "sns:RemovePermission",
      "sns:DeleteTopic",
      "sns:ListSubscriptionsByTopic",
    ]
    resources = [aws_sns_topic.alerts.arn]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "AWS:SourceOwner"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid       = "AllowSecretExpiryRule"
    actions   = ["sns:Publish"]
    resources = [aws_sns_topic.alerts.arn]

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.microsoft_secret_expiry[0].arn]
    }
  }
}

resource "aws_sns_topic_policy" "alerts" {
  count = local.ms_secret_remind_at == null ? 0 : 1

  arn    = aws_sns_topic.alerts.arn
  policy = data.aws_iam_policy_document.alerts_topic[0].json
}
