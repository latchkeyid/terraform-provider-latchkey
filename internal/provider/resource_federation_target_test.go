package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

const (
	fedTenant = `
resource "latchkey_tenant" "shop" {
  slug         = "fed-shop"
  display_name = "Fed Shop"
}
`
	fedPool = "projects/123456789012/locations/global/workloadIdentityPools/latchkey"
	fedWIP  = fedPool + "/providers/latchkey"
)

// the target must be gone (404) once Terraform destroyed it
func fedTargetGone(tenant, name, project string) func(*terraform.State) error {
	return func(*terraform.State) error {
		api := testAPI()
		if _, err := api.GetFederationTarget(context.Background(), api.Org, tenant, name, project); !latchkey.IsNotFound(err) {
			return fmt.Errorf("federation target %s/%s still answers: %v", tenant, name, err)
		}
		return nil
	}
}

// TestAccFederationTarget: a GCP target is created with the API's
// defaults in the plan, read back with its ref and principals, updated
// in place, imported by org/tenant/name, and dropped from state once it
// is deleted behind Terraform's back.
func TestAccFederationTarget(t *testing.T) {
	testIssuer(t)
	const res = "latchkey_federation_target.deployer"
	var ref string
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(res, plancheck.ResourceActionUpdate)}}
	sameRef := func(s *terraform.State) error {
		if got := s.RootModule().Resources[res].Primary.Attributes["target_ref"]; got != ref {
			return fmt.Errorf("target_ref changed in place: %q, was %q", got, ref)
		}
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             fedTargetGone("fed-shop", "deployer", ""),
		Steps: []resource.TestStep{
			{
				// the block must be the cloud's own
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant = latchkey_tenant.shop.slug
  name   = "deployer"
  cloud  = "gcp"
  aws = {
    role_arn = "arn:aws:iam::123456789012:role/deployer"
    region   = "us-east-1"
  }
}`,
				ExpectError: regexp.MustCompile(`needs the .gcp. block|takes the .gcp. block only`),
			},
			{
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant = latchkey_tenant.shop.slug
  name   = "deployer"
  cloud  = "gcp"
  gcp = {
    workload_identity_provider = "` + fedWIP + `"
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrWith(res, "target_ref", func(v string) error {
						if ref = v; len(v) != 36 {
							return fmt.Errorf("target_ref %q is not a ref", v)
						}
						return nil
					}),
					resource.TestCheckResourceAttr(res, "org", "acme"),
					resource.TestCheckResourceAttr(res, "id", "acme/fed-shop/deployer"),
					resource.TestCheckResourceAttr(res, "label", "deployer"),
					resource.TestCheckResourceAttr(res, "description", ""),
					resource.TestCheckResourceAttr(res, "status", "active"),
					resource.TestCheckResourceAttr(res, "audience", "https://iam.googleapis.com/"+fedWIP),
					resource.TestCheckResourceAttr(res, "gcp.scopes.#", "1"),
					resource.TestCheckResourceAttr(res, "gcp.scopes.0", "https://www.googleapis.com/auth/cloud-platform"),
					resource.TestCheckNoResourceAttr(res, "gcp.service_account"),
					resource.TestCheckNoResourceAttr(res, "project"),
					resource.TestCheckResourceAttrWith(res, "principal_set", func(v string) error {
						if want := "principalSet://iam.googleapis.com/" + fedPool + "/attribute.target/" + ref; v != want {
							return fmt.Errorf("principal_set %q, want %q", v, want)
						}
						return nil
					}),
					resource.TestCheckResourceAttrWith(res, "principal", func(v string) error {
						if want := "principal://iam.googleapis.com/" + fedPool + "/subject/fed:" + ref + ":<product>:<kind|workload>"; v != want {
							return fmt.Errorf("principal %q, want %q", v, want)
						}
						return nil
					}),
					resource.TestCheckResourceAttrWith(res, "attribute_condition", func(v string) error {
						if want := fmt.Sprintf("assertion.tenant == %q && assertion.target == %q", "fed-shop", ref); os.Getenv("LATCHKEY_ORG") == "acme" && v != want {
							return fmt.Errorf("attribute_condition %q, want %q", v, want)
						}
						return nil
					}),
				),
			},
			{
				// everything but the grants is replaced in place; the ref and
				// the principals, which follow the pool, stay known in the plan
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(res, tfjsonPath("target_ref"), notNull()),
					plancheck.ExpectKnownValue(res, tfjsonPath("principal_set"), notNull()),
				}},
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant      = latchkey_tenant.shop.slug
  name        = "deployer"
  cloud       = "gcp"
  label       = "Production deployer"
  description = "Deploys the shop to its GCP project."
  status      = "disabled"
  audience    = "latchkey-shop"
  gcp = {
    workload_identity_provider = "` + fedWIP + `"
    service_account            = "deployer@shop-prod.iam.gserviceaccount.com"
    scopes = [
      "https://www.googleapis.com/auth/devstorage.read_write",
      "https://www.googleapis.com/auth/cloud-platform",
    ]
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					sameRef,
					resource.TestCheckResourceAttr(res, "label", "Production deployer"),
					resource.TestCheckResourceAttr(res, "description", "Deploys the shop to its GCP project."),
					resource.TestCheckResourceAttr(res, "status", "disabled"),
					resource.TestCheckResourceAttr(res, "audience", "latchkey-shop"),
					resource.TestCheckResourceAttr(res, "gcp.service_account", "deployer@shop-prod.iam.gserviceaccount.com"),
					resource.TestCheckResourceAttr(res, "gcp.scopes.#", "2"),
				),
			},
			{
				ResourceName:      res,
				ImportState:       true,
				ImportStateId:     "acme/fed-shop/deployer",
				ImportStateVerify: true,
			},
			{
				// dropping the optional values restores the defaults; a new
				// provider path moves the audience and the principals
				ConfigPlanChecks: update,
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant = latchkey_tenant.shop.slug
  name   = "deployer"
  cloud  = "gcp"
  gcp = {
    workload_identity_provider = "` + fedPool + `-two/providers/latchkey"
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					sameRef,
					resource.TestCheckResourceAttr(res, "label", "deployer"),
					resource.TestCheckResourceAttr(res, "description", ""),
					resource.TestCheckResourceAttr(res, "status", "active"),
					resource.TestCheckResourceAttr(res, "audience", "https://iam.googleapis.com/"+fedPool+"-two/providers/latchkey"),
					resource.TestCheckResourceAttr(res, "gcp.scopes.#", "1"),
					resource.TestCheckNoResourceAttr(res, "gcp.service_account"),
					resource.TestCheckResourceAttrWith(res, "principal_set", func(v string) error {
						if want := "principalSet://iam.googleapis.com/" + fedPool + "-two/attribute.target/" + ref; v != want {
							return fmt.Errorf("principal_set %q, want %q", v, want)
						}
						return nil
					}),
				),
			},
			{
				// a target's cloud never changes and a deleted name stays
				// taken: the plan says so instead of failing halfway
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant = latchkey_tenant.shop.slug
  name   = "deployer"
  cloud  = "aws"
  aws = {
    role_arn = "arn:aws:iam::123456789012:role/deployer"
    region   = "us-east-1"
  }
}`,
				ExpectError: regexp.MustCompile(`cloud never changes`),
			},
			{
				// deleted in the console: the next refresh forgets it
				PreConfig: func() {
					api := testAPI()
					if err := api.DeleteFederationTarget(context.Background(), api.Org, "fed-shop", "deployer", ""); err != nil {
						t.Fatalf("deleting the target behind terraform: %v", err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[res]; ok {
						return fmt.Errorf("%s is still in state after its target was deleted", res)
					}
					return nil
				},
			},
		},
	})
}

