# Import a channel by its id, from the console (Dashboard → Settings → Alerts).
# The API never returns a channel's URL or PagerDuty key, so write it into the
# configuration afterwards: when it matches the channel's url_hint or key_hint
# it is recorded in place, and anything else replaces the channel. A webhook's
# signing secret cannot be read again and stays null after an import.
terraform import kividb_alert_channel.slack 3b1f6a0e-6c55-4a8e-9c1d-2f8e7b6a5d41
