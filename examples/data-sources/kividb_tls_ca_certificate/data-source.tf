# The CA a client trusts when it connects to a database with TLS.
data "kividb_tls_ca_certificate" "kividb" {
  instance_id = kividb_instance.cache.id
}

# For example, hand it to an application as a file.
resource "local_file" "kividb_ca" {
  filename = "${path.module}/kividb-ca.pem"
  content  = data.kividb_tls_ca_certificate.kividb.certificate_pem
}
