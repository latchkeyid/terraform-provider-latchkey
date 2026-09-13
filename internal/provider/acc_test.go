package provider

import (
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Acceptance tests drive the real terraform CLI against the stub org API
// (or a live issuer when LATCHKEY_ISSUER is set in the environment).

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"latchkey": providerserver.NewProtocol6WithError(New("test")()),
}

// testIssuer arranges the environment: the stub unless a real issuer is
// provided, always with credentials wired.
func testIssuer(t *testing.T) {
	t.Helper()
	if os.Getenv("LATCHKEY_ISSUER") == "" {
		srv := newStub("acme")
		t.Cleanup(srv.Close)
		t.Setenv("LATCHKEY_ISSUER", srv.URL)
		t.Setenv("LATCHKEY_ORG", "acme")
		t.Setenv("LATCHKEY_CLIENT_ID", "00000000-0000-0000-0000-000000000001")
		t.Setenv("LATCHKEY_CLIENT_SECRET", "stub-secret")
	}
	t.Setenv("TF_ACC", "1")
}

func TestAccClient(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_client" "web" {
  name          = "acme-web"
  redirect_uris = ["https://app.acme.test/cb"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("latchkey_client.web", "id"),
					resource.TestCheckResourceAttr("latchkey_client.web", "public", "true"),
					resource.TestCheckResourceAttr("latchkey_client.web", "redirect_uris.0", "https://app.acme.test/cb"),
					resource.TestCheckResourceAttr("latchkey_client.web", "otp_daily_ceiling", "0"),
				),
			},
			{
				// in-place updates: rename, new URI set, flags, ceiling
				Config: `
resource "latchkey_client" "web" {
  name              = "acme-web-app"
  redirect_uris     = ["https://app.acme.test/cb", "https://app.acme.test/cb2"]
  captcha_required  = true
  otp_daily_ceiling = 250
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_client.web", "name", "acme-web-app"),
					resource.TestCheckResourceAttr("latchkey_client.web", "redirect_uris.#", "2"),
					resource.TestCheckResourceAttr("latchkey_client.web", "captcha_required", "true"),
					resource.TestCheckResourceAttr("latchkey_client.web", "otp_daily_ceiling", "250"),
				),
			},
			{
				ResourceName:            "latchkey_client.web",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
		},
	})
}

// TestAccClientExchange: the `exchange` block configures a confidential
// client as an RFC 8693 actor, changes in place, and is the grant
// switched off when removed — with state converging to null, not `[]`.
func TestAccClientExchange(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_client" "backend" {
  name   = "acme-backend"
  public = false
  secret = "s3cret"
  exchange = {
    audiences = ["runsheet-chippy-cloud"]
    claims    = ["https://aws.amazon.com/tags"]
    subject   = true
    ttl       = 3600
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.audiences.0", "runsheet-chippy-cloud"),
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.claims.0", "https://aws.amazon.com/tags"),
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.subject", "true"),
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.ttl", "3600"),
				),
			},
			{
				// in place: a second audience, no claims, defaults for the rest
				Config: `
resource "latchkey_client" "backend" {
  name   = "acme-backend"
  public = false
  secret = "s3cret"
  exchange = {
    audiences = ["cloud", "runsheet-chippy-cloud"]
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.audiences.#", "2"),
					resource.TestCheckNoResourceAttr("latchkey_client.backend", "exchange.claims.#"),
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.subject", "false"),
					resource.TestCheckResourceAttr("latchkey_client.backend", "exchange.ttl", "0"),
				),
			},
			{
				// removed: the grant is off and the block is absent from state
				Config: `
resource "latchkey_client" "backend" {
  name   = "acme-backend"
  public = false
  secret = "s3cret"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr("latchkey_client.backend", "exchange.audiences.#"),
				),
			},
		},
	})
}

func TestAccBranding(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_branding" "this" {
  accent   = "#1a73e8"
  tagline  = "Managed as code."
  bg_fit   = "tile"
  bg_scrim = "none"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_branding.this", "accent", "#1a73e8"),
					resource.TestCheckResourceAttr("latchkey_branding.this", "bg_fit", "tile"),
					resource.TestCheckResourceAttr("latchkey_branding.this", "bg_scrim", "none"),
					resource.TestCheckResourceAttr("latchkey_branding.this", "bg_position", "center"),
				),
			},
			{
				Config: `
resource "latchkey_branding" "this" {
  accent  = "#0f2e1d"
  tagline = "Managed as code."
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_branding.this", "accent", "#0f2e1d"),
					resource.TestCheckResourceAttr("latchkey_branding.this", "bg_fit", "cover"),
				),
			},
		},
	})
}

