# A KDB export of the database, taken once, before something irreversible.
resource "kividb_kdb_snapshot" "before_migration" {
  instance_id = kividb_instance.orders.id
  label       = "before-migration"

  # Copied into the database's export destination too, if it has a verified
  # one (kividb_instance_export). Set false to keep it in KiviDB's storage only.
  deliver = true
}

# A new database made from it, in the same cloud, region and account.
resource "kividb_instance" "orders_copy" {
  name                         = "orders-copy"
  tier                         = "essentials"
  cloud                        = kividb_kdb_snapshot.before_migration.cloud
  region                       = kividb_kdb_snapshot.before_migration.region
  data_size_gb                 = 8
  restore_from_kdb_snapshot_id = kividb_kdb_snapshot.before_migration.id
}
