---
page_title: "kividb_rest_api Resource"
subcategory: ""
description: |-
  A database's REST API, compatible with Upstash's.
---

# kividb_rest_api (Resource)

A database's REST API: an HTTPS endpoint at `https://<database hostname>`,
compatible with Upstash's REST API. An app on the `@upstash/redis` SDK switches by
setting `UPSTASH_REDIS_REST_URL` to `url` and `UPSTASH_REDIS_REST_TOKEN` to a token.

A token is `kvr_` followed by the base64url (no padding) of `username:password` for a
database user (`kividb_database_user`), so the user's profile decides what it may
run: a `read_only` user gives a read-only token. Changing the user's password
revokes its token within 30 seconds.

Creating this resource turns the endpoint on: KiviDB issues a public
certificate for the database's hostname and opens port 443 to the database's
allow-list. Deleting it turns the endpoint off and closes the port. Available
on dedicated databases that are not sharded.

```terraform
# An HTTPS endpoint for the database, for serverless and edge functions.
resource "kividb_rest_api" "cache" {
  instance_id = kividb_instance.cache.id
}

# A read-only user, whose token is safe to use from a browser.
resource "kividb_database_user" "web" {
  instance_id = kividb_instance.cache.id
  username    = "web"
  profile     = "read_only"
}

# What an app on @upstash/redis sets: UPSTASH_REDIS_REST_URL and
# UPSTASH_REDIS_REST_TOKEN. The token is kvr_ + base64url("user:password").
output "rest_url" {
  value = kividb_rest_api.cache.url
}

output "rest_token_web" {
  value     = "kvr_${replace(replace(replace(base64encode("${kividb_database_user.web.username}:${kividb_database_user.web.password}"), "+", "-"), "/", "_"), "=", "")}"
  sensitive = true
}
```

## Schema

### Required

- `instance_id` (String) The database. **Changing this turns the old database's endpoint off and the new one's on.**

### Optional

- `wait_until_ready` (Boolean) Wait for the certificate and the nodes, so `url` is set when the apply finishes (usually one to two minutes; at most 15). Defaults to `true`.

### Read-Only

- `id` (String) The database's id: a database has one REST API.
- `status` (String) `issuing` while the certificate is issued and pushed, `ready` once the database serves it, `unavailable` if that failed.
- `url` (String) The endpoint, `https://<database hostname>`, once it serves. Empty while it is being set up.

## Import

Import is supported using the following syntax:

```shell
# The id is the database id.
terraform import kividb_rest_api.cache 11111111-2222-4333-8444-555555555555
```
