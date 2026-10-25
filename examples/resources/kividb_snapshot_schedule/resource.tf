# A disk snapshot every day, keeping the newest seven.
resource "kividb_snapshot_schedule" "cache" {
  instance_id    = kividb_instance.cache.id
  cadence        = "daily"
  retention_mode = "keep_last"
  retention      = 7
}

# Hourly snapshots of a busier database, each kept for two days.
resource "kividb_snapshot_schedule" "orders" {
  instance_id    = kividb_instance.orders.id
  cadence        = "hourly"
  retention_mode = "keep_days"
  retention      = 2
}
