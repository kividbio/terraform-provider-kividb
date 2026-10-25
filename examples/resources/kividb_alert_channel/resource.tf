# The organization's owners and admins, by email. An email channel sends only to
# the organization's own members, chosen by role, never to an address you type.
resource "kividb_alert_channel" "owners" {
  name  = "Owners and admins"
  kind  = "email"
  roles = ["owner", "admin"]

  # A channel cannot be edited, only replaced, and a channel an alert still
  # sends to cannot be deleted. This lets the alert move to the new channel
  # before the old one goes.
  lifecycle {
    create_before_destroy = true
  }
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}

resource "kividb_alert_channel" "slack" {
  name = "#ops-alerts"
  kind = "slack"
  url  = var.slack_webhook_url

  lifecycle {
    create_before_destroy = true
  }
}

variable "pagerduty_integration_key" {
  type      = string
  sensitive = true
}

resource "kividb_alert_channel" "pagerduty" {
  name        = "Database on-call"
  kind        = "pagerduty"
  routing_key = var.pagerduty_integration_key
  region      = "us"

  lifecycle {
    create_before_destroy = true
  }
}

# A signed webhook. The signing secret is shown once, when the channel is
# created, and kept in state for you to hand to the receiver.
resource "kividb_alert_channel" "webhook" {
  name = "Incident bridge"
  kind = "webhook"
  url  = "https://hooks.example.com/kividb"

  lifecycle {
    create_before_destroy = true
  }
}

output "webhook_signing_secret" {
  value     = kividb_alert_channel.webhook.signing_secret
  sensitive = true
}
