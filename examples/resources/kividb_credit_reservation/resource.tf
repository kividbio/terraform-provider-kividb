# Set three months of the orders database's price aside for it. Nothing is
# spent: the credit leaves the unreserved balance, which every other database
# runs on. `terraform plan` shows the API's refusal if the balance cannot cover
# it, and warns which databases are left closer to being stopped.
resource "kividb_credit_reservation" "orders" {
  instance_id = kividb_instance.orders.id
  months      = 3
}

# Or a fixed amount, in US cents. Raising it adds the difference in place;
# lowering it replaces the reservation (the plan says what that does).
resource "kividb_credit_reservation" "search" {
  instance_id  = kividb_instance.search.id
  amount_cents = 5000
}

output "orders_reserved_left" {
  value = kividb_credit_reservation.orders.remaining_cents
}
