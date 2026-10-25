# Customers stored as hashes under customer:<id>, and their orders as JSON under
# order:<id>. The model indexes them and gives an agent tools to search each
# and to follow a customer to their orders.
resource "kividb_context_model" "shop" {
  instance_id = kividb_instance.cache.id
  name        = "shop"
  description = "The shop's customers and their orders."

  entities = [
    {
      name        = "customer"
      description = "A customer of the shop."
      prefix      = "customer:"
      fields = [
        { name = "name", type = "text", description = "Full name" },
        { name = "tenant", type = "tag" },
        { name = "plan", type = "tag" },
      ]
      relationships = [
        { name = "orders", entity = "order", field = "customer_id" },
      ]
    },
    {
      name        = "order"
      description = "An order a customer placed."
      prefix      = "order:"
      storage     = "json"
      id_field    = "order_id"
      fields = [
        { name = "customer_id", type = "tag" },
        { name = "total", type = "numeric", description = "Order total in cents" },
      ]
    },
  ]
}

# Destroying the model deletes its indexes and revokes its agent keys. The
# customer: and order: records stay in the database.