func TestAccMailTemplate(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_mail_template" "login" {
  kind    = "login"
  subject = "Sign in to Acme"
  body    = "Click to sign in:\n{{.Link}}"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_mail_template.login", "id", "login"),
					resource.TestCheckResourceAttr("latchkey_mail_template.login", "subject", "Sign in to Acme"),
				),
			},
			{
				Config: `
resource "latchkey_mail_template" "login" {
  kind    = "login"
  subject = "Sign in"
  body    = "Click to sign in:\n{{.Link}}"
  html    = "<p><a href=\"{{.Link}}\">Click to sign in</a></p>"
}`,
				// the html part must carry the anchor too — the live API
				// refuses an html alternative without {{.Link}}
				Check: resource.TestCheckResourceAttr("latchkey_mail_template.login", "html", `<p><a href="{{.Link}}">Click to sign in</a></p>`),
			},
		},
	})
}

// every kind the org API accepts plans and applies; tenant_invite is the
// tenant invitation email (its accept link is injected by the service)
func TestAccMailTemplateKinds(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				// the validator names every accepted kind, tenant_invite included
				PlanOnly: true,
				Config: `
resource "latchkey_mail_template" "nope" {
  kind    = "invitation"
  subject = "x"
  body    = "y"
}`,
				ExpectError: regexp.MustCompile(`tenant_invite`),
			},
			{
				// the org API renders every part with missingkey=error over
				// its allowlist — a placeholder outside it ({{.Tenant}} is
				// the classic slip for {{.TenantName}}) is a 400 at apply
				// whose message lists the variables that do exist
				Config: `
resource "latchkey_mail_template" "tenant_invite" {
  kind    = "tenant_invite"
  subject = "You're invited to {{.Tenant}}"
  body    = "Accept your place: {{.Link}}"
}`,
				ExpectError: regexp.MustCompile(`\{\{\.TenantName\}\}`),
			},
			{
				Config: `
resource "latchkey_mail_template" "tenant_invite" {
  kind    = "tenant_invite"
  subject = "You're invited to {{.TenantName}} on {{.OrgName}}"
  body    = "Accept your place as {{.Role}}: {{.Link}}"
}

resource "latchkey_mail_template" "login_code" {
  kind    = "login_code"
  subject = "Your Acme code"
  body    = "Code: {{.Code}}"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_mail_template.tenant_invite", "id", "tenant_invite"),
					resource.TestCheckResourceAttr("latchkey_mail_template.tenant_invite", "kind", "tenant_invite"),
					resource.TestCheckResourceAttr("latchkey_mail_template.login_code", "id", "login_code"),
				),
			},
		},
	})
}

func TestAccAuthDomain(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_auth_domain" "main" {
  domain = "auth.acme.test"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_auth_domain.main", "id", "auth.acme.test"),
					resource.TestCheckResourceAttr("latchkey_auth_domain.main", "issuer", "https://auth.acme.test"),
					resource.TestCheckNoResourceAttr("latchkey_auth_domain.main", "rp_id"),
				),
			},
			{
				// passkey scope widened to the apex in place (a re-claim upstream)
				Config: `
resource "latchkey_auth_domain" "main" {
  domain = "auth.acme.test"
  rp_id  = "acme.test"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_auth_domain.main", "id", "auth.acme.test"),
					resource.TestCheckResourceAttr("latchkey_auth_domain.main", "rp_id", "acme.test"),
				),
			},
			{
				// a second apply converges
				Config: `
resource "latchkey_auth_domain" "main" {
  domain = "auth.acme.test"
  rp_id  = "acme.test"
}`,
				PlanOnly: true,
			},
			{
				// not the domain or a parent of it: the server refuses
				Config: `
resource "latchkey_auth_domain" "main" {
  domain = "auth.acme.test"
  rp_id  = "elsewhere.test"
}`,
				ExpectError: regexp.MustCompile(`parent of it`),
			},
		},
	})
}

