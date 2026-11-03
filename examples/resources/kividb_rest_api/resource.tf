# An HTTPS endpoint for the database, for serverless and edge functions.
resource "kividb_rest_api" "cache" {
  instance_id = kividb_instance.cache.id
}

# A read-only user, whose token is safe to use from a browser.
resource "kividb_database_user" "web" {
  instance_id = kividb_instance.cache.id
  username    = "web"
  profile     = "read_only"
}

# What an app on @upstash/redis sets: UPSTASH_REDIS_REST_URL and
# UPSTASH_REDIS_REST_TOKEN. The token is kvr_ + base64url("user:password").
output "rest_url" {
  value = kividb_rest_api.cache.url
}

output "rest_token_web" {
  value     = "kvr_${replace(replace(replace(base64encode("${kividb_database_user.web.username}:${kividb_database_user.web.password}"), "+", "-"), "/", "_"), "=", "")}"
  sensitive = true
}
