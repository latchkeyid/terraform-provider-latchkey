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

# A tenant, its role vocabulary, a team that confers a binding on its
# members, and a direct grant — the whole §13 membership surface as code.
resource "latchkey_tenant" "shop" {
  slug         = "shop"
  display_name = "Shop"
}

resource "latchkey_role" "manager" {
  slug         = "manager"
  display_name = "Manager"
  base_level   = "admin"
}

resource "latchkey_team" "staff" {
  slug         = "shop-staff"
  tenant       = latchkey_tenant.shop.slug
  display_name = "Shop staff"
  bindings = [{
    ns    = "acme/shop"
    level = "member"
  }]
}

resource "latchkey_team_member" "pat" {
  team  = latchkey_team.staff.slug
  email = "pat@acme.test"
}

resource "latchkey_tenant_grant" "lee" {
  tenant = latchkey_tenant.shop.slug
  email  = "lee@acme.test"
  level  = "member"
  roles  = [latchkey_role.manager.slug]
}

# A browser (publishable) key for the shop tenant — embedded in frontend
# code, verified only from these origins. Changing anything replaces the
# key: replacement is rotation.
resource "latchkey_api_key" "storefront" {
  tenant          = latchkey_tenant.shop.slug
  name            = "storefront"
  browser         = true
  allowed_origins = ["https://app.acme.test", "https://*.acme.test"]
}
