provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = "moose"
      Environment = var.env
      ManagedBy   = "terraform"
    }
  }
}

# Credentials come from the environment: TAILSCALE_OAUTH_CLIENT_ID and
# TAILSCALE_OAUTH_CLIENT_SECRET (or TAILSCALE_API_KEY). Never put them in tfvars.
provider "tailscale" {
  tailnet = var.tailnet
}
