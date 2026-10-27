---
page_title: "kividb_instance Resource"
subcategory: ""
description: |-
  A KiviDB Cloud database.
---

# kividb_instance (Resource)

A KiviDB Cloud database.

```terraform
resource "kividb_instance" "cache" {
  name         = "orders-cache"
  tier         = "essentials"
  cloud        = "aws"
  region       = "us-east-1"
  data_size_gb = 8
}

# Growing the database is an in-place change: raise data_size_gb and apply.
# Moving it to another region is not -- Terraform plans a replacement, and
# replacing a database destroys its data. Read the plan.
output "endpoint" {
  value = kividb_instance.cache.endpoint
}

# A replicated database. Pro starts at 4 GB on AWS and 8 GB on Azure and Google
# Cloud, because neither has a 1-vCPU sustained-performance machine.
resource "kividb_instance" "primary" {
  name          = "orders-primary"
  tier          = "pro"
  cloud         = "azure"
  region        = "swedencentral"
  data_size_gb  = 8
  replica_count = 2
  tls_enabled   = true
}

# The same on Google Cloud. Pro starts at 8 GB there too: GCP's custom machine
# types begin at two vCPUs.
resource "kividb_instance" "gcp_primary" {
  name          = "orders-gcp"
  tier          = "pro"
  cloud         = "gcp"
  region        = "us-central1"
  data_size_gb  = 8
  replica_count = 1
}
```

## What changes in place, and what replaces the database

| Attribute | Effect of changing it |
|---|---|
| `name` | renamed in place; the endpoint hostname follows |
| `data_size_gb`, `throughput_mode` | resized in place |
| `replica_count` | scaled in place, between 1 and 3 |
| `tier` | upgraded in place |
| `kividb_version`, `lua_enabled`, `tls_enabled`, `replication_tls`, `client_cert_mode`, `client_ca_pem` | applied by a rolling restart |
| `tls_only` | changed in place, without a restart |
| `cloud`, `region`, `cloud_account_id`, `aof_enabled`, `free_tier`, `shard_count` | **replaces the database** |
| `restore_from_snapshot_id`, `restore_from_kdb_snapshot_id` | **replaces the database** when changed to a different snapshot |

The last rows are the ones to read carefully. Nothing moves a database between
clouds or regions, or between the free tier and a paid one, or changes how many
shards it has. Terraform will do exactly what you asked — destroy the old
database and create a new, empty one.

```
  # kividb_instance.cache must be replaced
      ~ region = "us-east-1" -> "eu-west-1" # forces replacement
Plan: 1 to add, 0 to change, 1 to destroy.
```

## Running in your own cloud account

Set `cloud_account_id` to run the database in a cloud account of your own
instead of KiviDB's. Connect the account in the KiviDB console first, under
**Dashboard → Settings → Cloud accounts**, and look it up with the
[`kividb_cloud_account`](../data-sources/cloud_account.md) data source:

```terraform
data "kividb_cloud_account" "production" {
  name = "production"
}

resource "kividb_instance" "orders" {
  name             = "orders"
  tier             = "pro"
  cloud            = "aws"
  region           = "eu-central-1"
  data_size_gb     = 8
  replica_count    = 2
  cloud_account_id = data.kividb_cloud_account.production.id
}

output "private_endpoint" {
  value = kividb_instance.orders.private_endpoint
}
```

AWS accounts, Google Cloud projects and Azure subscriptions are supported.
Set `cloud` to the account's cloud (`aws`, `gcp` or `azure`). The account must be
`verified`, and `region` must be one of the regions enabled for it. A database that cannot be placed in the account is refused at apply with
a message saying why.

The database stays in the account it was created in. **Changing
`cloud_account_id` replaces the database**, and replacing it destroys its data.

A database in your own account also has hostnames that resolve inside your
network:

| Attribute | What it points at |
|---|---|
| `private_endpoint` | the primary |
| `private_readonly_endpoint` | reads spread across the replicas (Pro) |
| `private_replica_endpoints` | one hostname per replica (Pro) |

They carry no port; connect on the same port as `endpoint`. They follow the
database's name, so a rename changes them, and `private_replica_endpoints`
grows and shrinks with `replica_count`. For a database in KiviDB's cloud they
are null and `private_replica_endpoints` is empty.

An existing database stays in the account it is in whether or not
`cloud_account_id` is in its configuration: removing the attribute does not
move it to KiviDB's cloud, and an imported database needs no
`cloud_account_id` to keep it where it is. Setting it to a different account
does replace the database.

## Creating a database from a snapshot

Set `restore_from_snapshot_id` to a disk snapshot, or
`restore_from_kdb_snapshot_id` to a KDB snapshot, to create the database with
that snapshot's data:

