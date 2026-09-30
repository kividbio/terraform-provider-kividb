resource "kividb_disk_snapshot" "before_migration" {
  instance_id = kividb_instance.cache.id
  label       = "before-migration"
}

# The label is the only thing about a snapshot you can change afterwards.
# Pointing instance_id at a different database plans a new snapshot and destroys
# this one: nothing re-points a copy that has already been taken.
