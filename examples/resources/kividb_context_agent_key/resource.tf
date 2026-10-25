# A key that sees only one tenant's customers and orders.
resource "kividb_context_agent_key" "acme" {
  model    = kividb_context_model.shop.name
  name     = "acme support agent"
  entities = ["customer", "order"]
  filters = {
    customer = { tenant = "acme" }
  }
}

# What an MCP client needs: the URL, and the key as a bearer token.
output "acme_mcp_url" {
  value = kividb_context_agent_key.acme.mcp_url
}

output "acme_agent_key" {
  value     = kividb_context_agent_key.acme.key
  sensitive = true
}
