# Changelog

## 0.5.0 (Unreleased)

### Added

- Azure subscriptions as cloud accounts. `kividb_cloud_account` and
  `kividb_cloud_accounts` read Azure connections and report
  `azure_tenant_id`, `azure_subscription_id`, `azure_resource_group`,
  `azure_snapshot_account` and `azure_snapshot_container` (null for AWS
  accounts and Google Cloud projects). A `kividb_instance` with
  `cloud = "azure"` can run in one through `cloud_account_id`.
- `kividb_instance.restore_from_snapshot_id` and
  `kividb_instance.restore_from_kdb_snapshot_id`: create a database from a disk
  snapshot or a KDB snapshot. A snapshot is restored only in its own cloud,
  region and cloud account. When the snapshot's id is known at plan time, the
  plan shows the account the database will be placed in and refuses a
  different cloud, region or account; otherwise the API refuses it at apply,
  and the error names the value to set.
- `cloud_account_name` on `kividb_instance` and the `kividb_instance` data
  source: the connected account's name as the console shows it.
- `cloud_account_id` and `cloud_account_name` on `kividb_disk_snapshot`: the
  account the snapshot is stored in, and the only one it can be restored into.

### Changed

- `kividb_instance.cloud_account_id` is now optional and computed. Left unset
  on a database created from a snapshot, it is the snapshot's account. Left
  unset on any other new database, it is still KiviDB's cloud. Removing it from
  the configuration of an existing database no longer plans a replacement into
  KiviDB's cloud: the database stays in the account it is in. Setting it to a
  different account still replaces the database, and the same id written in a
  different case no longer does.
- The cloud account `status` is documented as `verified`, `pending` or
  `broken`, which is what the API reports.