func TestAccOrgSandbox(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "latchkey_org_sandbox" "env" {}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_org_sandbox.env", "id", "acme-sandbox"),
					resource.TestCheckResourceAttr("latchkey_org_sandbox.env", "slug", "acme-sandbox"),
					resource.TestCheckResourceAttr("latchkey_org_sandbox.env", "sandbox_of", "acme"),
				),
			},
			{
				// a second apply converges: the claim is idempotent upstream
				Config:   `resource "latchkey_org_sandbox" "env" {}`,
				PlanOnly: true,
			},
		},
	})
}

func TestAccGrant(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_grant" "ops" {
  email = "ops@acme.test"
  role  = "member"
}`,
				Check: resource.TestCheckResourceAttr("latchkey_grant.ops", "role", "member"),
			},
			{
				Config: `
resource "latchkey_grant" "ops" {
  email = "ops@acme.test"
  role  = "owner"
}`,
				Check: resource.TestCheckResourceAttr("latchkey_grant.ops", "role", "owner"),
			},
		},
	})
}

func TestAccOrgData(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `data "latchkey_org" "this" {}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.latchkey_org.this", "slug"),
					resource.TestCheckResourceAttrSet("data.latchkey_org.this", "display_name"),
				),
			},
		},
	})
}

func TestAccTenant(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_tenant" "hq" {
  slug         = "mcdonalds"
  display_name = "McDonald's"
}

resource "latchkey_tenant" "store" {
  slug         = "store-451"
  display_name = "Store 451"
  parent       = latchkey_tenant.hq.slug
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_tenant.store", "parent", "mcdonalds"),
					resource.TestCheckResourceAttr("latchkey_tenant.store", "sso_required", "false"),
					resource.TestCheckResourceAttr("latchkey_tenant.hq", "id", "mcdonalds"),
				),
			},
			{
				// rename, flip the SSO gate and leave the enterprise — all
				// in place, never a replace (the slug would be burned)
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_tenant.store", plancheck.ResourceActionUpdate),
					},
				},
				Config: `
resource "latchkey_tenant" "hq" {
  slug         = "mcdonalds"
  display_name = "McDonald's"
}

resource "latchkey_tenant" "store" {
  slug         = "store-451"
  display_name = "Store 451 — Riverside"
  sso_required = true
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_tenant.store", "display_name", "Store 451 — Riverside"),
					resource.TestCheckResourceAttr("latchkey_tenant.store", "sso_required", "true"),
					resource.TestCheckNoResourceAttr("latchkey_tenant.store", "parent"),
				),
			},
		},
	})
}

func TestAccRole(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_role" "resident" {
  slug         = "resident"
  display_name = "Resident"
  base_level   = "member"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_role.resident", "id", "resident"),
					resource.TestCheckResourceAttr("latchkey_role.resident", "base_level", "member"),
				),
			},
			{
				// define is an upsert — editing re-levels holders at their
				// next refresh, no replace
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_role.resident", plancheck.ResourceActionUpdate),
					},
				},
				Config: `
resource "latchkey_role" "resident" {
  slug         = "resident"
  display_name = "Building manager"
  base_level   = "admin"
  capabilities = ["manages_tenant"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_role.resident", "display_name", "Building manager"),
					resource.TestCheckResourceAttr("latchkey_role.resident", "capabilities.0", "manages_tenant"),
				),
			},
		},
	})
}

func TestAccTeam(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_tenant" "shop" {
  slug         = "shopfront"
  display_name = "Shopfront"
}

resource "latchkey_role" "committee" {
  slug         = "committee"
  display_name = "Committee"
}

resource "latchkey_team" "east" {
  slug         = "east-region"
  tenant       = latchkey_tenant.shop.slug
  display_name = "East region"
  bindings = [{
    ns    = "acme/shopfront"
    level = "member"
    roles = [latchkey_role.committee.slug]
  }]
}

