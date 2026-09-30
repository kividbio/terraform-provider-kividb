terraform {
  required_providers {
    kividb = {
      source  = "kividbio/kividb"
      version = "~> 0.2"
    }
  }
}

# The API key comes from KIVIDB_API_KEY in the environment. Mint one in the
# console under Dashboard -> API keys, with the admin role: a member key can
# read but not modify.
#
# Prefer the environment variable over `api_key` here. A key written into a .tf
# file ends up in version control, and a key passed as a variable ends up in
# state.
provider "kividb" {}
