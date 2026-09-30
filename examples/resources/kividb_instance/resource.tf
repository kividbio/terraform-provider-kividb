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

# A replicated database. Pro starts at 4 GB on AWS and 8 GB on Azure, because
# Azure has no 1-vCPU sustained-performance VM.
resource "kividb_instance" "primary" {
  name          = "orders-primary"
  tier          = "pro"
  cloud         = "azure"
  region        = "swedencentral"
  data_size_gb  = 8
  replica_count = 2
  tls_enabled   = true
}
