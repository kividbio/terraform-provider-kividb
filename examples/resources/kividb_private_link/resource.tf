# Private connectivity for a database on AWS: $25 a month, from creation until
# destroy. Only the listed AWS accounts may create an endpoint against it.
resource "kividb_private_link" "orders" {
  instance_id        = kividb_instance.orders.id
  allowed_principals = ["arn:aws:iam::123456789012:root"]

  # Once your endpoint works, close the public endpoint entirely.
  public_access_blocked = false
}

# Your side: an interface endpoint in your VPC, in the same region.
resource "aws_vpc_endpoint" "kividb" {
  vpc_id              = var.vpc_id
  service_name        = kividb_private_link.orders.service_name
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.subnet_ids
  security_group_ids  = [var.security_group_id]
  private_dns_enabled = false
}
