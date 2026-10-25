# Who may connect over the public endpoint. This is the whole list: anything
# not here -- including the address KiviDB added for whoever created the
# database -- loses access when it is applied.
resource "kividb_instance_network" "orders" {
  instance_id = kividb_instance.orders.id

  allowlist = [
    { cidr = "203.0.113.0/24", label = "office" },
    { cidr = "198.51.100.17/32", label = "ci runner" },
  ]
}

output "connect_on" {
  value = kividb_instance_network.orders.port
}
