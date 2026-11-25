# Skeleton root. The ingress (one HTTP API, domain, ACM cert, Route53 records),
# both lambda-service instances, DynamoDB tables, KMS keys, secrets, the GitHub
# OIDC deploy role and the budget land in later phases. Rules for this root:
#   - DNS is aws_route53_record only; the hosted zone and state bucket are never created.
#   - No function sets reserved concurrency (the account quota is shared).
data "aws_caller_identity" "current" {}
