# Memory above 90% for ten minutes, to Slack and the owners.
resource "kividb_alert_rule" "memory" {
  name        = "cache memory above 90%"
  metric      = "memory_pct"
  instance_id = kividb_instance.cache.id
  op          = "gt"
  threshold   = 90
  for_minutes = 10
  channel_ids = [
    kividb_alert_channel.slack.id,
    kividb_alert_channel.owners.id,
  ]
}

# A failover fires on the event itself: no op, threshold or for_minutes.
resource "kividb_alert_rule" "failover" {
  name        = "cache failed over"
  metric      = "failover"
  instance_id = kividb_instance.cache.id
  channel_ids = [kividb_alert_channel.pagerduty.id]
}

# Credit runway is about the organization, so it names no database. Left unset,
# op and threshold take the metric's defaults: below 7 days.
resource "kividb_alert_rule" "credit" {
  name             = "credit running low"
  metric           = "credit_runway_days"
  cooldown_minutes = 1440
  channel_ids      = [kividb_alert_channel.owners.id]
}
