# A read-only user for a reporting job. No password is set, so the API
# generates one; it is in state, so keep the state somewhere private.
resource "kividb_database_user" "reporting" {
  instance_id = kividb_instance.cache.id
  username    = "reporting"
  profile     = "read_only"
}

# A user whose password you choose -- from a secret store, not a literal.
resource "kividb_database_user" "notifier" {
  instance_id = kividb_instance.cache.id
  username    = "notifier"
  profile     = "pubsub"
  password    = var.notifier_password
}

# Rotate a generated password every month: changing rotation_trigger has the
# API generate a new one on the next apply.
resource "kividb_database_user" "worker" {
  instance_id      = kividb_instance.cache.id
  username         = "worker"
  rotation_trigger = "2026-10"
}

output "reporting_password" {
  value     = kividb_database_user.reporting.password
  sensitive = true
}
