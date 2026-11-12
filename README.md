# terraform-provider-latchkey

Manage a [Latchkey](https://latchkey.id) organization's configuration as
code: OIDC clients, membership grants, custom auth domains, hosted-page
branding, mail templates and federation targets — everything the org API
exposes.

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
  rp_id  = "acme.com"      # optional: passkey scope — the apex, so passkeys
                           # enrolled on your own apps keep working here
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

# The org's environment sandboxes — acme-sandbox (the default name) and
# acme-staging — and their contents through a second provider alias:
# production's service client is authorized there. Destroy deletes one.
resource "latchkey_org_sandbox" "env" {}

resource "latchkey_org_sandbox" "staging" {
  name = "staging"
}

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
`LATCHKEY_ORG`, `LATCHKEY_CLIENT_ID`, `LATCHKEY_CLIENT_SECRET`,
`LATCHKEY_TOKEN`.

### Authenticating with a token

A caller that has already signed in to Latchkey — a workload holding a
short-lived access token, with no client secret to give — hands the
provider that token instead of a client:

```hcl
provider "latchkey" {
  issuer = "https://auth.latchkey.id"
  org    = "acme"
  token  = var.latchkey_token # sensitive; or LATCHKEY_TOKEN
}
```

- With a token, `client_id` and `client_secret` are not required, and
  the provider never calls `/oauth/token`: the token is sent as it is.
- **The provider does not renew the token.** It must stay valid until
  the plan or apply finishes; one that expires midway fails the calls
  after it, and the run must be started again with a fresh token.
- `token` is exclusive with `client_id` and `client_secret`, from the
  provider block or the environment. Setting both is a configuration
  error — the provider does not guess which credential was meant.
- The token can do what its holder can: Latchkey authorizes each call
  as it would for that caller anywhere else.
- `token` is sensitive, and the provider never puts it in a
  diagnostic, a log line or an error.

With no token, nothing changes: `client_id` and `client_secret` are
required and the provider signs in with them.

## Resources

| Resource | Manages | On destroy |
| --- | --- | --- |
| `latchkey_org` | **Estate-level:** a product organization created through the estate (the account above orgs) — `slug`, `display_name`, `owner_email`, all fixed at creation, and its anythink tenant: `tenant` (an existing house tenant the owner administers — key, ref or slug) or `new_tenant` (create one), read back into `tenant_key` / `tenant_ref` / `tenant_slug`. Born claimed, owned, and holding a confidential `terraform` client whose `terraform_client_id` + sensitive `terraform_client_secret` (returned once) configure a provider on the new org. Needs estate-owner credentials (the house's client under the `latchkey` org), not a product's | Forgets it from state, with a warning — Terraform never deletes an org (a platform staff act) |
| `latchkey_client` | An OIDC client and its flags (redirect URIs, phone, captcha, attestation, OTP ceiling) | Disables — client ids are never reused |
| `latchkey_branding` | Hosted-page dress: logo URL, colours, tagline, backdrop style | Restores the plain default card |
| `latchkey_mail_template` | Per-org email copy (plaintext + optional HTML part) for one kind: `login`, `login_code`, `invite`, `link_email` or `tenant_invite` (the tenant invitation email) | Restores the default copy |
| `latchkey_auth_domain` | A product-branded issuer host | Releases the claim |
| `latchkey_org_sandbox` | One of the org's named environment sandboxes (`{org}-{name}`; `name` defaults to `sandbox`, the one sandbox an org had before names); manage its contents with a provider alias on `slug` | **Deletes** the sandbox org and everything in it (a sandbox is disposable) |
| `latchkey_grant` | One identity's membership in the org | Revokes the membership |
| `latchkey_webhook` | Signed webhook endpoint: `url` (keyed — a change replaces), sensitive `secret`, optional `events` filter (`invitation.*`, `membership.set`, …). Runtime deliveries stay in the console | Disables the endpoint |
| `latchkey_github_signin` | Continue with GitHub on the hosted sign-in pages: `mode` `platform` (borrow Latchkey's OAuth app) or `custom` (`client_id` + sensitive `client_secret`, write-only) | Removes the button |
| `latchkey_github_app` | The org's GitHub App for the GitHub proxy: `app_id` + sensitive `private_key` (write-only; parsed when set). Products then call GitHub through `/org/{slug}/github/as-app` and `as-installation` without holding the key | Proxy calls as the App refuse |
| `latchkey_federation_target` | A tenant's federation target — the trust at a cloud that Latchkey's short-lived subject tokens are traded on: `cloud` `gcp` (workload identity provider path, service account, scopes), `aws` (role, region, session policy, duration), `azure` (directory, client, scope, hosts) or `oidc` (token endpoint or a direct target, hosts). Holds no secret; everything but the grants updates in place. Reads back `target_ref`, `audience` and, for GCP, `principal_set`, `principal` and `attribute_condition`, so the cloud's side of the trust is declared in the same config. The org must hold the federation feature. Import by `org/tenant/name` | **Deletes — final: the name stays taken**, so a stale ref never names a new trust |
| `latchkey_federation_target_grant` | One product's grant on a target: `purposes` (at least one), `kinds` (the job kinds whose sessions may mint; `*` is all; none is the product's own workload only) and `environments` (none is any). Replaced in place. Import by `org/tenant/target/product` | Revokes the grant; the target stays |

Data source: `latchkey_org` — the org's identity and configured-ness
(secrets never appear in any API response).

## The estate: creating orgs as code

A second provider, configured with the **estate's** own credentials — a
confidential client under the `latchkey` org that an estate owner has
added over `POST /platform/estate/owners` — creates product orgs. Each
arrives with its default `terraform` client, so the org's own provider
can be configured from the same apply and nothing is ever clicked in
the console:

```hcl
provider "latchkey" {
  alias  = "estate"
  issuer = "https://auth.latchkey.id"
  org    = "latchkey"
  # LATCHKEY_CLIENT_ID / LATCHKEY_CLIENT_SECRET: the estate's client
}

resource "latchkey_org" "wardroom" {
  provider     = latchkey.estate
  slug         = "wardroom"
  display_name = "Wardroom"
  owner_email  = "captain@wardroom.example"
}

provider "latchkey" {
  alias         = "wardroom"
  issuer        = "https://auth.latchkey.id"
  org           = latchkey_org.wardroom.slug
  client_id     = latchkey_org.wardroom.terraform_client_id
  client_secret = latchkey_org.wardroom.terraform_client_secret
}
```

`slug`, `display_name` and `owner_email` cannot change once created:
Latchkey has no org rename or delete, and a plan that changes them is
refused rather than pretending to replace an org that cannot be
destroyed. Import an existing org by slug; the secret is shown once at
creation and stays null after import.

### Orgs follow anythink tenants (ADR 012)

Every production org is **bound** to one tenant of the house
(anythink) org — the customer it serves; the tenant's admins are the
org's owners. Name the tenant at creation:

```hcl
resource "latchkey_org" "wardroom" {
  provider     = latchkey.estate
  slug         = "wardroom"
  display_name = "Wardroom"
  owner_email  = "captain@wardroom.example"
  tenant       = "plygmjohrbtnkxnlsehy" # key, ref or slug; the owner administers it
  # or: new_tenant = { display_name = "Wardroom" } — created first, the owner its admin
}
```

`tenant` and `new_tenant` are exclusive. Once Latchkey enforces bound
orgs, a create naming neither is refused with a diagnostic saying so.
`tenant` keeps the spelling you wrote: a ref or slug that resolves to
the bound tenant plans nothing (`tenant_key` holds the key). Setting
`tenant` on an org created unbound binds it in place (the provider's
credentials must be the owner's or platform staff's — the estate's
`*`-tagged client is staff); changing it on a bound org is refused at
plan time, because a rebind is a platform staff act, never a
replacement. `new_tenant` is read only at creation. Import reads the
binding into `tenant_key`.

## Federation: one apply wires Latchkey and the cloud

A federation target is a tenant's trust at a cloud. Latchkey holds no
cloud key: it signs a five-minute subject token for a product that
holds a grant on the target, and the cloud trades it for its own
credential. Both halves of that trust — Latchkey's target and grant,
and the cloud's provider and IAM binding — fit in one configuration,
because the target reads back what the cloud's side must name:

```hcl
data "google_project" "this" {}

locals {
  # the provider's resource name: a path, written before the provider exists
  workload_identity_provider = "projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/latchkey/providers/latchkey"
}

resource "google_service_account" "deployer" {
  account_id = "latchkey-deployer"
}

resource "latchkey_federation_target" "deployer" {
  tenant = "shop"
  name   = "gcp-deployer"
  cloud  = "gcp"

  gcp = {
    workload_identity_provider = local.workload_identity_provider
    service_account            = google_service_account.deployer.email
  }
}

resource "latchkey_federation_target_grant" "runsheet" {
  tenant       = latchkey_federation_target.deployer.tenant
  target       = latchkey_federation_target.deployer.name
  product      = "runsheet"
  purposes     = ["deploy"]
  environments = ["production"]
}

resource "google_iam_workload_identity_pool" "latchkey" {
  workload_identity_pool_id = "latchkey"
}

resource "google_iam_workload_identity_pool_provider" "latchkey" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.latchkey.workload_identity_pool_id
  workload_identity_pool_provider_id = "latchkey"

  attribute_mapping = {
    "google.subject"    = "assertion.sub"
    "attribute.target"  = "assertion.target"
    "attribute.tenant"  = "assertion.tenant"
    "attribute.product" = "assertion.product"
  }

  # assertion.tenant == "<the tenant's ref>" && assertion.target == "<target_ref>"
  attribute_condition = latchkey_federation_target.deployer.attribute_condition

  oidc {
    issuer_uri = "https://auth.latchkey.id"
  }
}

resource "google_service_account_iam_member" "deployer" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = latchkey_federation_target.deployer.principal_set
}
```

The complete file, with providers and variables, is
[`examples/federation-gcp/main.tf`](examples/federation-gcp/main.tf).

**There is no chicken-and-egg.** The target does not reference the GCP
provider resource; its `workload_identity_provider` is a path string,
built from the project's number and two ids you choose, so it can be
written before the provider exists. The dependency runs one way: the
GCP provider's condition and the IAM member depend on the target
(`attribute_condition`, `target_ref`, `principal_set`), and the target
depends on nothing at GCP but the service account's email. Do not set
`workload_identity_provider` from
`google_iam_workload_identity_pool_provider.latchkey.name` — that is
the one spelling that makes a cycle. Until the apply finishes, the
target simply names a provider GCP does not have yet, and mints for it
are refused by GCP, not by Latchkey.

Things to know:

- `attribute_condition` is Latchkey's recommendation for a provider
  that admits this one target. The `tenant` claim is the tenant's ref,
  which is not always its slug, so take the condition from the target
  rather than spelling the tenant. To admit several targets through
  one provider, write your own condition from each target's
  `target_ref`.
- `principal` is a pattern, not a member: it ends
  `fed:<target_ref>:<product>:<kind|workload>` with the last two parts
  left as placeholders. `principal_set` is ready to use.
- `audience` defaults to the cloud's own (for GCP the provider's
  canonical name, which a provider with no allowed audiences accepts).
  Set it only if the provider lists `allowed_audiences`.
- `org` defaults to the provider's org on both resources.
- Deleting a target is final and its name stays taken, and Latchkey
  never changes a target's `cloud`. A plan that changes `cloud` under
  the same name is therefore refused with a request for a new `name`,
  rather than destroying a target that could not be set again.
- The provider's credentials must be the org's own service client with
  no `product` and no scope (or the owner's, or staff's). A product's
  client never manages a federation target: Latchkey answers it 403.
- All four clouds are implemented. For `aws`, `azure` and `oidc` the
  provider reads back `target_ref` and `audience` only: the trust
  values for those clouds (the AWS trust policy condition, the Azure
  federated credential's subject) are not exposed as attributes, so
  build them from `target_ref` — a subject is
  `fed:<target_ref>:<product>:<kind|workload>`.
- `POST …/federation-targets/{name}/verify` (one live exchange at the
  cloud) is an action, not desired state, and is not modelled.

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
