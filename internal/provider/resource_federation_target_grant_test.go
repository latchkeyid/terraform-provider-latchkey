package provider

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func tfjsonPath(name string) tfjsonpath.Path { return tfjsonpath.New(name) }

func notNull() knownvalue.Check { return knownvalue.NotNull() }

const fedGrantTarget = fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant = latchkey_tenant.shop.slug
  name   = "granted"
  cloud  = "gcp"
  gcp = {
    workload_identity_provider = "` + fedWIP + `"
  }
}
`

// the product's grant as the target lists it, nil for none
func fedGrantOf(project, product string) ([]string, error) {
	api := testAPI()
	name := "granted"
	if project != "" {
		name = "granted-project"
	}
	t, err := api.GetFederationTarget(context.Background(), api.Org, "fed-shop", name, project)
	if err != nil {
		return nil, err
	}
	if g := t.GrantFor(product); g != nil {
		return g.Purposes, nil
	}
	return nil, nil
}

// TestAccFederationTargetGrant: a product's grant is created, read from
// the target's own list, replaced in place, imported by
// org/tenant/target/product, granted again after a revoke behind
// Terraform's back, and revoked on destroy while the target stays.
func TestAccFederationTargetGrant(t *testing.T) {
	testIssuer(t)
	const res = "latchkey_federation_target_grant.runsheet"
	full := fedGrantTarget + `
resource "latchkey_federation_target_grant" "runsheet" {
  tenant       = latchkey_tenant.shop.slug
  target       = latchkey_federation_target.deployer.name
  product      = "runsheet"
  purposes     = ["deploy"]
  environments = ["production"]
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: full,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(res, "id", "acme/fed-shop/granted/runsheet"),
					resource.TestCheckResourceAttr(res, "org", "acme"),
					resource.TestCheckResourceAttr(res, "purposes.#", "1"),
					resource.TestCheckResourceAttr(res, "purposes.0", "deploy"),
					resource.TestCheckResourceAttr(res, "environments.0", "production"),
					resource.TestCheckNoResourceAttr(res, "kinds.#"),
				),
			},
			{
				// the grant endpoint replaces the product's grant in place
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("latchkey_federation_target.deployer", plancheck.ResourceActionNoop),
				}},
				Config: fedGrantTarget + `
resource "latchkey_federation_target_grant" "runsheet" {
  tenant   = latchkey_tenant.shop.slug
  target   = latchkey_federation_target.deployer.name
  product  = "runsheet"
  purposes = ["observe", "deploy", "configure"]
  kinds    = ["*"]
}

resource "latchkey_federation_target_grant" "wardroom" {
  tenant   = latchkey_tenant.shop.slug
  target   = latchkey_federation_target.deployer.name
  product  = "wardroom"
  purposes = ["observe"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(res, "purposes.#", "3"),
					resource.TestCheckTypeSetElemAttr(res, "purposes.*", "configure"),
					resource.TestCheckResourceAttr(res, "kinds.0", "*"),
					resource.TestCheckNoResourceAttr(res, "environments.#"),
					resource.TestCheckResourceAttr("latchkey_federation_target_grant.wardroom", "purposes.0", "observe"),
				),
			},
			{
				ResourceName:      res,
				ImportState:       true,
				ImportStateId:     "acme/fed-shop/granted/runsheet",
				ImportStateVerify: true,
			},
			{
				// revoked in the console: the refresh forgets the grant and
				// only that grant
				PreConfig: func() {
					api := testAPI()
					if err := api.RevokeFederationTargetGrant(context.Background(), api.Org, "fed-shop", "granted", "", "runsheet"); err != nil {
						t.Fatalf("revoking the grant behind terraform: %v", err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[res]; ok {
						return fmt.Errorf("%s is still in state after its grant was revoked", res)
					}
					if _, ok := s.RootModule().Resources["latchkey_federation_target_grant.wardroom"]; !ok {
						return fmt.Errorf("the other product's grant left state too")
					}
					return nil
				},
			},
			{
				// and the next apply grants it again; the other product's
				// grant is destroyed, which revokes it alone
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionCreate),
					plancheck.ExpectResourceAction("latchkey_federation_target_grant.wardroom", plancheck.ResourceActionDestroy),
				}},
				Config: full,
				Check: func(*terraform.State) error {
					if got, err := fedGrantOf("", "runsheet"); err != nil || !slices.Equal(got, []string{"deploy"}) {
						return fmt.Errorf("runsheet's grant after the re-apply: %v, %v", got, err)
					}
					if got, err := fedGrantOf("", "wardroom"); err != nil || got != nil {
						return fmt.Errorf("wardroom's grant survived its destroy: %v, %v", got, err)
					}
					return nil
				},
			},
			{
				// destroying the grant revokes it and leaves the target
				Config: fedGrantTarget,
				Check: func(*terraform.State) error {
					if got, err := fedGrantOf("", "runsheet"); err != nil || got != nil {
						return fmt.Errorf("runsheet's grant survived its destroy: %v, %v", got, err)
					}
					return nil
				},
			},
		},
	})
}

// TestAccFederationTargetGrantProject: a project's target takes its
// grant through the same project, and imports with the project last.
func TestAccFederationTargetGrantProject(t *testing.T) {
	testIssuer(t)
	const res = "latchkey_federation_target_grant.runsheet"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: fedTenant + `
resource "latchkey_federation_target" "deployer" {
  tenant  = latchkey_tenant.shop.slug
  name    = "granted-project"
  project = "storefront"
  cloud   = "gcp"
  gcp = {
    workload_identity_provider = "` + fedWIP + `"
  }
}

resource "latchkey_federation_target_grant" "runsheet" {
  tenant   = latchkey_tenant.shop.slug
  target   = latchkey_federation_target.deployer.name
  project  = latchkey_federation_target.deployer.project
  product  = "runsheet"
  purposes = ["deploy"]
  kinds    = ["deploy", "configure"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(res, "id", "acme/fed-shop/granted-project/runsheet/storefront"),
					resource.TestCheckResourceAttr(res, "kinds.#", "2"),
					func(*terraform.State) error {
						if got, err := fedGrantOf("storefront", "runsheet"); err != nil || !slices.Equal(got, []string{"deploy"}) {
							return fmt.Errorf("the project target's grant: %v, %v", got, err)
						}
						return nil
					},
				),
			},
			{ResourceName: res, ImportState: true, ImportStateVerify: true},
			{ResourceName: "latchkey_federation_target.deployer", ImportState: true, ImportStateId: "acme/fed-shop/granted-project/storefront", ImportStateVerify: true},
		},
	})
}
