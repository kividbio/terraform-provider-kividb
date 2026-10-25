resource "kividb_export_destination_verification" "backups" {
  destination_id = kividb_export_destination.backups.id

  # Verify again whenever the role's policies change.
  triggers = {
    trust      = aws_iam_role.kividb_exports.assume_role_policy
    permission = aws_iam_role_policy.kividb_exports.policy
  }

  # After the role and its policy exist; IAM may still take a minute to make
  # the role assumable, and the check is retried for up to five minutes.
  depends_on = [aws_iam_role_policy.kividb_exports]
}
