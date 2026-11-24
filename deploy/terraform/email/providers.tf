provider "aws" {
  region  = var.region
  profile = var.aws_profile

  default_tags {
    tags = {
      Project     = "moose"
      Environment = var.env
      ManagedBy   = "terraform"
    }
  }
}
