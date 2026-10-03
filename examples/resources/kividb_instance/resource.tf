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
