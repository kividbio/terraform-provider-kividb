# An AWS account connected in the KiviDB console, looked up by the name it was
# given there. Use `id` instead if two accounts share a name.
data "kividb_cloud_account" "production" {
  name = "production"
}

output "cloud_account_regions" {
  value = data.kividb_cloud_account.production.regions
}

# An Azure subscription is looked up the same way. Its tenant, subscription and
# resource group are read back with it.
data "kividb_cloud_account" "emea" {
  name = "emea"
}

output "azure_resource_group" {
  value = data.kividb_cloud_account.emea.azure_resource_group
}
