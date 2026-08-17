terraform {
  required_providers {
    latchkey = {
      source = "latchkeyid/latchkey"
    }
  }
}

provider "latchkey" {
  issuer = "http://localhost:8100" # local dev latchkey
  org    = "acme"
}

resource "latchkey_client" "web" {
  name          = "acme-web"
  redirect_uris = ["https://app.acme.test/cb"]
}

resource "latchkey_branding" "this" {
  accent  = "#1a73e8"
  tagline = "Managed as code."
}
