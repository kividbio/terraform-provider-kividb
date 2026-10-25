---
page_title: "kividb_data_pipeline Resource"
subcategory: ""
description: |-
  A data integration pipeline: change data capture from PostgreSQL or MySQL into a KiviDB database.
---

# kividb_data_pipeline (Resource)

A data integration pipeline copies tables from your PostgreSQL or MySQL
database into a KiviDB database, first as a snapshot and then change by
change, so the KiviDB copy stays seconds behind the source. Each row becomes
one key — a hash, or a JSON document — named by a template such as
`customer:{id}`.

```terraform
variable "shop_db_password" {
  type      = string
  sensitive = true
}

# Copy two tables from a PostgreSQL database into a KiviDB database, and keep
# them in step. Billed: $0.04 an hour while it runs, plus $0.10 per GB written
# beyond 10 GB a month.
resource "kividb_data_pipeline" "shop" {
  name        = "shop-cdc"
  instance_id = kividb_instance.cache.id

  source_type     = "postgres"
  source_host     = "shop.cluster-abc123.eu-central-1.rds.amazonaws.com"
  source_port     = 5432
  source_database = "shop"
  source_user     = "kividb_cdc"
  source_password = var.shop_db_password

  tables = [
    {
      # Every column, one hash per row: HGETALL customer:42
      table = "public.customers"
      key   = "customer:{id}"
    },
    {
      # Two columns as a JSON document, one renamed, expiring after a day.
      table       = "public.orders"
      key         = "order:{id}"
      type        = "json"
      columns     = ["id", "total_cents"]
      rename      = { total_cents = "total" }
      ttl_seconds = 86400
    },
  ]

  # Set to false to stop it (and its hourly charge) without deleting it.
  enabled = true
}

output "shop_pipeline_status" {
  value = kividb_data_pipeline.shop.status
}
```

## What it costs

A pipeline is billed while it runs: **$0.04 for every hour, or part of an
hour**, in which it runs, plus **$0.10 per GB** it writes into the database
beyond 10 GB a month. The target database is billed as usual.

Starting a pipeline needs enough credit in the organization to cover about a
month of it ($29.20) on top of what is already running. When there is not
enough, the create — or setting `enabled = true` — is refused with the
shortfall in the API's words, and nothing starts. Organizations whose
databases are not charged are not gated.

## What the source needs

KiviDB connects to the source from its own network, so `source_host` must
resolve to a public address; private, loopback and other reserved addresses
are refused. Every table needs a primary key.

- **PostgreSQL** (logical replication): `wal_level = logical`, and a user with
  `REPLICATION` — on Amazon RDS, the `rds_replication` role, with
  `rds.logical_replication = 1` in the parameter group — that owns the tables
  or may create a publication for them. The pipeline makes one replication
  slot and one publication in the source, both named `kividb_…`.
- **MySQL** 5.7, 8.0, 8.4 or MariaDB (the binary log): `log_bin` on,
  `binlog_format = ROW`, `binlog_row_image = FULL`, and a user with
  `REPLICATION SLAVE`, `REPLICATION CLIENT` and `SELECT` on the tables.

Before saving a create or a change, the API connects with the settings given
and reads one row of each table. A typo, a wrong password or a source not set
up for replication fails the apply there, with the source's own message, and
nothing is saved.

Key templates should use the primary key's columns: a delete carries only
those, unless a PostgreSQL table has `REPLICA IDENTITY FULL`.

## Starting and stopping

A pipeline is created running; the API has no way to create one stopped. With
`enabled = false` the provider stops it straight after the create, so it is
charged for that first hour and may have started copying.

`enabled = false` stops a pipeline without deleting it, and the hourly charge
stops with it. A stopped **PostgreSQL** pipeline keeps its replication slot in
the source, and the source keeps its WAL for us until the pipeline is started
again or deleted. Leave one stopped for long and the source's disk fills.

`enabled = true` starts a stopped pipeline, and retries one that failed. A
pipeline stopped in the console shows as a change on the next plan.

## Changing a pipeline

The source's host, port, database, user, password, TLS mode and the tables
change in place; each change is checked against the source first. Changing
the tables copies every table again from the start, then carries on streaming.
To copy everything again without changing anything, change
`resnapshot_trigger` to any new value.

`name`, `instance_id` and `source_type` cannot change. Changing one replaces
the pipeline: the old one is deleted (and its replication slot dropped) and a
new one copies every table again.

## The password

The API stores `source_password` encrypted and never returns it. Terraform
keeps the value you configured in state, so treat the state as sensitive. A
password changed in the console is not seen by Terraform, and an apply that
changes any other source setting sends the configured password again.

## Deleting

Destroy stops the pipeline, waits for it to let go (up to 30 seconds), and
drops the replication slot and publication it made in a PostgreSQL source.

If the slot cannot be dropped — the source is unreachable, say — the destroy
fails with the API's explanation and the pipeline stays, stopped. Fix the
connection and destroy again, or set `force_delete = true`, apply, and destroy:
the pipeline is then deleted anyway, the destroy warns which slot was left,
and KiviDB keeps trying to drop it for about four days before telling the
organization to drop it by hand. Until then the source keeps its WAL.
`force_delete` is read from state, which is why it has to be applied before
the destroy.

