# Every cloud account connected in the KiviDB console.
data "kividb_cloud_accounts" "all" {}

output "verified_cloud_accounts" {
  value = [for a in data.kividb_cloud_accounts.all.cloud_accounts : a.name if a.status == "verified"]
}
