package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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
  html    = "<p>Click to sign in</p>"
}`,
				Check: resource.TestCheckResourceAttr("latchkey_mail_template.login", "html", "<p>Click to sign in</p>"),
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
				),
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
