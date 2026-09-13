# terraform-provider-latchkey

Manage a [Latchkey](https://latchkey.id) organization's configuration as
code: OIDC clients, membership grants, custom auth domains, hosted-page
branding, mail templates, tenants, role definitions, teams, tenant
grants and API keys — everything the org API exposes.

The provider authenticates as the org's **own confidential service
client** (OAuth2 `client_credentials`), so Terraform can do exactly what
the org could do itself and nothing more. No platform credentials, no
staff tokens.

## Usage

```hcl
terraform {
  required_providers {
    latchkey = {
      source = "latchkeyid/latchkey"
    }
  }
}

provider "latchkey" {
  issuer = "https://auth.latchkey.id" # or your custom auth domain
  org    = "acme"
  # client_id / client_secret via LATCHKEY_CLIENT_ID / LATCHKEY_CLIENT_SECRET
}

resource "latchkey_client" "web" {
  name          = "acme-web"
  redirect_uris = ["https://app.acme.com/callback"]
}

resource "latchkey_client" "backend" {
  name   = "acme-backend"
  public = false
  secret = var.backend_client_secret # hashed client-side before it leaves Terraform

  # RFC 8693 token exchange, this client as the actor: turn a live Latchkey
  # token into one an external system trusts (a cloud's OIDC federation,
  # your own API). Omit the block to keep the grant off.
  exchange = {
    audiences = ["acme-cloud-federation"]
    claims    = ["https://aws.amazon.com/tags"]
    subject   = true # may replace `sub` — the shape the trust policy matches
    ttl       = 3600 # ceiling; expires_in may shorten per token
  }
}

resource "latchkey_branding" "this" {
  accent   = "#1a73e8"
  tagline  = "Your neighbourhood, connected."
  bg_fit   = "tile"
  bg_scrim = "soft"
}

resource "latchkey_mail_template" "login" {
  kind    = "login"
  subject = "Sign in to Acme"
  body    = "Click to sign in:\n{{.Link}}\n\nThe link works once and expires in 15 minutes."
}

resource "latchkey_auth_domain" "main" {
  domain = "auth.acme.com" # pair with your DNS record + domain mapping
}

resource "latchkey_grant" "ops" {
  email = "ops@acme.com"
  role  = "member"
}

# Tenants are the units SSO, teams, keys and grants attach to; an
# enterprise is a tenant other tenants point at (one level deep).
resource "latchkey_tenant" "hq" {
  slug         = "mcdonalds"
  display_name = "McDonald's"
  sso_required = true
}

resource "latchkey_tenant" "store" {
  slug         = "store-451"
  display_name = "Store 451"
  parent       = latchkey_tenant.hq.slug
}

# The org's role vocabulary — granting a role confers its base_level at
# token mint.
resource "latchkey_role" "manager" {
  slug         = "manager"
  display_name = "Store manager"
  base_level   = "admin"
}

resource "latchkey_team" "franchisees" {
  slug         = "franchisees"
  tenant       = latchkey_tenant.hq.slug
  display_name = "Franchisee group"
  bindings = [{
    ns    = "acme/store-451"
    level = "member"
    roles = [latchkey_role.manager.slug]
  }]
}

resource "latchkey_team_member" "pat" {
  team  = latchkey_team.franchisees.slug
  email = "pat@acme.com"
}

resource "latchkey_tenant_grant" "lee" {
  tenant = latchkey_tenant.store.slug
  email  = "lee@acme.com"
  level  = "member"
  roles  = [latchkey_role.manager.slug]
}

resource "latchkey_mail_template" "tenant_invite" {
  kind    = "tenant_invite"
  subject = "You're invited to {{.TenantName}} on {{.OrgName}}"
  body    = "Accept your place: {{.Link}}"
}

resource "latchkey_webhook" "product" {
  url    = "https://api.example.com/latchkey/webhooks"
  secret = var.latchkey_webhook_secret # sensitive — never in plan output
  events = ["invitation.*", "membership.set"]
}
```

Every provider attribute falls back to the environment: `LATCHKEY_ISSUER`,
`LATCHKEY_ORG`, `LATCHKEY_CLIENT_ID`, `LATCHKEY_CLIENT_SECRET`.

## Resources

| Resource | Manages | On destroy |
| --- | --- | --- |
| `latchkey_client` | An OIDC client and its flags (redirect URIs, phone, captcha, attestation, OTP ceiling) and, for a confidential client, its RFC 8693 `exchange` allow-list (audiences, claim names, subject override, TTL ceiling) | Disables — client ids are never reused |
| `latchkey_branding` | Hosted-page dress: logo URL, colours, tagline, backdrop style | Restores the plain default card |
| `latchkey_mail_template` | Per-org email copy (plaintext + optional HTML part) for one kind: `login`, `login_code`, `invite`, `link_email` or `tenant_invite` (the tenant invitation email) | Restores the default copy |
| `latchkey_auth_domain` | A product-branded issuer host | Releases the claim |
| `latchkey_grant` | One identity's membership in the org | Revokes the membership |
| `latchkey_api_key` | A tenant API key — secret (`lk_live_`) or publishable (`lk_pk_live_`, `browser = true`; add `allowed_origins` for a web bundle, omit it for an unrestricted key a native app or server SDK embeds). Immutable: any change replaces the key, which is rotation; the plaintext lands in the sensitive `key` attribute | Revokes the key |
| `latchkey_tenant` | A tenant — display name, enterprise `parent` pointer, `sso_required`. Creation is claim-style and convergent | **Archives — one-way, and the slug is never claimable again** |
| `latchkey_role` | A role definition in the org registry: display name, `base_level` (admin/member/viewer conferred at mint), `capabilities` | Retires — confers nothing, refuses new grants; redefining un-retires |
| `latchkey_team` | A team under a tenant and its bindings (what membership confers). Hand-managed only — SCIM-synced teams belong to the IdP | Soft-deletes — the slug stays retired forever |
| `latchkey_team_member` | One identity's place on a hand-managed team (by email; the identity is ensured) | Removes the member |
| `latchkey_tenant_grant` | One identity's grant on a `{org}/{tenant}` namespace: level + registry roles. Org-level roles deliberately stay behind the owner-gated invite flow | Revokes the grant |
| `latchkey_webhook` | Signed webhook endpoint: `url` (keyed — a change replaces), sensitive `secret`, optional `events` filter (`invitation.*`, `membership.set`, …). Runtime deliveries stay in the console | Disables the endpoint |

Data source: `latchkey_org` — the org's identity and configured-ness
(secrets never appear in any API response).

## What stays in the console

Hosted image uploads (logo/background files), provider credentials
(SendGrid, Twilio, Turnstile — write-only secrets), and everything
operational (sessions, logins, the fraud dashboards, webhook deliveries
and redelivery). Tenant invitations are runtime data too — they are
created by products or the console, expire, and grant nothing until the
invitee accepts — so there is deliberately no `latchkey_invitation`
resource; manage the endpoint that hears about them (`latchkey_webhook`)
and the email they send (`latchkey_mail_template` kind `tenant_invite`).

## Development

```sh
go test ./...          # acceptance tests run against a faithful in-memory
                       # stub of the org API, driving the real terraform CLI
LATCHKEY_ISSUER=...    # point the same tests at a live issuer instead
```

## License

[MPL-2.0](LICENSE)
