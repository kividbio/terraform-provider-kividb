# Whether the organization keeps an audit log. Needs an owner or admin key.
data "kividb_audit_log_settings" "this" {}

# Turning the log on is the owner's decision, made signed in to the console, so
# Terraform cannot do it -- but it can refuse to go on without it.
resource "terraform_data" "require_audit_log" {
  lifecycle {
    precondition {
      condition     = data.kividb_audit_log_settings.this.enabled
      error_message = "Turn on the audit log (Dashboard → Settings → Audit log, as the owner) before applying."
    }
  }
}
