# The account's GitHub OIDC provider already exists and is shared with other
# projects: it is looked up, never created.
data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

# Deploy role for moose's GitHub Actions. It is trusted by the immutable
# repository id, not the repository name, so a renamed or recreated repository
# under the same name cannot assume it. It can only push new code to the two
# functions; infrastructure changes are applied by a person.
data "aws_iam_policy_document" "github_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:repository_id"
      values   = [var.github_repository_id]
    }

    # IAM requires a scoped sub claim on GitHub OIDC trust; only main may deploy.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:change-fabric/moose:ref:refs/heads/main"]
    }
  }
}

resource "aws_iam_role" "deploy" {
  name               = "moose-email-${var.env}-deploy"
  assume_role_policy = data.aws_iam_policy_document.github_trust.json
}

data "aws_iam_policy_document" "deploy" {
  statement {
    sid = "UpdateCode"
    actions = [
      "lambda:GetFunction",
      "lambda:UpdateFunctionCode",
      "lambda:PublishVersion",
    ]
    resources = [module.email.function_arn, module.approver.function_arn]
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "update-function-code"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}
