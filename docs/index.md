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
```

Every provider attribute falls back to the environment: `LATCHKEY_ISSUER`,
`LATCHKEY_ORG`, `LATCHKEY_CLIENT_ID`, `LATCHKEY_CLIENT_SECRET`.

## Resources

| Resource | Manages | On destroy |
| --- | --- | --- |
| `latchkey_client` | An OIDC client and its flags (redirect URIs, phone, captcha, attestation, OTP ceiling) | Disables — client ids are never reused |
| `latchkey_branding` | Hosted-page dress: logo URL, colours, tagline, backdrop style | Restores the plain default card |
| `latchkey_mail_template` | Per-org login/invite email copy (plaintext + optional HTML part) | Restores the default copy |
| `latchkey_auth_domain` | A product-branded issuer host | Releases the claim |
| `latchkey_grant` | One identity's membership in the org | Revokes the membership |

Data source: `latchkey_org` — the org's identity and configured-ness
(secrets never appear in any API response).

## What stays in the console

Hosted image uploads (logo/background files), provider credentials
(SendGrid, Twilio, Turnstile — write-only secrets), and everything
operational (sessions, logins, the fraud dashboards).

## Development

```sh
go test ./...          # acceptance tests run against a faithful in-memory
                       # stub of the org API, driving the real terraform CLI
LATCHKEY_ISSUER=...    # point the same tests at a live issuer instead
```

## License

[MPL-2.0](LICENSE)
