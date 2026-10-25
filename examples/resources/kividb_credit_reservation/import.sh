# Import a reservation by its id (the kividb_credit_reservations data source
# lists them). It is imported in cents: write amount_cents in the configuration
# with the reserved_cents it shows, or months, which is then recorded as is
# without re-pricing anything.
terraform import kividb_credit_reservation.orders 3f2c1d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f
