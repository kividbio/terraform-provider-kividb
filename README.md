# Terraform Provider for KiviDB Cloud

Manage KiviDB databases from Terraform.

```hcl
terraform {
  required_providers {
    kividb = {
      source  = "kividbio/kividb"
      version = "~> 0.1"
    }
  }
}

provider "kividb" {}

resource "kividb_instance" "cache" {
  name         = "orders-cache"
  tier         = "essentials"
  cloud        = "aws"
  region       = "us-east-1"
  data_size_gb = 8
}
```

```console
$ export KIVIDB_API_KEY=kvdb_live_…
$ terraform apply
```

## Authentication

Mint an API key in the console under **Dashboard → API keys** and put it in
`KIVIDB_API_KEY`. The key carries the organization and the role, so there is
nothing else to configure.

Prefer the environment variable over `api_key` in a provider block: a key
written into a `.tf` file ends up in version control, and one passed as a
variable ends up in state.

A key needs the **admin** role to create or change databases. A `member` key can
read but not modify, which is what a monitoring or reporting configuration
should use.

## What changes in place, and what replaces the database

KiviDB changes a running database through a small set of separate operations,
and this provider mirrors them exactly rather than pretending everything is
editable.

| Attribute | Effect of changing it |
|---|---|
| `name` | renamed in place |
| `data_size_gb`, `throughput_mode` | resized in place |
| `replica_count` | scaled in place, 1–3 |
| `tier` | upgraded in place |
| `kividb_version`, `lua_enabled`, `tls_enabled` | applied by a rolling restart |
| `cloud`, `region`, `aof_enabled`, `tls_only` | **replaces the database** |

The last row is the one to read carefully. There is no operation that moves a
database between clouds or regions, or that turns append-only persistence on
after the fact. Terraform will plan a destroy and create, which **destroys the
data**. The plan says so before anything happens:

```
  # kividb_instance.cache must be replaced
      ~ region = "us-east-1" -> "eu-west-1" # forces replacement
Plan: 1 to add, 0 to change, 1 to destroy.
```

A few refusals are worth knowing about before you meet them:

- **Replicas do not scale to zero.** Going from replicated to single-node
  changes the shape of the database rather than its size. Scale between 1 and 3,
  or create a new database without replicas and migrate.
- **Free-tier databases cannot be managed here.** The free tier is one per
  person and is created from the console while signed in; an API key has no
  person behind it.
- **A resize needs a settled database.** If it is still provisioning, the API
  says so and the apply fails with that message rather than a status code.

## Waiting

By default an apply waits for the database to finish provisioning, because
anything referring to `endpoint` needs one that answers. Set
`wait_for_ready = false` to return as soon as the work is accepted.

## Importing

```console
$ terraform import kividb_instance.cache <instance-id>
```

## Development

```console
$ go build ./...
$ go test ./...
```

To try a local build against a running control plane, point Terraform at a
filesystem mirror:

```hcl
# ~/.terraformrc
provider_installation {
  filesystem_mirror {
    path    = "/path/to/plugins"
    include = ["registry.terraform.io/kividbio/*"]
  }
  direct { exclude = ["registry.terraform.io/kividbio/*"] }
}
```

```console
$ go build -o /path/to/plugins/registry.terraform.io/kividbio/kividb/0.1.0/<os>_<arch>/terraform-provider-kividb_v0.1.0
$ KIVIDB_API_ENDPOINT=http://127.0.0.1:3000 terraform plan
```

## Licence

Mozilla Public License 2.0. See [LICENSE](LICENSE).

## Snapshots

A `kividb_disk_snapshot` is a point-in-time copy of a database's data volume.

```hcl
resource "kividb_disk_snapshot" "nightly" {
  instance_id = kividb_instance.cache.id
  label       = "before-migration"
}
```

The label is the only thing about a snapshot you can change afterwards. Pointing
`instance_id` at a different database plans a new snapshot and destroys this
one, because nothing re-points a copy that has already been taken.

By default the apply waits for the copy to finish, since a snapshot still being
written cannot be restored from. Set `wait_for_ready = false` to return as soon
as the server accepts it.
