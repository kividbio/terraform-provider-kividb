# Run a KiviDB database in your own AWS account.
#
# Before applying:
#
#   1. Connect the AWS account in the KiviDB console, under
#      Dashboard -> Settings -> Cloud accounts. This is done in the console
#      rather than from Terraform: it grants KiviDB access to the account,
#      which needs a person signed in.
#   2. Enable the region you want for that account, and wait for its status to
#      be "verified".
#   3. export KIVIDB_API_KEY=kvdb_...  (an admin key)
#
# AWS accounts, Google Cloud projects and Azure subscriptions are supported.
# For a Google Cloud project set cloud = "gcp", and for an Azure subscription
# cloud = "azure", with a region enabled for that account.

terraform {
  required_providers {
    kividb = {
      source  = "kividbio/kividb"
      version = "~> 0.2"
    }
  }
}

provider "kividb" {}

variable "cloud_account_name" {
  description = "The name the AWS account, Google Cloud project or Azure subscription was given when it was connected in the KiviDB console."
  type        = string
  default     = "production"
}

data "kividb_cloud_account" "this" {
  name = var.cloud_account_name
}

resource "kividb_instance" "orders" {
  name          = "orders"
  tier          = "pro"
  cloud         = "aws"
  region        = "eu-central-1" # must be one of data.kividb_cloud_account.this.regions
  data_size_gb  = 8
  replica_count = 2
  tls_enabled   = true

  # The database runs in this account. It cannot be moved to another account
  # later: changing this replaces the database, and replacing it destroys its
  # data.
  cloud_account_id = data.kividb_cloud_account.this.id

  lifecycle {
    precondition {
      condition     = contains(data.kividb_cloud_account.this.regions, "eu-central-1")
      error_message = "Enable eu-central-1 for this account in the KiviDB console first."
    }
  }
}

# Hostnames that resolve inside your own network. Connect on the same port as
# the public endpoint.
output "private_endpoint" {
  value = kividb_instance.orders.private_endpoint
}

output "private_readonly_endpoint" {
  value = kividb_instance.orders.private_readonly_endpoint
}

output "private_replica_endpoints" {
  value = kividb_instance.orders.private_replica_endpoints
}
