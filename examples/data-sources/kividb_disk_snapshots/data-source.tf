# The finished disk snapshots of one database, newest first -- scheduled ones
# included, which no configuration created.
data "kividb_disk_snapshots" "orders" {
  instance_id = "790df3ae-c064-45f5-b924-9ed89471fd72"
  status      = "ready"
}

output "latest_disk_snapshot" {
  value = try(data.kividb_disk_snapshots.orders.snapshots[0].id, null)
}
