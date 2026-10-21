# Tailnet ACL tags and grants are not managed here (the policy file is shared
# tailnet-wide); paste deploy/tailscale/acl-snippet.hujson by hand.

# One-time key for the relay's first boot.
resource "tailscale_tailnet_key" "relay" {
  reusable      = false
  ephemeral     = false
  preauthorized = true
  expiry        = 3600
  tags          = ["tag:moose-relay"]
  description   = "moose ${var.env} relay"
}

# The relay registers during boot; wait for it, then approve its VPC route.
# If the device is not up in time, run `terraform apply` again.
data "tailscale_device" "relay" {
  hostname = var.relay_hostname
  wait_for = "300s"

  depends_on = [aws_instance.relay]
}

resource "tailscale_device_subnet_routes" "relay" {
  device_id = data.tailscale_device.relay.node_id
  routes    = [var.vpc_cidr]
}
