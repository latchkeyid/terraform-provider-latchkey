# terraform-provider-latchkey

Manage a [Latchkey](https://latchkey.id) organization's configuration as
code: OIDC clients, membership grants, custom auth domains, hosted-page
branding and mail templates — everything the org API exposes.

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

# Continue with GitHub, as the org's own GitHub App (so sign-ins yield
# GitHub App user tokens), and the App itself for the GitHub proxy.
resource "latchkey_github_signin" "this" {
  mode          = "custom"
  client_id     = "Iv1.abc123"
  client_secret = var.github_client_secret # write-only
}

resource "latchkey_github_app" "this" {
  app_id      = "1901048"
  private_key = file("github-app.pem") # write-only; parsed when set
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

# The org's environment sandbox (acme-sandbox), and its contents through a
# second provider alias — production's service client is authorized there.
resource "latchkey_org_sandbox" "env" {}

provider "latchkey" {
  alias  = "sandbox"
  issuer = "https://auth.latchkey.id"
  org    = latchkey_org_sandbox.env.slug
}

resource "latchkey_client" "app_sandbox" {
  provider      = latchkey.sandbox
  name          = "Acme (sandbox)"
  public        = true
  redirect_uris = ["acme://auth/callback"]
}
```

Every provider attribute falls back to the environment: `LATCHKEY_ISSUER`,
`LATCHKEY_ORG`, `LATCHKEY_CLIENT_ID`, `LATCHKEY_CLIENT_SECRET`.

## Resources

| Resource | Manages | On destroy |
| --- | --- | --- |
| `latchkey_client` | An OIDC client and its flags (redirect URIs, phone, captcha, attestation, OTP ceiling) | Disables — client ids are never reused |
| `latchkey_branding` | Hosted-page dress: logo URL, colours, tagline, backdrop style | Restores the plain default card |
| `latchkey_mail_template` | Per-org email copy (plaintext + optional HTML part) for one kind: `login`, `login_code`, `invite`, `link_email` or `tenant_invite` (the tenant invitation email) | Restores the default copy |
| `latchkey_auth_domain` | A product-branded issuer host | Releases the claim |
| `latchkey_org_sandbox` | The org's environment sandbox (`{org}-sandbox`, one per org, no arguments); manage its contents with a provider alias on `slug` | Forgets it from state — Latchkey has no org delete |
| `latchkey_grant` | One identity's membership in the org | Revokes the membership |
| `latchkey_webhook` | Signed webhook endpoint: `url` (keyed — a change replaces), sensitive `secret`, optional `events` filter (`invitation.*`, `membership.set`, …). Runtime deliveries stay in the console | Disables the endpoint |
| `latchkey_github_signin` | Continue with GitHub on the hosted sign-in pages: `mode` `platform` (borrow Latchkey's OAuth app) or `custom` (`client_id` + sensitive `client_secret`, write-only) | Removes the button |
| `latchkey_github_app` | The org's GitHub App for the GitHub proxy: `app_id` + sensitive `private_key` (write-only; parsed when set). Products then call GitHub through `/org/{slug}/github/as-app` and `as-installation` without holding the key | Proxy calls as the App refuse |

Data source: `latchkey_org` — the org's identity and configured-ness
(secrets never appear in any API response).

## What stays in the console

Hosted image uploads (logo/background files), the mail/SMS/captcha
provider credentials (SendGrid, Twilio, Turnstile), and everything
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
