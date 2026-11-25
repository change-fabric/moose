# Exactly one of each ingress resource: one HTTP API, its $default stage, one
# ACM certificate with DNS validation, one custom domain, one Route53 alias and
# one API mapping. The lambda-service module creates none of these. DNS is
# aws_route53_record only; the zone is looked up by id and never managed.

resource "aws_apigatewayv2_api" "this" {
  name          = local.email_name
  protocol_type = "HTTP"
  description   = "moose-email and moose-approver for ${var.domain_name}"
  # Only the custom domain serves traffic, so the receipt rp id, the OAuth
  # issuer and the passkey origin can never be reached under another hostname.
  disable_execute_api_endpoint = true
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.this.id
  name        = "$default"
  auto_deploy = true

  # The Lambda concurrency quota is shared across the account and no function
  # reserves any, so bound what the API lets through.
  default_route_settings {
    throttling_rate_limit  = var.api_throttle_rate
    throttling_burst_limit = var.api_throttle_burst
  }
}

resource "aws_acm_certificate" "this" {
  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "validation" {
  for_each = {
    for o in aws_acm_certificate.this.domain_validation_options : o.domain_name => {
      name   = o.resource_record_name
      type   = o.resource_record_type
      record = o.resource_record_value
    }
  }

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = false
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}

resource "aws_apigatewayv2_domain_name" "this" {
  domain_name = var.domain_name

  domain_name_configuration {
    certificate_arn = aws_acm_certificate_validation.this.certificate_arn
    endpoint_type   = "REGIONAL"
    security_policy = "TLS_1_2"
  }
}

resource "aws_route53_record" "alias" {
  for_each = toset(["A", "AAAA"])

  zone_id = var.route53_zone_id
  name    = var.domain_name
  type    = each.value

  alias {
    name                   = aws_apigatewayv2_domain_name.this.domain_name_configuration[0].target_domain_name
    zone_id                = aws_apigatewayv2_domain_name.this.domain_name_configuration[0].hosted_zone_id
    evaluate_target_health = false
  }
}

resource "aws_apigatewayv2_api_mapping" "this" {
  api_id      = aws_apigatewayv2_api.this.id
  domain_name = aws_apigatewayv2_domain_name.this.id
  stage       = aws_apigatewayv2_stage.default.id
}

resource "aws_apigatewayv2_integration" "email" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "AWS_PROXY"
  integration_uri        = module.email.invoke_arn
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_integration" "approver" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "AWS_PROXY"
  integration_uri        = module.approver.invoke_arn
  payload_format_version = "2.0"
}

locals {
  # The route map. Every route is listed: there is no catch-all, so a path that
  # is not named here is a 404 from API Gateway and never reaches a function.
  email_routes = [
    "GET /healthz",
    "ANY /email/mcp",
    "ANY /.well-known/{proxy+}",
    "ANY /register",
    "GET /authorize",
    "GET /authorize/poll",
    "ANY /token",
    "ANY /connect/{proxy+}",
  ]
  approver_routes = [
    "ANY /approve/r/{proxy+}",
    "ANY /approve/register/{proxy+}",
    "ANY /approve/api/{proxy+}",
    "ANY /approve/queue",
    "ANY /approve/queue/{proxy+}",
  ]
}

resource "aws_apigatewayv2_route" "email" {
  for_each  = toset(local.email_routes)
  api_id    = aws_apigatewayv2_api.this.id
  route_key = each.value
  target    = "integrations/${aws_apigatewayv2_integration.email.id}"
}

resource "aws_apigatewayv2_route" "approver" {
  for_each  = toset(local.approver_routes)
  api_id    = aws_apigatewayv2_api.this.id
  route_key = each.value
  target    = "integrations/${aws_apigatewayv2_integration.approver.id}"
}
