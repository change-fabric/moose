# Attachment handling for email_get_attachment: a private bucket whose objects
# live for about an hour, the sweeper that deletes them, and the alarms that
# say when it stops.
#
#   - Objects are private, SSE-KMS under the data key, never public, and never
#     versioned. Versioning is set to Disabled on purpose: a versioned bucket
#     keeps a deleted attachment as a noncurrent version, which would defeat
#     the sweeper.
#   - The sweeper (cmd/moose-attach-sweeper) runs every 15 minutes and deletes
#     anything under att/ older than 1 hour. The 1-day lifecycle rule is only
#     the backstop for when the sweeper is broken.
#   - Extracted text is never stored anywhere; only the original file is here.

locals {
  sweeper_name      = "moose-attach-sweeper-${var.env}"
  attach_alarm_mail = var.alarm_emails == null ? var.budget_emails : var.alarm_emails
}

resource "aws_s3_bucket" "attachments" {
  bucket = "${local.email_name}-attachments-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_ownership_controls" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "attachments" {
  bucket                  = aws_s3_bucket.attachments.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  versioning_configuration {
    status = "Disabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    bucket_key_enabled = true

    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.data.arn
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    id     = "expire-attachments"
    status = "Enabled"

    filter {
      prefix = "att/"
    }

    expiration {
      days = 1
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }

  # Parts of an upload that died mid-way belong to no object and are invisible
  # to the sweeper's listing, so the rule above must also cover the whole bucket.
  rule {
    id     = "abort-stale-multipart-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

data "aws_iam_policy_document" "attachments_bucket" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.attachments.arn, "${aws_s3_bucket.attachments.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "attachments" {
  bucket = aws_s3_bucket.attachments.id
  policy = data.aws_iam_policy_document.attachments_bucket.json

  depends_on = [aws_s3_bucket_public_access_block.attachments]
}

# ---- the sweeper ----

resource "aws_sqs_queue" "sweeper_dlq" {
  name                      = "${local.sweeper_name}-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}

data "aws_iam_policy_document" "sweeper" {
  statement {
    sid       = "List"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.attachments.arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["att/*", "att/"]
    }
  }

  statement {
    sid       = "Delete"
    actions   = ["s3:DeleteObject"]
    resources = ["${aws_s3_bucket.attachments.arn}/att/*"]
  }

  statement {
    sid       = "DeadLetter"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.sweeper_dlq.arn]
  }
}

module "sweeper" {
  source = "../modules/lambda-service"

  name        = local.sweeper_name
  zip_path    = var.sweeper_zip_path
  policy_json = data.aws_iam_policy_document.sweeper.json
  memory_size = 128
  timeout     = 300

  # Runs from the schedule only: no API route, no invoke permission.
  allow_api = false

  dead_letter_enabled    = true
  dead_letter_target_arn = aws_sqs_queue.sweeper_dlq.arn

  environment = {
    MOOSE_EMAIL_ATTACH_BUCKET = aws_s3_bucket.attachments.bucket
  }
}

# EventBridge invokes the function asynchronously, so a failed run is retried
# twice by Lambda and then goes to the dead-letter queue. No reserved
# concurrency: one run at a time is enough, and a throttled run is retried.
resource "aws_lambda_function_event_invoke_config" "sweeper" {
  function_name                = module.sweeper.function_name
  maximum_retry_attempts       = 2
  maximum_event_age_in_seconds = 3600
}

resource "aws_cloudwatch_event_rule" "sweeper" {
  name                = local.sweeper_name
  description         = "Delete attachment objects older than an hour"
  schedule_expression = "rate(15 minutes)"
}

resource "aws_cloudwatch_event_target" "sweeper" {
  rule = aws_cloudwatch_event_rule.sweeper.name
  arn  = module.sweeper.function_arn
}

resource "aws_lambda_permission" "sweeper_schedule" {
  statement_id  = "AllowSweepSchedule"
  action        = "lambda:InvokeFunction"
  function_name = module.sweeper.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.sweeper.arn
}

# ---- alarms ----

resource "aws_sns_topic" "alerts" {
  name = "moose-${var.env}-alerts"
}

resource "aws_sns_topic_subscription" "alerts" {
  for_each = toset(local.attach_alarm_mail)

  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = each.value
}

# Fires when any attachment is more than 2 hours old, which means the sweeper
# is failing to delete it (the lifecycle rule removes it within a day anyway).
# The sweeper publishes the age of the oldest object it left behind on every run.
resource "aws_cloudwatch_metric_alarm" "attachment_age" {
  alarm_name          = "${local.sweeper_name}-oldest-attachment"
  alarm_description   = "An email attachment object is more than 2 hours old: the sweeper is not deleting it."
  namespace           = "Moose/Attachments"
  metric_name         = "OldestAttachmentAgeSeconds"
  statistic           = "Maximum"
  period              = 900
  evaluation_periods  = 1
  threshold           = 7200
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

# Covers the case the age alarm cannot see: a sweeper that never runs publishes
# no age at all. Eight quiet 15-minute periods in a row is two hours.
resource "aws_cloudwatch_metric_alarm" "sweeper_silent" {
  alarm_name          = "${local.sweeper_name}-silent"
  alarm_description   = "The attachment sweeper has published nothing for 2 hours."
  namespace           = "Moose/Attachments"
  metric_name         = "OldestAttachmentAgeSeconds"
  statistic           = "SampleCount"
  period              = 900
  evaluation_periods  = 8
  datapoints_to_alarm = 8
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "sweeper_dlq" {
  alarm_name          = "${local.sweeper_name}-dead-letters"
  alarm_description   = "A sweeper run failed after its retries and was dead-lettered."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.sweeper_dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}