resource "latchkey_team_member" "ops" {
  team  = latchkey_team.east.slug
  email = "ops@acme.test"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_team.east", "id", "east-region"),
					resource.TestCheckResourceAttr("latchkey_team.east", "bindings.0.ns", "acme/shopfront"),
					resource.TestCheckResourceAttr("latchkey_team.east", "bindings.0.level", "member"),
					resource.TestCheckResourceAttr("latchkey_team.east", "bindings.0.roles.0", "committee"),
					resource.TestCheckResourceAttrSet("latchkey_team_member.ops", "identity_id"),
				),
			},
			{
				// rename + rebind in place; membership stays put
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_team.east", plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction("latchkey_team_member.ops", plancheck.ResourceActionNoop),
					},
				},
				Config: `
resource "latchkey_tenant" "shop" {
  slug         = "shopfront"
  display_name = "Shopfront"
}

resource "latchkey_role" "committee" {
  slug         = "committee"
  display_name = "Committee"
}

resource "latchkey_team" "east" {
  slug         = "east-region"
  tenant       = latchkey_tenant.shop.slug
  display_name = "East + North region"
  bindings = [{
    ns    = "acme/shopfront"
    level = "admin"
  }]
}

resource "latchkey_team_member" "ops" {
  team  = latchkey_team.east.slug
  email = "ops@acme.test"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_team.east", "display_name", "East + North region"),
					resource.TestCheckResourceAttr("latchkey_team.east", "bindings.0.level", "admin"),
				),
			},
		},
	})
}

func TestAccTenantGrant(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_tenant" "home" {
  slug         = "home-123"
  display_name = "Home 123"
}

resource "latchkey_role" "resident2" {
  slug         = "resident2"
  display_name = "Resident"
}

resource "latchkey_tenant_grant" "alice" {
  tenant = latchkey_tenant.home.slug
  email  = "alice@acme.test"
  level  = "member"
  roles  = [latchkey_role.resident2.slug]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("latchkey_tenant_grant.alice", "identity_id"),
					resource.TestCheckResourceAttr("latchkey_tenant_grant.alice", "level", "member"),
					resource.TestCheckResourceAttr("latchkey_tenant_grant.alice", "roles.0", "resident2"),
				),
			},
			{
				// the grant endpoint upserts — level changes in place
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_tenant_grant.alice", plancheck.ResourceActionUpdate),
					},
				},
				Config: `
resource "latchkey_tenant" "home" {
  slug         = "home-123"
  display_name = "Home 123"
}

resource "latchkey_role" "resident2" {
  slug         = "resident2"
  display_name = "Resident"
}

resource "latchkey_tenant_grant" "alice" {
  tenant = latchkey_tenant.home.slug
  email  = "alice@acme.test"
  level  = "admin"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_tenant_grant.alice", "level", "admin"),
					resource.TestCheckNoResourceAttr("latchkey_tenant_grant.alice", "roles"),
				),
			},
		},
	})
}

func TestAccWebhook(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_webhook" "grapevine" {
  url    = "https://api.grapevine.test/latchkey/webhooks"
  secret = "hook-secret-1"
  events = ["invitation.*", "membership.set"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "id", "https://api.grapevine.test/latchkey/webhooks"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "status", "active"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "events.#", "2"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "events.0", "invitation.*"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "events.1", "membership.set"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "secret", "hook-secret-1"),
				),
			},
			{
				// same url: secret rotation + narrower filter update in place
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_webhook.grapevine", plancheck.ResourceActionUpdate),
					},
				},
				Config: `
resource "latchkey_webhook" "grapevine" {
  url    = "https://api.grapevine.test/latchkey/webhooks"
  secret = "hook-secret-2"
  events = ["invitation.*"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "events.#", "1"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "secret", "hook-secret-2"),
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "status", "active"),
				),
			},
			{
				// a new url is a new endpoint — replace, and the old one is
				// disabled; unset events = the whole catalog
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_webhook.grapevine", plancheck.ResourceActionReplace),
					},
				},
				Config: `
