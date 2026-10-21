# The VPC hosts only the relay EC2 and the execute-api endpoint. The Lambda runs
# outside it, so there is no NAT gateway.

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name = "moose-${var.env}"
  azs  = slice(data.aws_availability_zones.available.names, 0, 2)
}

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = local.name }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = local.name }
}

# /26 public subnet for the relay, two /26 private subnets.
resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 2, 0)
  availability_zone = local.azs[0]

  tags = { Name = "${local.name}-public" }
}

resource "aws_subnet" "private" {
  count             = 2
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 2, count.index + 1)
  availability_zone = local.azs[count.index]

  tags = { Name = "${local.name}-private-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${local.name}-private" }
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

resource "aws_security_group" "relay" {
  name        = "${local.name}-relay"
  description = "moose relay: no inbound, Tailscale needs outbound only"
  vpc_id      = aws_vpc.this.id

  tags = { Name = "${local.name}-relay" }
}

resource "aws_vpc_security_group_egress_rule" "relay_all" {
  security_group_id = aws_security_group.relay.id
  description       = "All outbound (Tailscale, package installs, VPC endpoint)"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_security_group" "vpce" {
  name        = "${local.name}-vpce"
  description = "execute-api endpoint: 443 from the relay only"
  vpc_id      = aws_vpc.this.id

  tags = { Name = "${local.name}-vpce" }
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_relay" {
  security_group_id            = aws_security_group.vpce.id
  description                  = "HTTPS from the relay"
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = aws_security_group.relay.id
}

data "aws_region" "current" {}

# One AZ (the relay's subnet) halves the interface endpoint cost for v0.
resource "aws_vpc_endpoint" "execute_api" {
  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${data.aws_region.current.name}.execute-api"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = [aws_subnet.public.id]
  security_group_ids  = [aws_security_group.vpce.id]

  tags = { Name = "${local.name}-execute-api" }
}