Deleting the target database deletes its pipelines; the next plan creates the
pipeline again.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `instance_id` (String) The KiviDB database rows are written into. It cannot change: pointing this at another database replaces the pipeline, which drops its replication slot and copies every table again into the new one.
- `name` (String) The pipeline's name: 1-63 lowercase letters, digits, `-` or `_`, starting with a letter or digit. Unique in the organization. Changing it replaces the pipeline.
- `source_database` (String) The database in the source server the tables are in.
- `source_host` (String) The source database's hostname or IPv4 address. It must resolve to a public address: KiviDB connects from its own network, and refuses private, loopback and other reserved addresses.
- `source_password` (String, Sensitive) The source user's password. The API stores it encrypted and never returns it, so Terraform keeps the value you configured in state (mark the state as sensitive accordingly) and cannot see a password changed in the console. After an import the first apply sends it.
- `source_port` (Number) The source database's port, such as 5432 or 3306.
- `source_type` (String) `postgres` (PostgreSQL 10+, logical replication) or `mysql` (MySQL 5.7, 8.0, 8.4 or MariaDB, the binary log). It cannot change; changing it replaces the pipeline. PostgreSQL needs `wal_level = logical`, a user with `REPLICATION` (on Amazon RDS, the `rds_replication` role) that owns the tables or may create a publication for them, and a primary key on every table. MySQL needs `log_bin` on, `binlog_format = ROW`, `binlog_row_image = FULL`, and a user with `REPLICATION SLAVE`, `REPLICATION CLIENT` and `SELECT` on the tables.
- `source_user` (String) The user KiviDB connects to the source as. See `source_type` for what it needs.
- `tables` (Attributes List) The tables to copy, 1 to 50, and where each row goes. Adding, removing or changing a table copies every table again from the start, then carries on streaming. (see [below for nested schema](#nestedatt--tables))

### Optional

- `enabled` (Boolean) Whether the pipeline runs. Defaults to true. Setting it to true starts the pipeline (and retries one that has failed), which needs enough organization credit for about a month of it; false stops it, and the hourly charge stops with it. A stopped PostgreSQL pipeline keeps its replication slot in the source, so the source keeps its WAL for us until the pipeline is started again or deleted: a pipeline left stopped for long fills the source's disk. A pipeline created with `enabled = false` is created running and stopped straight after, because that is how the API creates one: it is charged for that first hour, and may have started copying.
- `force_delete` (Boolean) What a destroy does when KiviDB cannot drop the pipeline's replication slot in a PostgreSQL source (the source is unreachable, say). False, the default: the destroy fails with the API's explanation and the pipeline stays, stopped. True: the pipeline is deleted anyway and KiviDB keeps trying to drop the slot for about four days, then tells the organization which slot to drop by hand. Until it is dropped, the source keeps its WAL. Set it and apply before destroying: a destroy uses the value in state.
- `resnapshot_trigger` (String) Any value; changing it copies every table again from the start and then carries on streaming, without changing anything else. Nothing happens when it is first set on a new pipeline, which copies everything anyway.
- `source_tls` (String) How to connect to the source: `require` (encrypted, certificate not checked; the default), `verify` (encrypted and the certificate checked) or `off` (plain text).

### Read-Only

- `changes_applied` (Number) Changes applied since the snapshot.
- `consecutive_failures` (Number) Failures in a row since the pipeline last ran cleanly.
- `created_at` (String) When the pipeline was created.
- `id` (String) The pipeline's name, which is how the API identifies it.
- `instance_name` (String) The target database's name.
- `lag_ms` (Number) How far behind the source the KiviDB copy was at the last change, in milliseconds.
- `last_change_at` (String) When the last change was applied.
- `last_error` (String) The last failure, in the words of the source or of KiviDB. A failed pipeline is retried on its own.
- `last_error_at` (String) When the last failure happened.
- `rows_snapshotted` (Number) Rows copied by the snapshot.
- `slot_name` (String) The replication slot the pipeline made in a PostgreSQL source, once it has made one. Null for MySQL.
- `snapshot_done` (Boolean) Whether the first copy of every table has finished.
- `status` (String) What the pipeline is doing: `pending`, `snapshotting`, `streaming`, `stopped` or `error`.

<a id="nestedatt--tables"></a>
### Nested Schema for `tables`

Required:

- `key` (String) The key each row is written to: literal text with at least one `{column}` placeholder and no spaces, such as `customer:{id}`. Use the primary key's columns: a delete carries only those (unless a PostgreSQL table has `REPLICA IDENTITY FULL`).
- `table` (String) `schema.table` for PostgreSQL (`public` when the schema is left out); just the table for MySQL, whose database is `source_database`. The table needs a primary key.

Optional:

- `columns` (List of String) Copy only these columns, 1 to 200. Every column is copied when this is left out. Every placeholder in `key` must be one of them.
- `rename` (Map of String) Store a column under another field name: `{ column = "field" }`.
- `ttl_seconds` (Number) Expire each key this many seconds after it is written, 60 to 31536000 (a year). Keys do not expire when this is left out.
- `type` (String) `hash` (one field per column; the default) or `json` (one JSON document per row).

## Import

Import is supported using the following syntax:

```shell
# Import an existing pipeline by its name. The API never returns the source
# password, so set source_password in the configuration: the first apply after
# the import sends it.
terraform import kividb_data_pipeline.shop shop-cdc
```
