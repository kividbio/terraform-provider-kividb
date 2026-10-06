# An AWS account connected in the KiviDB console, looked up by the name it was
# given there. Use `id` instead if two accounts share a name.
data "kividb_cloud_account" "production" {
  name = "production"
}

output "cloud_account_regions" {
  value = data.kividb_cloud_account.production.regions
}
