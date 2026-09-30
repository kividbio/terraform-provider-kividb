# Read a database this configuration does not manage -- to depend on its
# endpoint, or to check what it is before changing something next to it.
data "kividb_instance" "existing" {
  id = "790df3ae-c064-45f5-b924-9ed89471fd72"
}

output "existing_endpoint" {
  value = data.kividb_instance.existing.endpoint
}
