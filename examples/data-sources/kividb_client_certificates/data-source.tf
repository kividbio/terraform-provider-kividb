# How a database checks client certificates, and when its CA expires.
data "kividb_client_certificates" "payments" {
  instance_id = kividb_instance.payments.id
}

output "client_ca_expires" {
  value = [for c in data.kividb_client_certificates.payments.ca_certificates : "${c.subject}: ${c.not_after}"]
}
