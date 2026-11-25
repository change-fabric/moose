output "account_id" {
  description = "AWS account the root is applied into."
  value       = data.aws_caller_identity.current.account_id
}
