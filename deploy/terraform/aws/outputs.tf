output "relay_tailnet_hostname" {
  description = "Tailnet hostname clients use: https://<this>.<tailnet>.ts.net/mcp"
  value       = var.relay_hostname
}

output "api_invoke_url" {
  description = "Private API invoke URL (reachable only through the VPC endpoint)."
  value       = aws_api_gateway_stage.v0.invoke_url
}

output "api_vpce_url" {
  description = "VPC-endpoint-specific API URL the relay proxy forwards to."
  value       = local.api_upstream
}

output "vpce_dns_name" {
  description = "execute-api VPC endpoint DNS name."
  value       = aws_vpc_endpoint.execute_api.dns_entry[0].dns_name
}

output "dynamodb_table" {
  value = aws_dynamodb_table.moose.name
}

output "lambda_function" {
  value = aws_lambda_function.moose.function_name
}

output "relay_instance_id" {
  value = aws_instance.relay.id
}

output "secret_ids" {
  description = "Secrets to fill with aws secretsmanager put-secret-value."
  value = {
    gdrive_sa_key = aws_secretsmanager_secret.gdrive_sa_key.name
    relay_secret  = aws_secretsmanager_secret.relay_secret.name
  }
}
