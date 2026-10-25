# The ready KDB exports of one database, newest first.
data "kividb_kdb_snapshots" "orders" {
  instance_id = "790df3ae-c064-45f5-b924-9ed89471fd72"
  status      = "ready"
}

# Make a new database from the newest one.
resource "kividb_instance" "orders_restored" {
  name                         = "orders-restored"
  tier                         = "essentials"
  cloud                        = data.kividb_kdb_snapshots.orders.snapshots[0].cloud
  region                       = data.kividb_kdb_snapshots.orders.snapshots[0].region
  data_size_gb                 = 8
  restore_from_kdb_snapshot_id = data.kividb_kdb_snapshots.orders.snapshots[0].id
}
