package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// legacySandboxResource is latchkey_org_sandbox as released before named
// sandboxes: schema version 0, no arguments, POST /org/{slug}/sandbox.
// It writes the state an upgrade must read with no diff.
type legacySandboxResource struct{ api *latchkey.Client }

func (r *legacySandboxResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_sandbox"
}

func (r *legacySandboxResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = orgSandboxSchemaV0()
}

func (r *legacySandboxResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *legacySandboxResource) Create(ctx context.Context, _ resource.CreateRequest, resp *resource.CreateResponse) {
	slug, err := r.api.CreateSandbox(ctx)
	if err != nil {
		resp.Diagnostics.AddError("creating org sandbox", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, orgSandboxModelV0{ID: types.StringValue(slug), Slug: types.StringValue(slug), SandboxOf: types.StringValue(r.api.Org)})...)
}

func (r *legacySandboxResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgSandboxModelV0
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *legacySandboxResource) Update(context.Context, resource.UpdateRequest, *resource.UpdateResponse) {
}
func (r *legacySandboxResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

type legacyProvider struct{ *latchkeyProvider }

func (p legacyProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{func() resource.Resource { return &legacySandboxResource{} }}
}

// TestAccOrgSandboxLegacyState: state written before names (version 0,
// no `name`) upgrades to the sandbox named "sandbox" — no diff, no
// replacement — and is then managed (and destroyed) as a named one.
func TestAccOrgSandboxLegacyState(t *testing.T) {
	testIssuer(t)
	legacy := map[string]func() (tfprotov6.ProviderServer, error){
		"latchkey": providerserver.NewProtocol6WithError(func() provider.Provider {
			return legacyProvider{&latchkeyProvider{version: "legacy"}}
		}()),
	}
	cfg := `resource "latchkey_org_sandbox" "env" {}`
	tfresource.Test(t, tfresource.TestCase{
		CheckDestroy: checkNoSandboxes,
		Steps: []tfresource.TestStep{
			{
				ProtoV6ProviderFactories: legacy,
				Config:                   cfg,
				Check: tfresource.ComposeTestCheckFunc(
					tfresource.TestCheckResourceAttr("latchkey_org_sandbox.env", "slug", "acme-sandbox"),
					tfresource.TestCheckNoResourceAttr("latchkey_org_sandbox.env", "name"),
				),
			},
			{
				// the upgrade plans nothing
				ProtoV6ProviderFactories: factories,
				Config:                   cfg,
				PlanOnly:                 true,
			},
			{
				ProtoV6ProviderFactories: factories,
				Config:                   cfg,
				Check: tfresource.ComposeTestCheckFunc(
					tfresource.TestCheckResourceAttr("latchkey_org_sandbox.env", "name", "sandbox"),
					tfresource.TestCheckResourceAttr("latchkey_org_sandbox.env", "id", "acme-sandbox"),
					tfresource.TestCheckResourceAttr("latchkey_org_sandbox.env", "slug", "acme-sandbox"),
					tfresource.TestCheckResourceAttrSet("latchkey_org_sandbox.env", "key"),
				),
			},
		},
	})
}

// TestOrgSandboxStateUpgrader: the version 0 → 1 upgrader in isolation.
func TestOrgSandboxStateUpgrader(t *testing.T) {
	ctx := context.Background()
	r := &orgSandboxResource{}
	up := r.UpgradeState(ctx)[0]

	v0 := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"id": tftypes.String, "slug": tftypes.String, "sandbox_of": tftypes.String}}
	prior := tfsdk.State{Schema: *up.PriorSchema, Raw: tftypes.NewValue(v0, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "acme-sandbox"), "slug": tftypes.NewValue(tftypes.String, "acme-sandbox"), "sandbox_of": tftypes.NewValue(tftypes.String, "acme"),
	})}
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	cur := sr.Schema
	resp := resource.UpgradeStateResponse{State: tfsdk.State{Schema: cur, Raw: tftypes.NewValue(cur.Type().TerraformType(ctx), nil)}}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{State: &prior}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got orgSandboxModel
	resp.Diagnostics.Append(resp.State.Get(ctx, &got)...)
	if got.Name.ValueString() != "sandbox" || got.Slug.ValueString() != "acme-sandbox" || got.ID.ValueString() != "acme-sandbox" || got.SandboxOf.ValueString() != "acme" || !got.Key.IsNull() {
		t.Fatalf("upgraded state: %+v", got)
	}
}
