data "aws_ssm_parameter" "al2023_arm64" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

data "aws_caller_identity" "current" {}

# Small artifacts bucket: the one S3 bucket this root creates, holding the
# relay proxy binary the instance downloads at boot.
resource "aws_s3_bucket" "artifacts" {
  bucket        = "${local.name}-artifacts-${data.aws_caller_identity.current.account_id}"
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_object" "relay_proxy" {
  bucket      = aws_s3_bucket.artifacts.id
  key         = "relay/moose-relay-proxy"
  source      = var.relay_proxy_binary
  source_hash = filemd5(var.relay_proxy_binary)
}

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "relay" {
  name               = "${local.name}-relay"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "relay_ssm_core" {
  role       = aws_iam_role.relay.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "relay" {
  statement {
    sid       = "RelaySecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.relay_secret.arn]
  }

  statement {
    sid       = "Params"
    actions   = ["ssm:GetParameter"]
    resources = [data.aws_ssm_parameter.al2023_arm64.arn]
  }

  statement {
    sid       = "Artifact"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/relay/*"]
  }
}

resource "aws_iam_role_policy" "relay" {
  name   = "${local.name}-relay"
  role   = aws_iam_role.relay.id
  policy = data.aws_iam_policy_document.relay.json
}

resource "aws_iam_instance_profile" "relay" {
  name = "${local.name}-relay"
  role = aws_iam_role.relay.name
}

locals {
  # The VPCE-specific hostname reaches the private API without a Host override.
  api_upstream = "https://${aws_api_gateway_rest_api.moose.id}-${aws_vpc_endpoint.execute_api.id}.execute-api.${data.aws_region.current.name}.vpce.amazonaws.com/${aws_api_gateway_stage.v0.stage_name}"
}

resource "aws_instance" "relay" {
  ami                         = data.aws_ssm_parameter.al2023_arm64.insecure_value
  instance_type               = "t4g.nano"
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.relay.id]
  associate_public_ip_address = true
  source_dest_check           = false # subnet router
  iam_instance_profile        = aws_iam_instance_profile.relay.name
  user_data_replace_on_change = true

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_size = 8
    volume_type = "gp3"
    encrypted   = true
  }

  user_data = templatefile("${path.module}/relay-user-data.sh.tftpl", {
    ts_authkey       = tailscale_tailnet_key.relay.key
    hostname         = var.relay_hostname
    vpc_cidr         = var.vpc_cidr
    region           = data.aws_region.current.name
    relay_secret_arn = aws_secretsmanager_secret.relay_secret.arn
    proxy_s3_uri     = "s3://${aws_s3_bucket.artifacts.id}/${aws_s3_object.relay_proxy.key}"
    proxy_etag       = aws_s3_object.relay_proxy.etag
    proxy_image      = var.relay_proxy_image
    upstream         = local.api_upstream
  })

  tags = { Name = "${local.name}-relay" }

  lifecycle {
    # The AMI parameter moves with every AL2023 release; replace deliberately.
    ignore_changes = [ami]
  }
}
