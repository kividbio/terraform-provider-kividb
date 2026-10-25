# Planned maintenance on Tuesdays from 02:00 to 06:00, Berlin time -- after
# the overnight batch jobs and before the working day.
resource "kividb_maintenance_window" "cache" {
  instance_id = kividb_instance.cache.id
  day         = "tuesday"
  hour        = 2
  timezone    = "Europe/Berlin"
}

# When the next window opens, and what KiviDB has planned for it.
output "next_maintenance" {
  value = kividb_maintenance_window.cache.next_window_start
}
