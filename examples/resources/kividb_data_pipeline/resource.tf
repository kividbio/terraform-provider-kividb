variable "shop_db_password" {
  type      = string
  sensitive = true
}

# Copy two tables from a PostgreSQL database into a KiviDB database, and keep
# them in step. Billed: $0.04 an hour while it runs, plus $0.10 per GB written
# beyond 10 GB a month.
resource "kividb_data_pipeline" "shop" {
  name        = "shop-cdc"
  instance_id = kividb_instance.cache.id

  source_type     = "postgres"
  source_host     = "shop.cluster-abc123.eu-central-1.rds.amazonaws.com"
  source_port     = 5432
  source_database = "shop"
  source_user     = "kividb_cdc"
  source_password = var.shop_db_password

  tables = [
    {
      # Every column, one hash per row: HGETALL customer:42
      table = "public.customers"
      key   = "customer:{id}"
    },
    {
      # Two columns as a JSON document, one renamed, expiring after a day.
      table       = "public.orders"
      key         = "order:{id}"
      type        = "json"
      columns     = ["id", "total_cents"]
      rename      = { total_cents = "total" }
      ttl_seconds = 86400
    },
  ]

  # Set to false to stop it (and its hourly charge) without deleting it.
  enabled = true
}

output "shop_pipeline_status" {
  value = kividb_data_pipeline.shop.status
}
