# A bucket in your own AWS account that KDB exports are delivered to.
resource "kividb_export_destination" "backups" {
  name           = "backups"
  aws_account_id = "123456789012"
  bucket         = "acme-kividb-exports"
  prefix         = "kividb/"
}

# The role KiviDB assumes to write into the bucket, built from the
# destination's own policies (hashicorp/aws provider).
resource "aws_iam_role" "kividb_exports" {
  name               = kividb_export_destination.backups.role_name
  assume_role_policy = kividb_export_destination.backups.trust_policy
}

resource "aws_iam_role_policy" "kividb_exports" {
  role   = aws_iam_role.kividb_exports.id
  name   = "kividb-exports"
  policy = kividb_export_destination.backups.permission_policy
}

# Nothing is delivered until the destination passes verification.
resource "kividb_export_destination_verification" "backups" {
  destination_id = kividb_export_destination.backups.id
  depends_on     = [aws_iam_role_policy.kividb_exports]
}
