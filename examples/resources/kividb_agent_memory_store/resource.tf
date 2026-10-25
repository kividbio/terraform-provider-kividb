# No hosted models: your application sends its own vectors, and keyword search
# works without any. Sessions are kept but not summarized, and long-term
# memories are the ones your application adds.
resource "kividb_agent_memory_store" "support" {
  instance_id    = kividb_instance.cache.id
  name           = "support"
  embed_provider = "none"
  llm_provider   = "none"
  dimensions     = 384
}

# With models: OpenAI embeddings, and Anthropic summarizing sessions and
# extracting long-term memories from them, with your keys.
resource "kividb_agent_memory_store" "assistant" {
  instance_id    = kividb_instance.cache.id
  name           = "assistant"
  embed_provider = "openai"
  embed_model    = "text-embedding-3-small"
  embed_api_key  = var.openai_api_key
  dimensions     = 1536
  llm_provider   = "anthropic"
  llm_model      = "claude-haiku-4-5"
  llm_api_key    = var.anthropic_api_key

  session_ttl_seconds = 86400

  custom_types = [{
    name        = "product"
    description = "Which of our products the user uses or is asking about."
  }]
}

# Destroying either one deletes every session and memory in it.