// TestAccFederationTargetDestroy: destroy deletes the target (CheckDestroy
// reads the 404), and the name stays taken — setting it again is refused
// with the reason.
func TestAccFederationTargetDestroy(t *testing.T) {
	testIssuer(t)
	target := `
resource "latchkey_federation_target" "once" {
  tenant = latchkey_tenant.shop.slug
  name   = "once"
  cloud  = "gcp"
  gcp = {
    workload_identity_provider = "` + fedWIP + `"
  }
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             fedTargetGone("fed-shop", "once", ""),
		Steps: []resource.TestStep{
			{Config: fedTenant + target},
			{
				Config: fedTenant,
				Check:  fedTargetGone("fed-shop", "once", ""),
			},
			{
				Config:      fedTenant + target,
				ExpectError: regexp.MustCompile(`name stays taken`),
			},
		},
	})
}

// TestAccFederationTargetClouds: the aws, azure and oidc blocks, each
// created with its defaults, changed in place and imported, and a
// project's target.
func TestAccFederationTargetClouds(t *testing.T) {
	testIssuer(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				// an oidc target has no default audience
				Config: fedTenant + `
resource "latchkey_federation_target" "sts" {
  tenant = latchkey_tenant.shop.slug
  name   = "vendor-sts"
  cloud  = "oidc"
  oidc = {
    hosts = ["api.vendor.test"]
  }
}`,
				ExpectError: regexp.MustCompile(`an oidc target names its audience`),
			},
			{
				Config: fedTenant + `
resource "latchkey_federation_target" "aws" {
  tenant = latchkey_tenant.shop.slug
  name   = "aws-deployer"
  cloud  = "aws"
  aws = {
    role_arn = "arn:aws:iam::123456789012:role/deployer"
    region   = "us-east-1"
  }
}

resource "latchkey_federation_target" "azure" {
  tenant = latchkey_tenant.shop.slug
  name   = "azure-deployer"
  cloud  = "azure"
  azure = {
    tenant_id = "11111111-2222-3333-4444-555555555555"
    client_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
    scope     = "https://management.azure.com/.default"
  }
}

resource "latchkey_federation_target" "sts" {
  tenant   = latchkey_tenant.shop.slug
  name     = "vendor-sts"
  cloud    = "oidc"
  audience = "https://sts.vendor.test"
  oidc = {
    token_url = "https://sts.vendor.test/token"
    hosts     = ["api.vendor.test"]
  }
}

resource "latchkey_federation_target" "direct" {
  tenant   = latchkey_tenant.shop.slug
  name     = "vendor-direct"
  project  = "storefront"
  cloud    = "oidc"
  audience = "https://api.vendor.test"
  oidc = {
    hosts = ["*.vendor.test", "api.vendor.test"]
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_federation_target.aws", "audience", "sts.amazonaws.com"),
					resource.TestCheckResourceAttr("latchkey_federation_target.aws", "aws.max_duration", "3600"),
					resource.TestCheckNoResourceAttr("latchkey_federation_target.aws", "aws.session_policy"),
					resource.TestCheckNoResourceAttr("latchkey_federation_target.aws", "principal_set"),
					resource.TestCheckNoResourceAttr("latchkey_federation_target.aws", "gcp.%"),
					resource.TestCheckResourceAttr("latchkey_federation_target.azure", "audience", "api://AzureADTokenExchange"),
					resource.TestCheckNoResourceAttr("latchkey_federation_target.azure", "azure.hosts.#"),
					resource.TestCheckResourceAttr("latchkey_federation_target.sts", "oidc.grant", "urn:ietf:params:oauth:grant-type:token-exchange"),
					resource.TestCheckResourceAttr("latchkey_federation_target.sts", "audience", "https://sts.vendor.test"),
					resource.TestCheckResourceAttr("latchkey_federation_target.direct", "oidc.grant", ""),
					resource.TestCheckNoResourceAttr("latchkey_federation_target.direct", "oidc.token_url"),
					resource.TestCheckResourceAttr("latchkey_federation_target.direct", "oidc.hosts.#", "2"),
					resource.TestCheckResourceAttr("latchkey_federation_target.direct", "id", "acme/fed-shop/vendor-direct/storefront"),
					resource.TestCheckResourceAttrSet("latchkey_federation_target.direct", "target_ref"),
				),
			},
			{ResourceName: "latchkey_federation_target.aws", ImportState: true, ImportStateVerify: true},
			{ResourceName: "latchkey_federation_target.azure", ImportState: true, ImportStateVerify: true},
			{ResourceName: "latchkey_federation_target.sts", ImportState: true, ImportStateVerify: true},
			{ResourceName: "latchkey_federation_target.direct", ImportState: true, ImportStateVerify: true},
			{
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("latchkey_federation_target.aws", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("latchkey_federation_target.azure", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("latchkey_federation_target.sts", plancheck.ResourceActionUpdate),
				}},
				Config: fedTenant + `
resource "latchkey_federation_target" "aws" {
  tenant = latchkey_tenant.shop.slug
  name   = "aws-deployer"
  cloud  = "aws"
  aws = {
    role_arn       = "arn:aws:iam::123456789012:role/deployer"
    region         = "ap-southeast-2"
    session_policy = jsonencode({ Version = "2012-10-17", Statement = [] })
    max_duration   = 900
  }
}

resource "latchkey_federation_target" "azure" {
  tenant = latchkey_tenant.shop.slug
  name   = "azure-deployer"
  cloud  = "azure"
  azure = {
    tenant_id = "11111111-2222-3333-4444-555555555555"
    client_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
    scope     = "api://shop/.default"
    hosts     = ["api.shop.test"]
  }
}

resource "latchkey_federation_target" "sts" {
  tenant   = latchkey_tenant.shop.slug
  name     = "vendor-sts"
  cloud    = "oidc"
  audience = "https://sts.vendor.test"
  oidc = {
    token_url = "https://sts.vendor.test/token"
    grant     = "urn:vendor:params:oauth:grant-type:jwt"
    hosts     = ["api.vendor.test"]
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("latchkey_federation_target.aws", "aws.region", "ap-southeast-2"),
					resource.TestCheckResourceAttr("latchkey_federation_target.aws", "aws.max_duration", "900"),
					resource.TestCheckResourceAttrSet("latchkey_federation_target.aws", "aws.session_policy"),
					resource.TestCheckResourceAttr("latchkey_federation_target.azure", "azure.hosts.0", "api.shop.test"),
					resource.TestCheckResourceAttr("latchkey_federation_target.sts", "oidc.grant", "urn:vendor:params:oauth:grant-type:jwt"),
					fedTargetGone("fed-shop", "vendor-direct", "storefront"),
				),
			},
		},
	})
}

// the target holds no secret: nothing in either schema is sensitive, and
// the identity of a target and of a grant is fixed
func TestFederationSchemas(t *testing.T) {
	for name, r := range map[string]fwresource.Resource{"target": newFederationTargetResource(), "grant": newFederationTargetGrantResource()} {
		var resp fwresource.SchemaResponse
		r.Schema(context.Background(), fwresource.SchemaRequest{}, &resp)
		if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
			t.Fatalf("%s schema: %v", name, diags)
		}
		for attr, a := range resp.Schema.Attributes {
			if a.IsSensitive() {
				t.Errorf("%s.%s is sensitive: a federation target holds no secret", name, attr)
			}
		}
		for _, attr := range []string{"org", "tenant"} {
			if _, ok := resp.Schema.Attributes[attr]; !ok {
				t.Errorf("%s has no %s attribute", name, attr)
			}
		}
	}
}
