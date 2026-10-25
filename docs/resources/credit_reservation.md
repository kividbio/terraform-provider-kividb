---
page_title: "kividb_credit_reservation Resource"
subcategory: ""
description: |-
  Part of the organization's credit set aside for one database, so other databases cannot spend it.
---

# kividb_credit_reservation (Resource)

Part of the organization's credit set aside for one database, so other
databases cannot spend it.

```terraform
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
```

## What it does to money

- **Nothing is spent and nothing is added.** A reservation moves part of the
  balance you already have out of the unreserved part.
- **The credit is set aside.** The reserved database's own charges draw its
  reservation down (`consumed_cents`); `remaining_cents` is what is still set
  aside.
- **Other databases see a smaller balance.** Every database without a
  reservation runs on the balance less what is still reserved.
- **When the unreserved part runs out, the databases without a reservation
  are stopped first,** so the reserved database keeps running on what was set
  aside for it. When the whole balance runs out, everything stops, as before.
- **Owner or admin keys only.** A member key is refused, at plan as well as at
  apply.

## What the plan checks

The API refuses a reservation larger than the unreserved balance, and one that
would leave the running databases without a reservation less than a day of
credit. `terraform plan` asks the API the same question before anything is
applied, for a new reservation, a raise, a re-activation and a replacement:

- **Refused:** the plan fails with the API's own message, for example
  `Only $100.00 of your balance is unreserved; add credits to reserve $200.00.`
- **Allowed, with databases left closer to being stopped:** a warning names
  them and says what stays unreserved for them and how long it lasts, before
  and after the change:

  ```
  Warning: Databases without a reservation are left closer to being stopped

  This reservation sets aside $14.52. After this, $35.40 stays unreserved for 1
  database without a reservation (beta): about 72.2 days of running before they
  are stopped. Before this change: $49.92 unreserved, about 50.9 days.
  ```

When the database or the amount is not known until apply (a database created
in the same run, say), the plan asks nothing and the apply meets the API's
refusal instead. A destroy is never blocked; it warns that the database loses
its protection.

For a replacement, Terraform shows the refusal of the new reservation as a
plan error. It does not show the new reservation's own plan warnings, so the
provider names the databases put at risk from the plan for the old one. That
plan does not run for `terraform apply -replace=...` when nothing in the
configuration changed, so that one case shows a refusal but no at-risk
warning.

## Months or cents

Set exactly one of `months` and `amount_cents`.

`months` is priced by the API when the credit is set aside, from what the
database costs then. A later resize does not re-price it: `months` stays as
written, `reserved_cents` keeps the amount actually set aside, and nothing
shows as a change. Raising `months` sets aside the extra months at the
database's price on the day of the apply.

`amount_cents` is the reservation's total and is read back, so credit added in
the console shows as a change on the next plan; write the new total into the
configuration to keep it.

## Raising, lowering, pausing

A reservation can only grow. Raising `months` or `amount_cents` sets aside the
difference in place. The API adds at least 100 cents ($1.00) at a time, so a
smaller raise in cents fails at plan.

Lowering it replaces the reservation. The old one is deactivated and deleted,
which returns only what it still holds (`remaining_cents`) to the unreserved
balance. Then a new one sets aside the new amount in full. Credit already drawn
from the old reservation is not given back to it. When less than the new
amount is left on the old one, the replacement sets aside more, not less, and
the plan's warning says so with the amounts. This is a replacement rather than
a plan-time error because a replacement is the only way to a lower reservation
the API offers, and an error would leave no way to get there from Terraform.

To stop protecting the database without setting anything aside again, set
`active = false`: what is left returns to the unreserved balance and the plan
warns that the database is no longer protected. `active = true` activates it
again with what it still holds, which the unreserved balance must cover.

Changing `instance_id` replaces the reservation too, and the plan names the
database it leaves without protection.

## When KiviDB changes it

Metering moves `consumed_cents` and `remaining_cents` every hour; they are
never a change to apply.

Deleting a database deactivates its reservation. A refresh warns about it,
and it cannot be activated again: remove the resource from the configuration
(destroying it deletes the record). A reservation that is used up warns on
refresh too.

## Deleting

Destroy deactivates the reservation, returning what it still holds to the
unreserved balance, and deletes it.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `instance_id` (String) The database the credit is set aside for. A database has at most one active reservation. Changing this replaces the reservation: the old one is deactivated, so what it still holds returns to the unreserved balance, and deleted, then the new one is created.

### Optional

- `active` (Boolean) Whether the reservation protects its database. Defaults to true. False deactivates it: what is left returns to the unreserved balance and the database is no longer protected. True again re-activates it with what it still holds, which the unreserved balance must cover. KiviDB deactivates a reservation itself when its database is deleted; a refresh warns, and it cannot be activated again.
- `amount_cents` (Number) How much to set aside, in US cents: 100 ($1.00) to 100000000. Set this or `months`. This is the reservation's total, and is read back, so credit added in the console shows as a change. A reservation can only grow: raising this sets aside the difference (at least 100 cents) in place. Lowering it replaces the reservation: the old one is deactivated, returning only what it still holds (`remaining_cents`) to the unreserved balance, and a new one sets aside the new amount in full. When less than the new amount is left on the old one, that sets aside more, not less; the plan says so. To stop protecting the database without a new reservation, set `active = false`.
- `months` (Number) How much to set aside, in months of the database's price, 1 to 12. Set this or `amount_cents`. The API prices the months when they are set aside, from what the database costs then; a later resize does not re-price them, and `reserved_cents` keeps the amount actually set aside. Raising `months` sets aside the extra months at the price of the day. Lowering it replaces the reservation (see `amount_cents`).

### Read-Only

- `consumed_cents` (Number) What the database's charges have drawn from it. Metering moves it every hour, so it changes between refreshes without ever being a change to apply.
- `created_at` (String) When the reservation was created.
- `deactivated_at` (String) When it was deactivated. Null while active.
- `deactivated_reason` (String) Why an inactive reservation is inactive: `user`, or `instance_deleted` when KiviDB deactivated it because its database was deleted. Null while active.
- `id` (String) The reservation's id.
- `instance_name` (String) The database's name, or the name it had once it is deleted.
- `remaining_cents` (Number) `reserved_cents` less `consumed_cents`: what is still set aside. Moves every hour, like `consumed_cents`.
- `reserved_cents` (Number) Everything set aside on this reservation, in cents, including what has since been drawn.
- `status` (String) `active` or `inactive`.

## Import

Import is supported using the following syntax:

```shell
# Import a reservation by its id (the kividb_credit_reservations data source
# lists them). It is imported in cents: write amount_cents in the configuration
# with the reserved_cents it shows, or months, which is then recorded as is
# without re-pricing anything.
terraform import kividb_credit_reservation.orders 3f2c1d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f
```
