# Your application sends its own embedding with every write and lookup, so
# KiviDB never calls a model and holds no provider key.
resource "kividb_semantic_cache" "answers" {
  instance_id    = kividb_instance.cache.id
  name           = "answers"
  embed_provider = "none"
  dimensions     = 384
}

# Or let KiviDB embed the prompt with OpenAI, using your key.
resource "kividb_semantic_cache" "support" {
  instance_id         = kividb_instance.cache.id
  name                = "support"
  embed_provider      = "openai"
  embed_model         = "text-embedding-3-small"
  embed_api_key       = var.openai_api_key
  dimensions          = 1536
  threshold           = 0.7
  default_ttl_seconds = 3600
}

# Destroying either one deletes every entry cached in it.
