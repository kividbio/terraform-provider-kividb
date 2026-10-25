data "kividb_credit_reservations" "all" {}

output "unreserved_dollars" {
  value = data.kividb_credit_reservations.all.unreserved_cents / 100
}

output "unprotected_soon" {
  # Reservations with less than a dollar left.
  value = [for r in data.kividb_credit_reservations.all.reservations : r.instance_name
  if r.status == "active" && r.remaining_cents < 100]
}
