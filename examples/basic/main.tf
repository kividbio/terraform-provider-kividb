terraform {
  required_providers {
    kividb = {
      source  = "kividbio/kividb"
      version = "~> 0.1"
    }
  }
}

# The API key comes from KIVIDB_API_KEY in the environment. Mint one in the
# console under Dashboard -> API keys, with the admin role.
provider "kividb" {}

resource "kividb_instance" "cache" {
  name         = "orders-cache"
  tier         = "essentials"
  cloud        = "aws"
  region       = "us-east-1"
  data_size_gb = 8
}

# Growing the database is an in-place change: raise data_size_gb and apply.
# Moving it to another region is not -- Terraform will plan a replacement, and
# replacing a database destroys its data. Read the plan.

output "endpoint" {
  value = kividb_instance.cache.endpoint
}
