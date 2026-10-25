# Deliver the database's exports to your bucket, and export it every day.
resource "kividb_instance_export" "orders" {
  instance_id = kividb_instance.orders.id

  # The verification's id is the destination's id; referencing it, rather than
  # the destination, is what orders this after the destination is verified.
  export_destination_id = kividb_export_destination_verification.backups.id
  cadence               = "daily" # "off", "daily" or "weekly"
}