resource "latchkey_webhook" "grapevine" {
  url    = "https://api.grapevine.test/latchkey/webhooks/v2"
  secret = "hook-secret-2"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_webhook.grapevine", "id", "https://api.grapevine.test/latchkey/webhooks/v2"),
					resource.TestCheckNoResourceAttr("latchkey_webhook.grapevine", "events"),
				),
			},
		},
	})
}

func TestAccApiKey(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_api_key" "backend" {
  tenant = "shop"
  name   = "ci deploy key"
  scopes = ["properties:read"]
}

resource "latchkey_api_key" "storefront" {
  tenant          = "shop"
  name            = "storefront"
  browser         = true
  allowed_origins = ["https://app.acme.test", "https://*.acme.test"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("latchkey_api_key.backend", "id"),
					resource.TestMatchResourceAttr("latchkey_api_key.backend", "key", regexp.MustCompile(`^lk_live_`)),
					resource.TestMatchResourceAttr("latchkey_api_key.storefront", "key", regexp.MustCompile(`^lk_pk_live_`)),
					resource.TestCheckResourceAttr("latchkey_api_key.storefront", "browser", "true"),
					resource.TestCheckResourceAttr("latchkey_api_key.storefront", "allowed_origins.0", "https://app.acme.test"),
				),
			},
			{
				// a changed origin list REPLACES the key — replacement is
				// rotation; the plan itself must say so, and the untouched
				// sibling must stay put
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("latchkey_api_key.storefront", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("latchkey_api_key.backend", plancheck.ResourceActionNoop),
					},
				},
				Config: `
resource "latchkey_api_key" "backend" {
  tenant = "shop"
  name   = "ci deploy key"
  scopes = ["properties:read"]
}

resource "latchkey_api_key" "storefront" {
  tenant          = "shop"
  name            = "storefront"
  browser         = true
  allowed_origins = ["https://app.acme.test"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestMatchResourceAttr("latchkey_api_key.storefront", "key", regexp.MustCompile(`^lk_pk_live_`)),
					resource.TestCheckResourceAttr("latchkey_api_key.storefront", "allowed_origins.#", "1"),
				),
			},
		},
	})
}

// TestAccGithub: both GitHub singletons — sign-in in custom mode (the
// org's own client, secret write-only) then switched to platform (no
// credentials), and the App (numeric id, key must look like a PEM),
// with the org data source reporting configured-ness and never a secret.
func TestAccGithub(t *testing.T) {
	testIssuer(t)
	// HCL-escaped newlines: the config is a quoted string
	const pem = `-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END RSA PRIVATE KEY-----\n`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_github_signin" "this" {
  mode          = "custom"
  client_id     = "Iv1.acme"
  client_secret = "s3kret"
}
resource "latchkey_github_app" "this" {
  app_id      = "1901048"
  private_key = "` + pem + `"
}
data "latchkey_org" "this" {
  depends_on = [latchkey_github_signin.this, latchkey_github_app.this]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_github_signin.this", "mode", "custom"),
					resource.TestCheckResourceAttr("latchkey_github_signin.this", "client_id", "Iv1.acme"),
					resource.TestCheckResourceAttr("latchkey_github_app.this", "app_id", "1901048"),
					resource.TestCheckResourceAttr("data.latchkey_org.this", "github_signin", "custom"),
					resource.TestCheckResourceAttr("data.latchkey_org.this", "github_app_id", "1901048"),
				),
			},
			{
				// platform mode drops the credentials; the App stays
				Config: `
resource "latchkey_github_signin" "this" {
  mode = "platform"
}
resource "latchkey_github_app" "this" {
  app_id      = "1901048"
  private_key = "` + pem + `"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_github_signin.this", "mode", "platform"),
					resource.TestCheckNoResourceAttr("latchkey_github_signin.this", "client_id"),
				),
			},
			{
				// the service refuses a key that is not a PEM, at plan-apply time
				Config: `
resource "latchkey_github_app" "this" {
  app_id      = "1901048"
  private_key = "not a key"
}`,
				ExpectError: regexp.MustCompile(`private_key should be the \.pem`),
			},
		},
	})
}
