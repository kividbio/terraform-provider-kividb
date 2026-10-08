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
| `kividb_version`, `lua_enabled`, `tls_enabled` | applied by a rolling restart |
| `cloud`, `region`, `cloud_account_id`, `aof_enabled`, `tls_only` | **replaces the database** |

The last row is the one to read carefully. Nothing moves a database between
clouds or regions, and append-only persistence is chosen when the volume is laid
out rather than afterwards. Terraform will do exactly what you asked — destroy
the old database and create a new, empty one.

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

When importing a database that runs in your own account, set
`cloud_account_id` in the configuration to the account it runs in, or the plan
will propose replacing it.

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

- `aof_enabled` (Boolean) Append-only persistence. **Changing this replaces the database**: it is chosen when the volume is laid out and there is no operation to change it after.
- `cloud_account_id` (String) Run the database in your own cloud account: the id of an account connected in the KiviDB console (see the `kividb_cloud_account` data source). Omit it to run the database in KiviDB's cloud. Available on AWS, Azure and Google Cloud (set `cloud` to the account's cloud). The account must be verified and have the database's region enabled. **Changing this replaces the database.**
- `kividb_version` (String) Engine version, e.g. `1.0.4`. Applied by a rolling restart.
- `lua_enabled` (Boolean) Lua scripting. Applied by a rolling restart.
- `name` (String) Hostname-safe name, unique across KiviDB. Generated if omitted; changing it renames the database in place.
- `replica_count` (Number) Read replicas. `0` at creation for a single node; scaling afterwards accepts 1-3. Going back to `0` is not a scaling operation and is refused -- see the error for what to do instead.
- `throughput_mode` (String) `standard` or `high`. Changed in place.
- `tls_enabled` (Boolean) Serve TLS alongside plaintext. Applied by a rolling restart.
- `tls_only` (Boolean) Refuse plaintext connections. **Changing this replaces the database.**
- `wait_for_ready` (Boolean) Wait for the database to finish provisioning before the apply completes. On by default, because anything that depends on `endpoint` needs one that answers. Set to `false` to return as soon as the work is accepted.

### Read-Only

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
