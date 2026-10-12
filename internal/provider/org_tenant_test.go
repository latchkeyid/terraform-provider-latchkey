package provider

import (
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// ADR 012: latchkey_org binds the org to an anythink (house) tenant at
// creation. These drive the stub's house tenants and levers, so they
// run against the stub only.

func stubOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("LATCHKEY_ISSUER") != "" {
		t.Skip("drives the stub's house tenants and levers")
	}
	testIssuer(t)
}

// stubLever flips a stub-only switch (/stub/tenant-required, /stub/not-staff).
func stubLever(t *testing.T, name string) func() {
	return func() {
		resp, err := http.Post(os.Getenv("LATCHKEY_ISSUER")+"/stub/"+name, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
}

// TestAccOrgTenant: create bound to an existing house tenant named by
// its REF; the key reads back into tenant_key while `tenant` keeps the
// ref, so a refresh plans nothing. Another name for the same tenant (its
// slug) records the spelling without a call; a different tenant is a
// rebind, refused at plan time; import reads the binding.
func TestAccOrgTenant(t *testing.T) {
	stubOnly(t)
	cfg := func(tenant string) string {
		return `
resource "latchkey_org" "wardroom" {
  slug         = "wardroom"
  display_name = "Wardroom"
  owner_email  = "cap@wardroom.test"
  tenant       = "` + tenant + `"
}`
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: cfg("plygmjohrbtnkxnlsehy"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant", "plygmjohrbtnkxnlsehy"),
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant_key", "grapevine"),
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant_ref", "plygmjohrbtnkxnlsehy"),
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant_slug", "grapevine"),
				),
			},
			{
				// read back: the ref resolves to the bound key — no diff
				Config:   cfg("plygmjohrbtnkxnlsehy"),
				PlanOnly: true,
			},
			{
				// the slug names the same tenant: an in-place record, no
				// replace and no bind
				Config: cfg("grapevine"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("latchkey_org.wardroom", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant", "grapevine"),
					resource.TestCheckResourceAttr("latchkey_org.wardroom", "tenant_key", "grapevine"),
				),
			},
			{
				Config:   cfg("grapevine"),
				PlanOnly: true,
			},
			{
				// another tenant: a rebind, staff's — refused, never a replace
				Config:      cfg("optrader"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`rebinding an org's tenant is a staff\s+action`),
			},
			{
				Config:                  cfg("grapevine"),
				ResourceName:            "latchkey_org.wardroom",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"terraform_client_secret", "tenant"},
			},
		},
	})
}

// TestAccOrgNewTenant: new_tenant creates the house tenant first with
// the owner its admin; the binding reads back and plans nothing more.
func TestAccOrgNewTenant(t *testing.T) {
	stubOnly(t)
	cfg := `
resource "latchkey_org" "purser" {
  slug         = "purser"
  display_name = "Purser"
  owner_email  = "cap@wardroom.test"
  new_tenant = {
    display_name = "Purser Co"
  }
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr("latchkey_org.purser", "tenant"),
					resource.TestCheckResourceAttrSet("latchkey_org.purser", "tenant_key"),
					resource.TestCheckResourceAttr("latchkey_org.purser", "tenant_slug", "purser-co"),
					resource.TestCheckResourceAttr("latchkey_org.purser", "new_tenant.display_name", "Purser Co"),
				),
			},
			{Config: cfg, PlanOnly: true},
		},
	})
}

// TestAccOrgTenantExclusive: tenant and new_tenant together never reach
// the service.
func TestAccOrgTenantExclusive(t *testing.T) {
	stubOnly(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "latchkey_org" "both" {
  slug         = "both"
  display_name = "Both"
  owner_email  = "cap@wardroom.test"
  tenant       = "grapevine"
  new_tenant   = {}
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`cannot be configured together`),
			},
		},
	})
}

// TestAccOrgTenantRequired: once the bound-only invariant is on, an
// unbound create's 400 tenant_required becomes a diagnostic that says
// what to set.
func TestAccOrgTenantRequired(t *testing.T) {
	stubOnly(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				PreConfig: stubLever(t, "tenant-required"),
				Config: `
resource "latchkey_org" "loose" {
  slug         = "loose"
  display_name = "Loose"
  owner_email  = "cap@wardroom.test"
}`,
				ExpectError: regexp.MustCompile(`(?s)must be bound to an anythink tenant.*Set\s+.tenant.`),
			},
		},
	})
}

// TestAccOrgBindUnbound: an org created unbound (before the invariant)
// is bound in place when `tenant` is added — never replaced. new_tenant
// cannot be added after creation.
func TestAccOrgBindUnbound(t *testing.T) {
	stubOnly(t)
	base := `
resource "latchkey_org" "foghorn" {
  slug         = "foghorn"
  display_name = "Foghorn"
  owner_email  = "cap@wardroom.test"
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: base + "}",
				Check:  resource.TestCheckNoResourceAttr("latchkey_org.foghorn", "tenant_key"),
			},
			{
				Config:      base + "  new_tenant = {}\n}",
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`new_tenant applies only when the organization is\s+created`),
			},
			{
				Config: base + "  tenant = \"inflow\"\n}",
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("latchkey_org.foghorn", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_org.foghorn", "tenant", "inflow"),
					resource.TestCheckResourceAttr("latchkey_org.foghorn", "tenant_key", "csqhdrjodfiqlglgqoay"),
				),
			},
			{Config: base + "  tenant = \"inflow\"\n}", PlanOnly: true},
		},
	})
}

// TestAccOrgBindRefused: credentials without staff standing cannot bind
// (a person's act); the refusal says so, and refresh tolerates the 403
// on GET /org/{slug}.
func TestAccOrgBindRefused(t *testing.T) {
	stubOnly(t)
	base := `
resource "latchkey_org" "tripline" {
  slug         = "tripline"
  display_name = "Tripline"
  owner_email  = "cap@wardroom.test"
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: base + "}"},
			{
				PreConfig:   stubLever(t, "not-staff"),
				Config:      base + "  tenant = \"inflow\"\n}",
				ExpectError: regexp.MustCompile(`binding the organization to its tenant was refused`),
			},
		},
	})
}
