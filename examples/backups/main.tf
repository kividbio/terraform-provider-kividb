# Deliver a KiviDB database's KDB exports into an S3 bucket of your own.
#
# Before applying:
#
#   1. export KIVIDB_API_KEY=kvdb_...  (an admin key, not limited to some
#      databases: a limited key cannot reach export destinations at all)
#   2. AWS credentials for the account the bucket goes in, allowed to create a
#      bucket, an IAM role and a role policy.
#   3. The id of the database to export. A free-tier database cannot be created
#      by Terraform (they are one per person, made in the console), so this
#      takes one that exists. Exports on demand work on a free-tier database on
#      AWS; scheduled exports do not run on one.

terraform {
  required_providers {
    kividb = {
      source  = "kividbio/kividb"
      version = "~> 0.6"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "kividb" {}

provider "aws" {
  region = "eu-central-1"
}

variable "instance_id" {
  description = "The database to export: its id, from the KiviDB console."
  type        = string
}

variable "bucket" {
  description = "A globally unique name for the bucket exports are delivered to."
  type        = string
}

data "aws_caller_identity" "current" {}

data "kividb_instance" "db" {
  id = var.instance_id
}

# --- the bucket, in your account ---------------------------------------------

resource "aws_s3_bucket" "exports" {
  bucket = var.bucket
}

resource "aws_s3_bucket_public_access_block" "exports" {
  bucket                  = aws_s3_bucket.exports.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Exports accumulate; keep a month of them.
resource "aws_s3_bucket_lifecycle_configuration" "exports" {
  bucket = aws_s3_bucket.exports.id
  rule {
    id     = "expire-kividb-exports"
    status = "Enabled"
    filter {
      prefix = "kividb/"
    }
    expiration {
      days = 30
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

# --- the destination, and the role KiviDB writes with -------------------------

resource "kividb_export_destination" "this" {
  name           = "terraform-${var.bucket}"
  aws_account_id = data.aws_caller_identity.current.account_id
  bucket         = aws_s3_bucket.exports.id
  prefix         = "kividb/"
}

resource "aws_iam_role" "kividb_exports" {
  name               = kividb_export_destination.this.role_name
  assume_role_policy = kividb_export_destination.this.trust_policy
}

resource "aws_iam_role_policy" "kividb_exports" {
  role   = aws_iam_role.kividb_exports.id
  name   = "kividb-exports"
  policy = kividb_export_destination.this.permission_policy
}

resource "kividb_export_destination_verification" "this" {
  destination_id = kividb_export_destination.this.id
  triggers = {
    trust      = aws_iam_role.kividb_exports.assume_role_policy
    permission = aws_iam_role_policy.kividb_exports.policy
  }
  depends_on = [aws_iam_role_policy.kividb_exports, aws_s3_bucket_public_access_block.exports]
}

# --- the database's exports go there -----------------------------------------

resource "kividb_instance_export" "db" {
  instance_id           = data.kividb_instance.db.id
  export_destination_id = kividb_export_destination_verification.this.id
  # "daily" or "weekly" exports on a schedule -- but not on a free-tier
  # database, where a schedule is saved and never runs (the plan warns).
  cadence = "off"
}

# One export now. It is written to KiviDB's storage, then copied into the
# bucket under kividb/ shortly after the apply returns.
resource "kividb_kdb_snapshot" "now" {
  instance_id = data.kividb_instance.db.id
  label       = "terraform-check"
  depends_on  = [kividb_instance_export.db]
}

data "kividb_kdb_snapshots" "db" {
  instance_id = data.kividb_instance.db.id
  status      = "ready"
  depends_on  = [kividb_kdb_snapshot.now]
}

output "verification_status" {
  value = kividb_export_destination_verification.this.status
}

output "export" {
  value = {
    id              = kividb_kdb_snapshot.now.id
    size_bytes      = kividb_kdb_snapshot.now.size_bytes
    delivery_status = kividb_kdb_snapshot.now.delivery_status
  }
}

output "ready_exports" {
  value = [for s in data.kividb_kdb_snapshots.db.snapshots : s.id]
}