```terraform
resource "kividb_disk_snapshot" "nightly" {
  instance_id = kividb_instance.orders.id
  label       = "before-migration"
}

resource "kividb_instance" "orders_copy" {
  name                     = "orders-copy"
  tier                     = "pro"
  cloud                    = "aws"          # the snapshot's cloud
  region                   = "eu-central-1" # and its region
  data_size_gb             = 8
  replica_count            = 1
  restore_from_snapshot_id = kividb_disk_snapshot.nightly.id
}
```

A snapshot is restored only where it lives: in its own cloud, its own region
and its own cloud account. Snapshots never leave the cloud or the account they
were taken in, so the database you create from one must be there too.

- Leave `cloud_account_id` unset and the database is placed in the snapshot's
  account (or in KiviDB's cloud, for a snapshot taken there).
  `cloud_account_id` and `cloud_account_name` then report where it went.
- Set `cloud_account_id`, and it must be the snapshot's account.

When the snapshot's id is known at plan time, the plan reads where the snapshot
lives, shows the account the database will be placed in, and refuses a
different cloud, region or account there and then:

```
Error: The snapshot is in a different region

  Disk snapshot 5b2d7c1e-... is in eu-central-1 and this database is in
  us-east-1. Set `region = "eu-central-1"`, or move the snapshot to us-east-1
  first and then create the database from it.
```

When the id is not known until apply -- a snapshot created in the same apply,
as above -- the API applies the same rule at apply, and the error says which
cloud, region or account the snapshot is in.

The snapshot is read only when the database is created. Changing
`restore_from_snapshot_id` or `restore_from_kdb_snapshot_id` to a different
snapshot **replaces the database**, and replacing it destroys its data.
Removing the attribute afterwards changes nothing, and neither does adding it
to a database that already exists (an imported one, say); the plan warns that
nothing is restored. To create a database again from a snapshot, use
`terraform apply -replace`.

To move data between clouds or accounts, export it from the source database and
import it into the target.

## The free tier

Set `free_tier = true` for the free database. It is one per organization owner,
across every organization they own, on AWS only, in `us-east-1`,
`eu-central-1`, `ap-south-1`, `ap-northeast-1` or `af-south-1`:

```terraform
resource "kividb_instance" "sandbox" {
  name         = "sandbox"
  tier         = "essentials"
  cloud        = "aws"
  region       = "eu-central-1"
  data_size_gb = 1
  free_tier    = true
}
```

A free database is always an Essentials database of 1 GB, without replicas,
Lua, TLS or append-only persistence, on a host it shares with other free
databases. The API builds it that way whatever it is sent, so the plan refuses
any other value for those attributes instead of creating something the
configuration does not describe. Upgrading it to a paid database is done in the
console; in Terraform, changing `free_tier` replaces it.

## Append-only persistence

Every paid database keeps an append-only file, and the free tier does not: the
API decides, from the tier. Leave `aof_enabled` unset and the plan shows what
the database will have. Setting it to the other value is refused at plan,
because the API would ignore it.

## Encryption between nodes, and client certificates

`replication_tls = true` encrypts replication between a database's own nodes
with its TLS certificate, so a replica's copy of the data never crosses the
network in plaintext.

`client_cert_mode` asks clients on the TLS port for a certificate signed by
the CA in `client_ca_pem` (mutual TLS):

| `client_cert_mode` | A client without a certificate |
|---|---|
| `off` | connects (with its password) |
| `optional` | connects; a certificate it does offer must be valid |
| `required` | is refused |

```terraform
resource "kividb_instance" "payments" {
  name             = "payments"
  tier             = "pro"
  cloud            = "aws"
  region           = "eu-central-1"
  data_size_gb     = 8
  replica_count    = 1
  tls_enabled      = true
  tls_only         = true
  replication_tls  = true
  client_cert_mode = "required"
  client_ca_pem    = file("${path.module}/client-ca.pem")
}
```

Both need `tls_enabled`. `required` also needs `tls_only`, or a client could
use the plaintext port to connect without a certificate. Neither is available
on a sharded database yet. The plan refuses each of these combinations.

They are applied by the same rolling restart as an engine change, together with
any change to `kividb_version`, `lua_enabled` or `tls_enabled` in the same
apply. The API does not take them when a database is created, so a new database
that sets them is created first and then restarted with them once it is
running; the apply waits for both. The CA bundle is kept as you wrote it: the
API stores only its certificates, and the same certificates with different line
endings do not plan a change. Read the CA's expiry with the
[`kividb_client_certificates`](../data-sources/client_certificates.md) data
source.

## TLS-only

`tls_only = true` refuses plaintext connections: the allowlist is open on the
TLS port, 6380, and not on 6379. It needs `tls_enabled`, and it changes in
place with no restart, because it is a firewall setting rather than an engine
one.

It is ordered against the engine settings it depends on. Turning TLS on,
TLS-only on and `client_cert_mode = "required"` in one apply restarts the
database with TLS, then makes it TLS-only, then restarts it again to require
client certificates, since each step is refused before the one it rests on.
Going back the other way is ordered in reverse.

The allowlist itself is managed by
[`kividb_instance_network`](instance_network.md), and closing public access
altogether by [`kividb_private_link`](private_link.md). The three write
through the same API endpoint, and each changes only its own setting.

## Shards

`shard_count` above 1 makes a sharded database: `data_size_gb` and
`replica_count` are then per shard. Changing `shard_count` replaces the
database.

- **Where:** sharded databases are offered in the regions that can build them,
  AWS us-east-1 and us-west-2 at launch. Elsewhere, or for an arrangement
  larger than the region can take today, the API refuses the create and says
  why.
- **Credit:** a sharded database must set `reserve_months` to at least 1. It
  cannot be stopped when the balance runs out, so it reserves its first month
  when it is created.
- **Above 512 GB in total,** the database is provisioned once capacity has been
  reserved for it: send a capacity request from the console first.

```terraform
resource "kividb_instance" "orders" {
  name           = "orders"
  tier           = "essentials"
  cloud          = "aws"
  region         = "us-east-1"
  data_size_gb   = 16 # per shard
  shard_count    = 4  # 64 GB in total
  replica_count  = 0
  reserve_months = 1
}
```

## Pro has a minimum size, and it differs by cloud

| Cloud | Pro minimum |
|---|---|
| AWS | 4 GB |
| Azure | 8 GB |
| GCP | 8 GB |

That is hardware, not pricing. Neither Azure nor GCP has a 1-vCPU
sustained-performance machine — GCP's custom machine types start at two vCPUs and
must be even — so the smallest honest Pro instance on either is an 8 GB-shaped
machine. A smaller Pro database is refused with both numbers and the cloud named:

```
Pro on aws starts at 4 GB and this instance is 2 GB. Resize it to at least
4 GB first, then upgrade.
```

## Replicas do not scale to zero

`replica_count` moves between 1 and 3 on a Pro database. Going back to `0` is
refused: a replicated database and a single-node one are laid out differently,
and the read endpoint exists only on the replicated one. Create a new database
without replicas and migrate, or keep at least one replica.

## Changing several things at once

This works. Most of these operations refuse a database still busy with the
previous one, so the provider performs them in order and waits between them —
the apply takes roughly as long as the operations do, added together. Terraform
prints `Still modifying…` throughout; that is the wait, not a hang.

If something fails partway, state records what the database actually is rather
than what was planned, so the next plan shows the remaining difference.

## When a change is accepted and does not happen

An update waits for the change itself, not merely for the status to settle. A
database can return to `running` without the change having landed — the work
behind it is queued, and if that work fails the database stays healthy at its
old shape. The apply then says so:

```
Error: The change was accepted but the database did not settle

  the change was accepted but never took effect: data_size_gb is still 1, not
  2. The database is healthy and reports status "running", so the work behind
  the change did not reach it
```

## Deleting

Destroy deletes the database and its data. The call returns as soon as the
deletion is accepted and teardown continues in the background, so a
configuration that destroys and immediately recreates under the same name can
race its own teardown.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `cloud` (String) `aws`, `azure` or `gcp` (Google Cloud). **Changing this replaces the database.**
- `data_size_gb` (Number) Logical data size in GB. Resized in place.
- `region` (String) A region of the chosen cloud, e.g. `us-east-1` (AWS), `swedencentral` (Azure) or `us-central1` (Google Cloud). **Changing this replaces the database.**
- `tier` (String) `essentials`, `pro` or `scale`. Upgrading is done in place.

### Optional

- `aof_enabled` (Boolean) Append-only persistence. Always on for a paid database and off for the free tier: the API decides it, so leave it unset. Setting it to the other value is refused at plan. **Changing this replaces the database.**
- `client_ca_pem` (String) The CA certificate, or a bundle of up to 10, that signs your client certificates, in PEM. Each must be a CA certificate valid today; a private key is refused. Set it only with `client_cert_mode` `optional` or `required`: the API discards it when the mode is `off`.
- `client_cert_mode` (String) Client certificates (mutual TLS) on the TLS port: `off`, `optional` (checked when a client offers one) or `required` (a client without one is refused). Anything but `off` needs `tls_enabled` and `client_ca_pem`; `required` also needs `tls_only`. Applied by a rolling restart.
- `cloud_account_id` (String) Run the database in your own cloud account: the id of an account connected in the KiviDB console (see the `kividb_cloud_account` data source). Omit it to run the database in KiviDB's cloud. Available on AWS, Azure and Google Cloud (set `cloud` to the account's cloud). The account must be verified and have the database's region enabled. With `restore_from_snapshot_id` or `restore_from_kdb_snapshot_id`, omitting it places the database in the snapshot's account, and setting it to any other account is refused. **Changing this to a different account replaces the database.** Removing it from the configuration of a database that exists does not move the database: it stays in the account it is in.
- `free_tier` (Boolean) Create the free database: AWS only, 1 GB, Essentials, no replicas. One per organization owner, across every organization they own. See "The free tier" above for what it fixes. **Changing this replaces the database.**
- `kividb_version` (String) Engine version, e.g. `1.0.4`. Applied by a rolling restart.
- `lua_enabled` (Boolean) Lua scripting. Applied by a rolling restart.
- `name` (String) Hostname-safe name, unique across KiviDB. Generated if omitted; changing it renames the database in place.
- `replica_count` (Number) Read replicas. `0` at creation for a single node; scaling afterwards accepts 1-3. Going back to `0` is not a scaling operation and is refused -- see the error for what to do instead.
- `replication_tls` (Boolean) Encrypt replication between this database's nodes, using its TLS certificate. Needs `tls_enabled`; not available on sharded databases, which already encrypt traffic between their nodes when TLS is on. Applied by a rolling restart.
- `reserve_months` (Number) Months of credit (0-12) to set aside for this database when it is created, as checkout's "Reserve credits" does: databases launched later cannot spend it, and if the rest of the balance runs out, databases without a reservation are stopped first. A sharded database must reserve at least 1. Used only when the database is created: changing it later does nothing (manage reservations afterwards with `kividb_credit_reservation`).
- `restore_from_kdb_snapshot_id` (String) Create the database seeded from this KDB snapshot (a `.kdb` export). Same rule as `restore_from_snapshot_id`: the snapshot's cloud, region and cloud account only. Read only when the database is created. **Changing it to a different snapshot replaces the database**; removing it, or adding it to a database that already exists, changes nothing.
- `restore_from_snapshot_id` (String) Create the database from this disk snapshot (see `kividb_disk_snapshot`). The database must be in the snapshot's cloud, region and cloud account, and at least as large as the snapshot needs; the API refuses anything else. Read only when the database is created. **Changing it to a different snapshot replaces the database**; removing it, or adding it to a database that already exists, changes nothing.
- `shard_count` (Number) Shards. `1` is an ordinary database. Above 1, `data_size_gb` and `replica_count` are per shard, and the database needs `reserve_months` of at least 1. Sharded databases are offered in the regions that can build them (AWS us-east-1 and us-west-2 at launch); elsewhere, or for an arrangement larger than the region can take, the API refuses with the reason. **Changing this replaces the database.**
- `throughput_mode` (String) `standard` or `high`. Changed in place.
- `tls_enabled` (Boolean) Serve TLS alongside plaintext. Applied by a rolling restart.
- `tls_only` (Boolean) Refuse plaintext connections: the allowlist is open on the TLS port (6380) only. Needs `tls_enabled`. Changed in place, without a restart.
- `wait_for_ready` (Boolean) Wait for the database to finish provisioning before the apply completes. On by default, because anything that depends on `endpoint` needs one that answers. Set to `false` to return as soon as the work is accepted. A new database that sets `replication_tls` or `client_cert_mode` is waited for regardless: those are applied once it runs.

### Read-Only

- `cloud_account_name` (String) The name of the cloud account in `cloud_account_id`, as the KiviDB console shows it. Null for a database in KiviDB's cloud.
- `endpoint` (String) Private endpoint.
- `id` (String) The instance's id.
- `org_id` (String) Owning organization.
- `private_endpoint` (String) Hostname that resolves to the primary inside your own cloud account's network. Set only when `cloud_account_id` is. No port: use the same port as `endpoint`.
- `private_readonly_endpoint` (String) Hostname that spreads reads across the replicas, inside your own cloud account's network. Pro databases in your own cloud account only.
- `private_replica_endpoints` (List of String) One hostname per replica, inside your own cloud account's network. Pro databases in your own cloud account only; empty otherwise.
- `public_endpoint` (String) Public endpoint, when one is exposed.
- `status` (String) Lifecycle status.

## Import

Import is supported using the following syntax:

```shell
# Import an existing database by its id, which list_databases or the console URL
# will give you.
terraform import kividb_instance.cache 790df3ae-c064-45f5-b924-9ed89471fd72
```
